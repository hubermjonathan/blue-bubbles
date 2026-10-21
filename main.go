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
)

func main() {
	home, _ := os.UserHomeDir()
	configPath := flag.String("config", filepath.Join(home, ".config/blue-bubbles/config.yaml"), "config file")
	dbPath := flag.String("db", filepath.Join(home, ".local/state/blue-bubbles/state.db"), "state database")
	addr := flag.String("addr", ":8723", "listen address")
	flag.Parse()

	config, err := watchConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	st, err := openStore(*dbPath)
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

	im := newIMsg("imsg")
	g := newGateway(config, st, im, service)
	go im.run()
	go g.watch(im)
	go g.sendLoop()
	go func() {
		for ; ; time.Sleep(time.Hour) {
			if err := st.prune(time.Now().AddDate(0, 0, -30)); err != nil {
				log.Printf("prune: %v", err)
			}
		}
	}()

	log.Printf("listening on %s", *addr)
	srv := &http.Server{Addr: *addr, Handler: g.routes(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
