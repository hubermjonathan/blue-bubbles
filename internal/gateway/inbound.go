package gateway

import (
	"context"
	"encoding/json"
	"log"
	"slices"
	"strings"

	"github.com/hubermjonathan/blue-bubbles/internal/config"
	"github.com/hubermjonathan/blue-bubbles/internal/imsg"
	"github.com/hubermjonathan/blue-bubbles/internal/store"
)

type message struct {
	ID                   int64  `json:"id"`
	ChatID               int64  `json:"chat_id"`
	ChatGUID             string `json:"chat_guid"`
	ChatName             string `json:"chat_name"`
	IsGroup              bool   `json:"is_group"`
	GUID                 string `json:"guid"`
	ReplyToGUID          string `json:"reply_to_guid"`
	ThreadOriginatorGUID string `json:"thread_originator_guid"`
	Sender               string `json:"sender"`
	IsFromMe             bool   `json:"is_from_me"`
	IsReaction           bool   `json:"is_reaction"`
	Text                 string `json:"text"`
	CreatedAt            string `json:"created_at"`
	Mutation             string `json:"type"`
}

// Watch feeds every message from imsg through handle, resubscribing from the saved cursor whenever imsg starts.
func (g *Gateway) Watch(im *imsg.Client) {
	ctx := context.Background()
	subscribe := func(since int64) {
		params := map[string]any{}
		if since > 0 {
			params["since_rowid"] = since
		}
		if err := im.Call(ctx, "watch.subscribe", params, nil); err != nil {
			log.Printf("watch: %v; restarting imsg", err)
			im.Restart()
		}
	}
	for ev := range im.Events {
		switch ev.Method {
		case "started":
			rowid, err := g.store.Cursor()
			if err != nil {
				log.Fatalf("watch: %v", err)
			}
			subscribe(rowid)
		case "watch.overflow":
			var p struct {
				Resume int64 `json:"resume_after_rowid"`
			}
			json.Unmarshal(ev.Params, &p)
			subscribe(p.Resume)
		case "message":
			var p struct {
				Message message `json:"message"`
			}
			if err := json.Unmarshal(ev.Params, &p); err != nil {
				log.Printf("watch: %v", err)
				continue
			}
			if err := g.handle(ctx, p.Message); err != nil {
				log.Printf("watch: message %d: %v", p.Message.ID, err)
			}
		}
	}
}

func (g *Gateway) handle(ctx context.Context, m message) error {
	if isNew, err := g.store.NewChat(m.ChatGUID); err == nil && isNew {
		log.Printf("new chat %s %q", m.ChatGUID, m.ChatName)
	}
	ds := g.route(ctx, m)
	if err := g.store.Deliver(m.ID, ds); err != nil {
		return err
	}
	if len(ds) > 0 {
		g.wake()
	}
	return nil
}

// route returns the inbox entries m produces. Anything unrouted or unauthorized produces none.
func (g *Gateway) route(ctx context.Context, m message) []store.Delivery {
	if m.IsFromMe || m.IsReaction || m.Mutation != "" || strings.TrimSpace(m.Text) == "" {
		return nil
	}
	cfg := g.config()
	person := cfg.Person(m.Sender)
	if person == "" {
		return nil
	}
	if service, err := g.service(m.GUID); service != "iMessage" {
		if err != nil {
			log.Printf("route: service of %s: %v", m.GUID, err)
		}
		return nil
	}

	var targets []string
	via := map[string]string{}
	add := func(sender, how string) {
		if _, ok := via[sender]; !ok {
			targets = append(targets, sender)
			via[sender] = how
		}
	}
	for _, guid := range []string{m.ThreadOriginatorGUID, m.ReplyToGUID} {
		if guid == "" {
			continue
		}
		if sender, _ := g.store.Route(guid); sender != "" {
			add(sender, "reply")
			break
		}
	}
	for _, name := range cfg.Mentioned(m.Text) {
		add("agent:"+name, "mention")
	}

	var ds []store.Delivery
	for _, sender := range targets {
		kind, name, _ := strings.Cut(sender, ":")
		switch kind {
		case "agent":
			a := cfg.Agents[name]
			if a == nil || a.Members[person] == "" || (m.IsGroup && !slices.Contains(a.Groups, m.ChatGUID)) {
				continue
			}
			e := map[string]any{
				"id": m.GUID, "session": name + ":" + m.ChatGUID, "chat_guid": m.ChatGUID, "is_group": m.IsGroup,
				"person": person, "role": a.Members[person], "text": m.Text, "received_at": m.CreatedAt, "via": via[sender],
			}
			if m.IsGroup && a.History > 0 {
				e["history"] = g.history(ctx, cfg, m, a.History)
			}
			ds = append(ds, store.Delivery{Sender: sender, ID: m.GUID, Entry: e})
		case "thread":
			if person != cfg.Owner || m.IsGroup {
				continue
			}
			t, err := g.store.Thread(name)
			if err != nil || t == nil || !t.Open {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(m.Text), "/close") {
				g.closeThread(t, true)
				continue
			}
			ds = append(ds, store.Delivery{Sender: sender, ID: m.GUID, Entry: map[string]any{"id": m.GUID, "text": m.Text, "received_at": m.CreatedAt}})
		}
	}
	return ds
}

// history returns up to n messages before m in its chat, oldest first.
func (g *Gateway) history(ctx context.Context, cfg *config.Config, m message, n int) []map[string]string {
	var res struct{ Messages []message }
	if err := g.imsg.Call(ctx, "messages.history", map[string]any{"chat_id": m.ChatID, "limit": n + 1}, &res); err != nil {
		log.Printf("history: %v", err)
		return nil
	}
	out := []map[string]string{}
	for _, h := range res.Messages {
		if len(out) == n {
			break
		}
		if h.ID >= m.ID || h.Text == "" {
			continue
		}
		from := cfg.Person(h.Sender)
		if h.IsFromMe {
			from = "gateway"
		} else if from == "" {
			from = "unknown"
		}
		out = append(out, map[string]string{"from": from, "text": h.Text, "at": h.CreatedAt})
	}
	slices.Reverse(out)
	return out
}
