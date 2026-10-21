// blue-bubbles is an iMessage gateway: it routes texts between people, dedicated agents, and coding sessions.
package main

import (
	"database/sql"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/hubermjonathan/blue-bubbles/internal/config"
	"github.com/hubermjonathan/blue-bubbles/internal/gateway"
	"github.com/hubermjonathan/blue-bubbles/internal/imsg"
	"github.com/hubermjonathan/blue-bubbles/internal/store"
)

func main() {
	home, _ := os.UserHomeDir()
	configPath := flag.String("config", filepath.Join(home, ".config/blue-bubbles/config.yaml"), "config file")
	dbPath := flag.String("db", filepath.Join(home, ".local/state/blue-bubbles/state.db"), "state database")
	addr := flag.String("addr", ":8723", "listen address")
	flag.Parse()

	cfg, err := config.Watch(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	// imsg's message JSON has no service, so the SMS check reads it from Messages' database.
	chatDB, err := sql.Open("sqlite", "file:"+filepath.Join(home, "Library/Messages/chat.db")+"?mode=ro")
	if err != nil {
		log.Fatal(err)
	}
	service := func(guid string) (s string, err error) {
		err = chatDB.QueryRow(`SELECT service FROM message WHERE guid = ?`, guid).Scan(&s)
		return s, err
	}

	im := imsg.New("imsg")
	g := gateway.New(cfg, st, im, service)
	go im.Run()
	go g.Watch(im)
	go g.SendLoop()
	go func() {
		for ; ; time.Sleep(time.Hour) {
			if err := st.Prune(time.Now().AddDate(0, 0, -30)); err != nil {
				log.Printf("prune: %v", err)
			}
		}
	}()

	log.Printf("listening on %s", *addr)
	srv := &http.Server{Addr: *addr, Handler: g.Routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
