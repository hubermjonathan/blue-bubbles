package gateway

import (
	"context"
	"log"
	"strings"
	"time"
)

const maxMessageLen = 2000

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

// send queues text for the one send loop and waits for its first chunk's guid.
func (g *Gateway) send(ctx context.Context, sender, header string, to target, text string) (string, error) {
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

// SendLoop sends one chunk at a time, at least a second apart, and records which sender each guid belongs to.
func (g *Gateway) SendLoop() {
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

func (g *Gateway) sendOne(sender string, to target, text string) (string, error) {
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
	if err := g.imsg.Call(ctx, "send", params, &res); err != nil {
		return "", err
	}
	if res.GUID == "" {
		log.Printf("send for %s: imsg returned no guid, so replies to it won't route", sender)
		return "", nil
	}
	return res.GUID, g.store.AddRoute(res.GUID, sender)
}

// checkSent resends a chunk once if Messages reports that it failed.
func (g *Gateway) checkSent(guid, sender string, to target, text string) {
	if guid == "" {
		return
	}
	time.Sleep(15 * time.Second)
	var res struct {
		SendState string `json:"send_state"`
	}
	if err := g.imsg.Call(context.Background(), "message.send_status", map[string]any{"guid": guid}, &res); err != nil {
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
