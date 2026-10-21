package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS state (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS chats (guid TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS routes (guid TEXT PRIMARY KEY, sender TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS threads (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	open INTEGER NOT NULL DEFAULT 1,
	read_seq INTEGER NOT NULL DEFAULT 0,
	notified_seq INTEGER NOT NULL DEFAULT 0,
	watch_key TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS inbox (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	sender TEXT NOT NULL,
	id TEXT NOT NULL,
	entry TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	UNIQUE (sender, id)
);`

type store struct{ db *sql.DB }

type thread struct {
	ID, Name, WatchKey   string
	Open                 bool
	ReadSeq, NotifiedSeq int64
}

type delivery struct {
	sender, id string
	entry      map[string]any
}

func openStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &store{db}, nil
}

func (s *store) cursor() (rowid int64, err error) {
	err = s.db.QueryRow(`SELECT value FROM state WHERE key = 'rowid'`).Scan(&rowid)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return rowid, err
}

// deliver appends ds to their inboxes and advances the watch cursor past rowid in one transaction,
// so a crash redelivers a message at most, and a redelivered entry keeps its id and seq.
func (s *store) deliver(rowid int64, ds []delivery) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, d := range ds {
		b, err := json.Marshal(d.entry)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO inbox (sender, id, entry, created_at) VALUES (?, ?, ?, ?)`,
			d.sender, d.id, b, time.Now().Unix()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO state (key, value) VALUES ('rowid', ?)
		ON CONFLICT (key) DO UPDATE SET value = max(value, excluded.value)`, rowid); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *store) entries(sender string, after int64) ([]map[string]any, error) {
	rows, err := s.db.Query(`SELECT seq, entry FROM inbox WHERE sender = ? AND seq > ? ORDER BY seq LIMIT 100`, sender, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var seq int64
		var b []byte
		if err := rows.Scan(&seq, &b); err != nil {
			return nil, err
		}
		var e map[string]any
		if err := json.Unmarshal(b, &e); err != nil {
			return nil, err
		}
		e["seq"] = seq
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *store) lastSeq(sender string) (seq int64, err error) {
	err = s.db.QueryRow(`SELECT coalesce(max(seq), 0) FROM inbox WHERE sender = ?`, sender).Scan(&seq)
	return seq, err
}

func (s *store) prune(before time.Time) error {
	_, err := s.db.Exec(`DELETE FROM inbox WHERE created_at < ?`, before.Unix())
	return err
}

func (s *store) newChat(guid string) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO chats (guid) VALUES (?)`, guid)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *store) addRoute(guid, sender string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO routes (guid, sender) VALUES (?, ?)`, guid, sender)
	return err
}

func (s *store) route(guid string) (sender string, err error) {
	err = s.db.QueryRow(`SELECT sender FROM routes WHERE guid = ?`, guid).Scan(&sender)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return sender, err
}

// newThread opens a thread, naming it name-2, name-3, and so on when an open thread already has the name.
func (s *store) newThread(name string) (*thread, error) {
	t := &thread{ID: randomHex(), WatchKey: randomHex(), Open: true, Name: name}
	for n := 2; ; n++ {
		var taken bool
		if err := s.db.QueryRow(`SELECT count(*) > 0 FROM threads WHERE open AND name = ?`, t.Name).Scan(&taken); err != nil {
			return nil, err
		}
		if !taken {
			break
		}
		t.Name = fmt.Sprintf("%s-%d", name, n)
	}
	_, err := s.db.Exec(`INSERT INTO threads (id, name, watch_key) VALUES (?, ?, ?)`, t.ID, t.Name, t.WatchKey)
	return t, err
}

func (s *store) thread(id string) (*thread, error) {
	t := &thread{ID: id}
	err := s.db.QueryRow(`SELECT name, open, read_seq, notified_seq, watch_key FROM threads WHERE id = ?`, id).
		Scan(&t.Name, &t.Open, &t.ReadSeq, &t.NotifiedSeq, &t.WatchKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

func (s *store) closeThread(id string) (wasOpen bool, err error) {
	res, err := s.db.Exec(`UPDATE threads SET open = 0 WHERE id = ? AND open`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// markRead also marks the replies notified, so a watcher doesn't announce replies already read.
func (s *store) markRead(id string, seq int64) error {
	_, err := s.db.Exec(`UPDATE threads SET read_seq = max(read_seq, ?1), notified_seq = max(notified_seq, ?1) WHERE id = ?2`, seq, id)
	return err
}

func (s *store) markNotified(id string, seq int64) error {
	_, err := s.db.Exec(`UPDATE threads SET notified_seq = max(notified_seq, ?) WHERE id = ?`, seq, id)
	return err
}

func randomHex() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
