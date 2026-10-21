package main

import (
	"context"
	"encoding/json"
	"log"
	"slices"
	"strings"
	"sync"
	"time"
)

const maxMessageLen = 2000

type caller interface {
	call(ctx context.Context, method string, params, out any) error
}

type gateway struct {
	config  func() *Config
	store   *store
	imsg    caller
	service func(messageGUID string) (string, error)
	outbox  chan *outgoing

	mu       sync.Mutex
	changed  chan struct{}
	watchers map[string]watcher
}

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
}

// target is either a chat or a person's phone number.
type target struct{ chatGUID, phone string }

type outgoing struct {
	sender string
	to     target
	chunks []string
	retry  bool
	done   chan sendResult
}

type sendResult struct {
	guid string
	err  error
}

func newGateway(config func() *Config, st *store, im caller, service func(string) (string, error)) *gateway {
	return &gateway{
		config:   config,
		store:    st,
		imsg:     im,
		service:  service,
		outbox:   make(chan *outgoing, 64),
		changed:  make(chan struct{}),
		watchers: map[string]watcher{},
	}
}

// changes returns a channel that closes on the next inbox, thread, or watcher change.
func (g *gateway) changes() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.changed
}

func (g *gateway) wake() {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.changed)
	g.changed = make(chan struct{})
}

// waitFor calls ready until it reports true, the wait passes, or ctx ends.
func (g *gateway) waitFor(ctx context.Context, wait time.Duration, ready func() (bool, error)) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		changed := g.changes()
		if ok, err := ready(); ok || err != nil {
			return err
		}
		select {
		case <-changed:
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// watch feeds every message from imsg through handle, resubscribing from the saved cursor whenever imsg starts.
func (g *gateway) watch(im *imsg) {
	ctx := context.Background()
	subscribe := func(since int64) {
		params := map[string]any{}
		if since > 0 {
			params["since_rowid"] = since
		}
		if err := im.call(ctx, "watch.subscribe", params, nil); err != nil {
			log.Printf("watch: %v; restarting imsg", err)
			im.restart()
		}
	}
	for ev := range im.events {
		switch ev.Method {
		case "started":
			rowid, err := g.store.cursor()
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

func (g *gateway) handle(ctx context.Context, m message) error {
	if isNew, err := g.store.newChat(m.ChatGUID); err == nil && isNew {
		log.Printf("new chat %s %q", m.ChatGUID, m.ChatName)
	}
	ds := g.route(ctx, m)
	if err := g.store.deliver(m.ID, ds); err != nil {
		return err
	}
	if len(ds) > 0 {
		g.wake()
	}
	return nil
}

// route returns the inbox entries m produces. Anything unrouted or unauthorized produces none.
func (g *gateway) route(ctx context.Context, m message) []delivery {
	if m.IsFromMe || m.IsReaction || strings.TrimSpace(m.Text) == "" {
		return nil
	}
	cfg := g.config()
	person := cfg.person(m.Sender)
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
		if sender, _ := g.store.route(guid); sender != "" {
			add(sender, "reply")
			break
		}
	}
	for _, name := range cfg.mentioned(m.Text) {
		add("agent:"+name, "mention")
	}

	var ds []delivery
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
			ds = append(ds, delivery{sender, m.GUID, e})
		case "thread":
			if person != cfg.Owner || m.IsGroup {
				continue
			}
			t, err := g.store.thread(name)
			if err != nil || t == nil || !t.Open {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(m.Text), "/close") {
				g.closeThread(t, true)
				continue
			}
			ds = append(ds, delivery{sender, m.GUID, map[string]any{"id": m.GUID, "text": m.Text, "received_at": m.CreatedAt}})
		}
	}
	return ds
}

// history returns up to n messages before m in its chat, oldest first.
func (g *gateway) history(ctx context.Context, cfg *Config, m message, n int) []map[string]string {
	var res struct{ Messages []message }
	if err := g.imsg.call(ctx, "messages.history", map[string]any{"chat_id": m.ChatID, "limit": n + 1}, &res); err != nil {
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
		from := cfg.person(h.Sender)
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

func (g *gateway) closeThread(t *thread, notify bool) error {
	wasOpen, err := g.store.closeThread(t.ID)
	if err != nil || !wasOpen {
		return err
	}
	g.wake()
	if notify {
		cfg := g.config()
		go g.send(context.Background(), "thread:"+t.ID, threadHeader(t.Name), target{phone: cfg.People[cfg.Owner]}, "closed")
	}
	return nil
}

// send queues text for the one send loop and waits for its first chunk's guid.
func (g *gateway) send(ctx context.Context, sender, header string, to target, text string) (string, error) {
	o := &outgoing{sender: sender, to: to, retry: true, done: make(chan sendResult, 1)}
	for _, c := range split(text, maxMessageLen-len([]rune(header))) {
		o.chunks = append(o.chunks, header+c)
	}
	g.outbox <- o
	select {
	case r := <-o.done:
		return r.guid, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// sendLoop sends one chunk at a time, at least a second apart, and records which sender each guid belongs to.
func (g *gateway) sendLoop() {
	var last time.Time
	for o := range g.outbox {
		var res sendResult
		for _, text := range o.chunks {
			time.Sleep(time.Until(last.Add(time.Second)))
			guid, err := g.sendOne(o.sender, o.to, text)
			last = time.Now()
			if err != nil {
				log.Printf("send for %s: %v", o.sender, err)
				res.err = err
				break
			}
			if res.guid == "" {
				res.guid = guid
			}
			if o.retry {
				go g.checkSent(guid, o.sender, o.to, text)
			}
		}
		o.done <- res
	}
}

func (g *gateway) sendOne(sender string, to target, text string) (string, error) {
	params := map[string]any{"text": text}
	if to.chatGUID != "" {
		params["chat_guid"] = to.chatGUID
	} else {
		params["to"] = to.phone
		params["service"] = "imessage"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var res struct{ GUID string }
	if err := g.imsg.call(ctx, "send", params, &res); err != nil {
		return "", err
	}
	if res.GUID == "" {
		log.Printf("send for %s: imsg returned no guid, so replies to it won't route", sender)
		return "", nil
	}
	return res.GUID, g.store.addRoute(res.GUID, sender)
}

// checkSent resends a chunk once if Messages reports that it failed.
func (g *gateway) checkSent(guid, sender string, to target, text string) {
	if guid == "" {
		return
	}
	time.Sleep(15 * time.Second)
	var res struct {
		SendState string `json:"send_state"`
	}
	if err := g.imsg.call(context.Background(), "message.send_status", map[string]any{"guid": guid}, &res); err != nil {
		log.Printf("send status of %s: %v", guid, err)
		return
	}
	if res.SendState == "failed" {
		log.Printf("send for %s: %s failed; sending it again once", sender, guid)
		g.outbox <- &outgoing{sender: sender, to: to, chunks: []string{text}, done: make(chan sendResult, 1)}
	}
}

func agentHeader(name string) string  { return "🤖 " + bold(name) + "\n\n" }
func threadHeader(name string) string { return "🧵 " + bold(name) + "\n\n" }

// bold maps letters and digits to Unicode Mathematical Sans-Serif Bold, since iMessage has no bold text.
func bold(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z':
			return 0x1D5D4 + r - 'A'
		case r >= 'a' && r <= 'z':
			return 0x1D5EE + r - 'a'
		case r >= '0' && r <= '9':
			return 0x1D7EC + r - '0'
		}
		return r
	}, s)
}

// split cuts text into chunks of at most max runes, at a line break or space when one is near the end.
func split(text string, max int) []string {
	r := []rune(text)
	var out []string
	for len(r) > max {
		cut := max
		for i := max; i > max*3/4; i-- {
			if r[i] == '\n' || r[i] == ' ' {
				cut = i
				break
			}
		}
		out = append(out, string(r[:cut]))
		r = []rune(strings.TrimLeft(string(r[cut:]), " \n"))
	}
	return append(out, string(r))
}
