package gateway

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hubermjonathan/blue-bubbles/internal/config"
)

// watcher identifies one run of a watch_command: when it started, and a random id it picked.
type watcher struct {
	start int64
	id    string
}

func (w watcher) after(o watcher) bool {
	return w.start > o.start || (w.start == o.start && w.id > o.id)
}

func (g *Gateway) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", g.agentOnly(g.handleSend))
	mux.HandleFunc("GET /inbox", g.agentOnly(g.handleInbox))
	mux.HandleFunc("GET /watch/{id}", g.handleWatch)
	mux.Handle("/mcp", g.mcpHandler())
	return mux
}

func bearer(r *http.Request) string {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token
}

func tokenEqual(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (g *Gateway) agentOnly(h func(http.ResponseWriter, *http.Request, *config.Config, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := g.config()
		for name, a := range cfg.Agents {
			if tokenEqual(bearer(r), a.Token) {
				h(w, r, cfg, name)
				return
			}
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

func (g *Gateway) handleSend(w http.ResponseWriter, r *http.Request, cfg *config.Config, name string) {
	var req struct {
		To struct {
			ChatGUID string `json:"chat_guid"`
			Person   string `json:"person"`
		} `json:"to"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" ||
		(req.To.ChatGUID == "") == (req.To.Person == "") {
		http.Error(w, `send {"to": {"chat_guid": ...} or {"person": ...}, "text": ...}`, http.StatusBadRequest)
		return
	}
	a := cfg.Agents[name]
	var to target
	var allowed bool
	if req.To.Person != "" {
		to = target{phone: cfg.People[req.To.Person]}
		_, allowed = a.Members[req.To.Person]
	} else {
		to = target{chatGUID: req.To.ChatGUID}
		allowed = slices.Contains(a.Groups, to.chatGUID) || cfg.MemberDM(a, to.chatGUID)
	}
	if !allowed {
		http.Error(w, "not a member's direct chat or one of this agent's groups", http.StatusForbidden)
		return
	}
	guid, err := g.send(r.Context(), "agent:"+name, agentHeader(name), to, req.Text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]string{"message_guid": guid})
}

func (g *Gateway) handleInbox(w http.ResponseWriter, r *http.Request, cfg *config.Config, name string) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	var entries []map[string]any
	err := g.waitFor(r.Context(), waitParam(r), func() (bool, error) {
		var err error
		entries, err = g.store.Entries("agent:"+name, after)
		return len(entries) > 0, err
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"entries": entries})
}

func (g *Gateway) handleWatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	q := r.URL.Query()
	t, err := g.store.Thread(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if t == nil || !tokenEqual(q.Get("key"), t.WatchKey) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	me := watcher{id: q.Get("w")}
	me.start, _ = strconv.ParseInt(q.Get("t"), 10, 64)
	if time.Since(time.Unix(me.start, 0)) > 24*time.Hour {
		w.WriteHeader(http.StatusGone)
		return
	}
	g.mu.Lock()
	cur, ok := g.watchers[id]
	tookOver := !ok || me.after(cur)
	if tookOver {
		g.watchers[id] = me
	}
	g.mu.Unlock()
	if tookOver {
		g.wake()
	}

	status := http.StatusNoContent
	err = g.waitFor(r.Context(), waitParam(r), func() (bool, error) {
		g.mu.Lock()
		current := g.watchers[id] == me
		g.mu.Unlock()
		t, err := g.store.Thread(id)
		if err != nil {
			return false, err
		}
		last, err := g.store.LastSeq("thread:" + id)
		switch {
		case err != nil:
		case !t.Open:
			status = http.StatusGone
		case !current:
			status = http.StatusConflict
		case last > t.NotifiedSeq:
			status = http.StatusOK
			err = g.store.MarkNotified(id, last)
		}
		return status != http.StatusNoContent, err
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	if status == http.StatusOK {
		fmt.Fprintf(w, "user replied — call check_replies(thread_id=%s)", id)
	}
}

func waitParam(r *http.Request) time.Duration {
	s, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	return time.Duration(min(max(s, 0), 50)) * time.Second
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
