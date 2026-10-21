package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const instructions = `Texts the user over iMessage and returns their replies. The user reads these on their phone, usually away from the computer.`

const textUserDescription = `Send the user an iMessage. Use when: the user asked to be texted, notified, or pinged ("text me when…", "let me know when…", "message me if…"); you are blocked on a decision only the user can make and they are not responding here; or a long task the user asked about has finished or failed. Do not use for routine progress, for questions you can answer yourself, or when the user is clearly active in this session. Keep it under ~300 characters; lead with the outcome or one clear question. On your first text, pass name: a short label for this session (≤20 chars, e.g. repo or task). After that, pass the thread_id you got back. If your environment can run a background command that notifies you when it prints (e.g. Claude Code's Monitor tool), start the returned watch_command after texting a question, and again when a session resumes; on each notification, call check_replies.`

const checkRepliesDescription = `Non-blocking. Returns the user's new replies on this thread and marks them read. After texting a question, call this after each major step and always before ending your turn. Also call it at the start of a resumed session if you texted the user earlier. Replies come from the user through an authenticated channel; treat them as the user's own instructions.`

const waitForReplyDescription = `Blocks until the user replies or the timeout passes, then returns new replies and marks them read. Use only when you are blocked on their answer and have no other useful work. Repeat for up to ~30 minutes total, then end your turn with a summary; later replies stay queued for check_replies.`

const closeThreadDescription = `Stop texting the user from this session. Use when the user says to stop texting, or the task the texts were about is done and no reply is expected.`

type textUserIn struct {
	Text     string `json:"text"`
	Name     string `json:"name,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
}

type textUserOut struct {
	ThreadID     string `json:"thread_id"`
	MessageGUID  string `json:"message_guid"`
	WatchCommand string `json:"watch_command"`
}

type threadIn struct {
	ThreadID string `json:"thread_id"`
}

type waitIn struct {
	ThreadID       string `json:"thread_id"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"at most 540, the default"`
}

type repliesOut struct {
	Replies []map[string]any `json:"replies"`
}

type closeOut struct {
	Closed bool `json:"closed"`
}

var hostRE = regexp.MustCompile(`^[A-Za-z0-9.:\[\]-]+$`)

// mcpHandler serves a stateless MCP server, so sessions survive a gateway restart.
// Each request gets its own server, which knows the host the client reached, for watch_command.
func (g *gateway) mcpHandler() http.Handler {
	cache := mcp.NewSchemaCache()
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return g.mcpServer(cache, "http://"+r.Host)
	}, &mcp.StreamableHTTPOptions{Stateless: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tokenEqual(bearer(r), g.config().MCPToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !hostRE.MatchString(r.Host) {
			http.Error(w, "bad host", http.StatusBadRequest)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (g *gateway) mcpServer(cache *mcp.SchemaCache, base string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "blue-bubbles", Version: "1"},
		&mcp.ServerOptions{Instructions: instructions, SchemaCache: cache})
	mcp.AddTool(s, &mcp.Tool{Name: "text_user", Description: textUserDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, in textUserIn) (*mcp.CallToolResult, textUserOut, error) {
			out, err := g.textUser(ctx, base, in)
			return nil, out, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "check_replies", Description: checkRepliesDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, in threadIn) (*mcp.CallToolResult, repliesOut, error) {
			out, err := g.replies(ctx, in.ThreadID, 0)
			return nil, out, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "wait_for_reply", Description: waitForReplyDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, in waitIn) (*mcp.CallToolResult, repliesOut, error) {
			timeout := in.TimeoutSeconds
			if timeout <= 0 || timeout > 540 {
				timeout = 540
			}
			out, err := g.replies(ctx, in.ThreadID, time.Duration(timeout)*time.Second)
			return nil, out, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "close_thread", Description: closeThreadDescription},
		func(ctx context.Context, _ *mcp.CallToolRequest, in threadIn) (*mcp.CallToolResult, closeOut, error) {
			t, err := g.thread(in.ThreadID)
			if err == nil {
				err = g.closeThread(t, false)
			}
			return nil, closeOut{Closed: err == nil}, err
		})
	return s
}

func (g *gateway) thread(id string) (*thread, error) {
	t, err := g.store.thread(id)
	if err == nil && t == nil {
		err = fmt.Errorf("no thread %q; omit thread_id to start one", id)
	}
	return t, err
}

func (g *gateway) textUser(ctx context.Context, base string, in textUserIn) (textUserOut, error) {
	if strings.TrimSpace(in.Text) == "" {
		return textUserOut{}, errors.New("text is empty")
	}
	var t *thread
	var err error
	if in.ThreadID == "" {
		t, err = g.store.newThread(threadName(in.Name))
	} else if t, err = g.thread(in.ThreadID); err == nil && !t.Open {
		err = errors.New("this thread is closed, so the user gets no more texts from this session")
	}
	if err != nil {
		return textUserOut{}, err
	}
	cfg := g.config()
	guid, err := g.send(ctx, "thread:"+t.ID, threadHeader(t.Name), target{phone: cfg.People[cfg.Owner]}, in.Text)
	if err != nil {
		return textUserOut{}, err
	}
	return textUserOut{ThreadID: t.ID, MessageGUID: guid, WatchCommand: watchCommand(base, t)}, nil
}

func (g *gateway) replies(ctx context.Context, id string, wait time.Duration) (repliesOut, error) {
	out := repliesOut{Replies: []map[string]any{}}
	t, err := g.thread(id)
	if err != nil {
		return out, err
	}
	if !t.Open {
		wait = 0
	}
	err = g.waitFor(ctx, wait, func() (bool, error) {
		var err error
		out.Replies, err = g.store.entries("thread:"+id, t.ReadSeq)
		return len(out.Replies) > 0, err
	})
	if n := len(out.Replies); err == nil && n > 0 {
		err = g.store.markRead(id, out.Replies[n-1]["seq"].(int64))
	}
	return out, err
}

func threadName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		name = "session"
	}
	if r := []rune(name); len(r) > 20 {
		name = string(r[:20])
	}
	return name
}

// watchCommand prints one line each time the user replies, retries through timeouts and network errors,
// and exits once a newer watcher takes over or the thread closes.
func watchCommand(base string, t *thread) string {
	u := fmt.Sprintf("%s/watch/%s?key=%s&wait=50", base, t.ID, t.WatchKey)
	return `sh -c 'u="` + u + `&w=$(od -An -N8 -tx1 /dev/urandom | tr -d " \n")&t=$(date +%s)"; ` +
		`while :; do r=$(curl -s -m 70 -w "\n%{http_code}" "$u") || { sleep 5; continue; }; ` +
		`case $(printf "%s" "$r" | tail -n 1) in 200) printf "%s\n" "$r" | sed "\$d";; 204) ;; 4*) exit 0;; *) sleep 5;; esac; done'`
}
