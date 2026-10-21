package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	jon = "+15550000001"
	jac = "+15550000002"
	sam = "+15550000003"

	family = "any;+;family"
	other  = "any;+;other"
)

const testConfig = `
owner: jon
people:
  jon: "+15550000001"
  jac: "+15550000002"
  sam: "+15550000003"
agents:
  budget:
    token: budget-token
    members: { jon: write, jac: read }
    groups: ["any;+;family"]
    history: 2
  garden:
    token: garden-token
    members: { jon: write, jac: write }
    groups: ["any;+;family"]
mcp_token: mcp-token
`

type fakeIMsg struct {
	mu      sync.Mutex
	sent    []map[string]any
	history []message
}

func (f *fakeIMsg) call(_ context.Context, method string, params, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var res any
	switch method {
	case "send":
		f.sent = append(f.sent, params.(map[string]any))
		res = map[string]any{"ok": true, "guid": fmt.Sprintf("out-%d", len(f.sent))}
	case "messages.history":
		res = map[string]any{"messages": f.history}
	case "message.send_status":
		res = map[string]any{"send_state": "delivered"}
	}
	b, _ := json.Marshal(res)
	return json.Unmarshal(b, out)
}

func (f *fakeIMsg) last() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[len(f.sent)-1]
}

type env struct {
	t     *testing.T
	g     *gateway
	f     *fakeIMsg
	srv   *httptest.Server
	rowid int64
}

func newEnv(t *testing.T) *env {
	cfg, err := parseConfig([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	st, err := openStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIMsg{}
	service := func(guid string) (string, error) {
		if strings.HasPrefix(guid, "sms-") {
			return "SMS", nil
		}
		return "iMessage", nil
	}
	g := newGateway(func() *Config { return cfg }, st, f, service)
	go g.sendLoop()
	srv := httptest.NewServer(g.routes())
	t.Cleanup(srv.Close)
	return &env{t: t, g: g, f: f, srv: srv}
}

// text delivers an inbound message from a phone, in that person's direct chat when chat is "",
// as an inline reply when replyTo is set, and returns its guid.
func (e *env) text(from, chat, text, replyTo string) string {
	e.rowid++
	m := message{ID: e.rowid, GUID: fmt.Sprintf("in-%d", e.rowid), Sender: from, Text: text, ThreadOriginatorGUID: replyTo,
		ChatGUID: chat, IsGroup: chat != "", CreatedAt: "2026-10-21T00:00:00Z"}
	if chat == "" {
		m.ChatGUID = "any;-;" + from
	}
	e.handle(m)
	return m.GUID
}

func (e *env) handle(m message) {
	if err := e.g.handle(context.Background(), m); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) request(method, path, token, body string) (int, map[string]any) {
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *env) inbox(agent string) []any {
	code, out := e.request("GET", "/inbox?after=0", agent+"-token", "")
	if code != 200 {
		e.t.Fatalf("inbox: %d", code)
	}
	return out["entries"].([]any)
}

func (e *env) agentSend(agent, to, text string) (int, map[string]any) {
	return e.request("POST", "/send", agent+"-token", fmt.Sprintf(`{"to": %s, "text": %q}`, to, text))
}

func (e *env) mcp() *mcp.ClientSession {
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: e.srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport("mcp-token")},
	}, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { s.Close() })
	return s
}

// tool calls an MCP tool and returns its structured result, or its error text under "error".
func (e *env) tool(s *mcp.ClientSession, name string, args map[string]any) map[string]any {
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		e.t.Fatal(err)
	}
	if res.IsError {
		return map[string]any{"error": res.Content[0].(*mcp.TextContent).Text}
	}
	b, _ := json.Marshal(res.StructuredContent)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}

// watch runs a watch_command and returns its output lines, closed when it exits.
func (e *env) watch(command string) <-chan string {
	cmd := exec.Command("sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	lines := make(chan string, 10)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			lines <- sc.Text()
		}
		cmd.Wait()
		close(lines)
	}()
	return lines
}

type bearerTransport string

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

func receive(t *testing.T, lines <-chan string) (string, bool) {
	t.Helper()
	select {
	case l, ok := <-lines:
		return l, ok
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the watcher")
		return "", false
	}
}

func texts(entries []any) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.(map[string]any)["text"].(string))
	}
	return out
}

func equal(t *testing.T, got, want any) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAgentAnswersInDM(t *testing.T) {
	e := newEnv(t)
	e.text(jon, "", "@budget balance?", "")

	got := e.inbox("budget")
	equal(t, len(got), 1)
	entry := got[0].(map[string]any)
	equal(t, entry["session"], "budget:any;-;"+jon)
	equal(t, entry["person"], "jon")
	equal(t, entry["role"], "write")
	equal(t, entry["via"], "mention")
	equal(t, entry["is_group"], false)
	equal(t, len(e.inbox("garden")), 0)

	code, out := e.agentSend("budget", `{"chat_guid": "any;-;`+jon+`"}`, "Groceries: $412 of $600")
	equal(t, code, 200)
	equal(t, out["message_guid"], "out-1")
	equal(t, e.f.last()["text"], "🤖 𝗯𝘂𝗱𝗴𝗲𝘁\nGroceries: $412 of $600")
	equal(t, e.f.last()["chat_guid"], "any;-;"+jon)

	e.agentSend("budget", `{"person": "jac"}`, "hi")
	equal(t, e.f.last()["to"], jac)
	equal(t, e.f.last()["service"], "imessage")
}

func TestInlineReplyReachesTheAgent(t *testing.T) {
	e := newEnv(t)
	_, out := e.agentSend("budget", `{"person": "jon"}`, "Move $50 to savings?")
	e.text(jon, "", "yes", out["message_guid"].(string))
	e.text(jon, "", "yes", "")

	got := e.inbox("budget")
	equal(t, texts(got), []string{"yes"})
	equal(t, got[0].(map[string]any)["via"], "reply")
}

func TestGroupRouting(t *testing.T) {
	e := newEnv(t)
	e.text(jon, family, "@garden water today?", "")
	e.text(jac, family, "@budget @garden hi both", "")
	_, out := e.agentSend("garden", `{"chat_guid": "`+family+`"}`, "Rain tomorrow")
	e.text(jac, family, "nice", out["message_guid"].(string))
	e.text(jon, family, "dinner at 6", "")

	equal(t, texts(e.inbox("garden")), []string{"@garden water today?", "@budget @garden hi both", "nice"})
	equal(t, texts(e.inbox("budget")), []string{"@budget @garden hi both"})
}

func TestGroupNeedsAMemberAndAConfiguredGroup(t *testing.T) {
	e := newEnv(t)
	e.f.history = []message{
		{ID: 4, Sender: jac, Text: "@budget total?"},
		{ID: 3, IsFromMe: true, Text: "🤖 𝗴𝗮𝗿𝗱𝗲𝗻\nRain tomorrow"},
		{ID: 2, Sender: sam, Text: "pizza?"},
	}
	e.text(sam, family, "@budget hi", "")
	e.text(jon, other, "@budget hi", "")
	e.text(sam, family, "pizza?", "")
	e.text(jac, family, "@budget total?", "")

	got := e.inbox("budget")
	equal(t, texts(got), []string{"@budget total?"})
	entry := got[0].(map[string]any)
	equal(t, entry["role"], "read")
	equal(t, entry["is_group"], true)
	equal(t, entry["history"], []any{
		map[string]any{"from": "sam", "text": "pizza?", "at": ""},
		map[string]any{"from": "gateway", "text": "🤖 𝗴𝗮𝗿𝗱𝗲𝗻\nRain tomorrow", "at": ""},
	})
}

func TestInboxWaitsAndKeepsOrder(t *testing.T) {
	e := newEnv(t)
	done := make(chan []any)
	go func() {
		_, out := e.request("GET", "/inbox?after=0&wait=10", "budget-token", "")
		done <- out["entries"].([]any)
	}()
	time.Sleep(200 * time.Millisecond)
	e.text(jon, "", "@budget one", "")
	equal(t, texts(<-done), []string{"@budget one"})

	e.text(jon, "", "@budget two", "")
	e.text(jon, "", "@budget three", "")
	got := e.inbox("budget")
	equal(t, texts(got), []string{"@budget one", "@budget two", "@budget three"})
	after := int64(got[0].(map[string]any)["seq"].(float64))
	_, out := e.request("GET", fmt.Sprintf("/inbox?after=%d", after), "budget-token", "")
	equal(t, texts(out["entries"].([]any)), []string{"@budget two", "@budget three"})
}

func TestThreadRepliesAndWatcher(t *testing.T) {
	e := newEnv(t)
	s := e.mcp()
	sent := e.tool(s, "text_user", map[string]any{"text": "Tests pass. Deploy?", "name": "deploy"})
	equal(t, e.f.last()["text"], "🧵 𝗱𝗲𝗽𝗹𝗼𝘆\nTests pass. Deploy?")
	equal(t, e.f.last()["to"], jon)
	id := sent["thread_id"].(string)

	lines := e.watch(sent["watch_command"].(string))
	time.Sleep(300 * time.Millisecond)
	e.text(jon, "", "yes", sent["message_guid"].(string))
	line, _ := receive(t, lines)
	equal(t, line, "user replied — call check_replies(thread_id="+id+")")

	e.text(jon, "", "and tag it", sent["message_guid"].(string))
	e.text(jon, "", "not a reply", "")
	e.text(jac, "", "jac is not the owner", sent["message_guid"].(string))
	got := e.tool(s, "check_replies", map[string]any{"thread_id": id})
	equal(t, texts(got["replies"].([]any)), []string{"yes", "and tag it"})
	equal(t, len(e.tool(s, "check_replies", map[string]any{"thread_id": id})["replies"].([]any)), 0)

	again := e.tool(s, "text_user", map[string]any{"text": "Deployed", "thread_id": id})
	equal(t, again["thread_id"], id)
	other := e.tool(s, "text_user", map[string]any{"text": "hi", "name": "deploy"})
	equal(t, e.f.last()["text"], "🧵 𝗱𝗲𝗽𝗹𝗼𝘆-𝟮\nhi")
	if other["thread_id"] == id {
		t.Fatal("a second session reused the first thread")
	}
}

func TestWaitForReply(t *testing.T) {
	e := newEnv(t)
	s := e.mcp()
	sent := e.tool(s, "text_user", map[string]any{"text": "Which branch?", "name": "codex"})
	done := make(chan map[string]any)
	go func() {
		done <- e.tool(s, "wait_for_reply", map[string]any{"thread_id": sent["thread_id"], "timeout_seconds": 10})
	}()
	time.Sleep(300 * time.Millisecond)
	e.text(jon, "", "main", sent["message_guid"].(string))
	equal(t, texts((<-done)["replies"].([]any)), []string{"main"})
}

func TestCloseByReply(t *testing.T) {
	e := newEnv(t)
	s := e.mcp()
	sent := e.tool(s, "text_user", map[string]any{"text": "Done", "name": "deploy"})
	lines := e.watch(sent["watch_command"].(string))
	time.Sleep(300 * time.Millisecond)

	e.text(jon, "", "/close", sent["message_guid"].(string))
	if _, ok := receive(t, lines); ok {
		t.Fatal("the watcher printed instead of exiting")
	}
	for deadline := time.Now().Add(3 * time.Second); e.f.last()["text"] != "🧵 𝗱𝗲𝗽𝗹𝗼𝘆\nclosed"; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("got %q, want the closed notice", e.f.last()["text"])
		}
	}

	e.text(jon, "", "one more thing", sent["message_guid"].(string))
	equal(t, len(e.tool(s, "check_replies", map[string]any{"thread_id": sent["thread_id"]})["replies"].([]any)), 0)
	if e.tool(s, "text_user", map[string]any{"text": "hi", "thread_id": sent["thread_id"]})["error"] == nil {
		t.Fatal("text_user sent on a closed thread")
	}
}

func TestNewerWatcherTakesOver(t *testing.T) {
	e := newEnv(t)
	s := e.mcp()
	sent := e.tool(s, "text_user", map[string]any{"text": "Deploy?", "name": "deploy"})
	first := e.watch(sent["watch_command"].(string))
	time.Sleep(1100 * time.Millisecond)
	second := e.watch(sent["watch_command"].(string))
	if _, ok := receive(t, first); ok {
		t.Fatal("the first watcher printed instead of exiting")
	}
	e.tool(s, "close_thread", map[string]any{"thread_id": sent["thread_id"]})
	if _, ok := receive(t, second); ok {
		t.Fatal("the second watcher printed instead of exiting")
	}
}

func TestRejects(t *testing.T) {
	e := newEnv(t)
	s := e.mcp()
	a := e.tool(s, "text_user", map[string]any{"text": "a", "name": "a"})
	b := e.tool(s, "text_user", map[string]any{"text": "b", "name": "b"})

	code, _ := e.request("POST", "/mcp", "wrong", `{}`)
	equal(t, code, 401)
	code, _ = e.request("GET", "/inbox", "wrong", "")
	equal(t, code, 401)
	keyB := strings.Split(strings.Split(b["watch_command"].(string), "key=")[1], "&")[0]
	code, _ = e.request("GET", "/watch/"+a["thread_id"].(string)+"?key="+keyB, "", "")
	equal(t, code, 403)
	code, _ = e.agentSend("budget", `{"person": "sam"}`, "hi")
	equal(t, code, 403)
	code, _ = e.agentSend("budget", `{"chat_guid": "any;-;`+sam+`"}`, "hi")
	equal(t, code, 403)
	code, _ = e.agentSend("budget", `{"chat_guid": "`+other+`"}`, "hi")
	equal(t, code, 403)
}

func TestDrops(t *testing.T) {
	e := newEnv(t)
	e.handle(message{ID: 1, GUID: "sms-1", Sender: jon, ChatGUID: "any;-;" + jon, Text: "@budget via SMS"})
	e.handle(message{ID: 2, GUID: "in-2", Sender: "jon@icloud.com", ChatGUID: "any;-;jon@icloud.com", Text: "@budget via email"})
	e.handle(message{ID: 3, GUID: "in-3", Sender: jon, ChatGUID: "any;-;" + jon, Text: "@budget from me", IsFromMe: true})
	e.handle(message{ID: 4, GUID: "in-4", Sender: jon, ChatGUID: "any;-;" + jon, Text: " "})
	e.handle(message{ID: 5, GUID: "in-5", Sender: jon, ChatGUID: "any;-;" + jon, Text: "mail me@budget.com"})
	e.handle(message{ID: 6, GUID: "in-6", Sender: jon, ChatGUID: "any;-;" + jon, Text: "Loved “@budget hi”", IsReaction: true})
	equal(t, len(e.inbox("budget")), 0)
	rowid, _ := e.g.store.cursor()
	equal(t, rowid, 6)
}

func TestRedeliveryKeepsIDAndSeq(t *testing.T) {
	e := newEnv(t)
	m := message{ID: 7, GUID: "in-7", Sender: jon, ChatGUID: "any;-;" + jon, Text: "@budget hi"}
	e.handle(m)
	first := e.inbox("budget")
	e.handle(m)
	equal(t, e.inbox("budget"), first)
}

func TestLongTextSplitsWithHeaders(t *testing.T) {
	e := newEnv(t)
	word := strings.Repeat("x", 9) + " "
	e.agentSend("budget", `{"person": "jon"}`, strings.Repeat(word, 450))
	equal(t, len(e.f.sent), 3)
	for _, p := range e.f.sent {
		text := p["text"].(string)
		if !strings.HasPrefix(text, "🤖 𝗯𝘂𝗱𝗴𝗲𝘁\n") || len([]rune(text)) > maxMessageLen {
			t.Fatalf("chunk without a header or too long: %d runes", len([]rune(text)))
		}
	}
}

func TestConfigRejectsBadValues(t *testing.T) {
	for _, bad := range []string{
		strings.Replace(testConfig, "owner: jon", "owner: pat", 1),
		strings.Replace(testConfig, `jac: "+15550000002"`, `jac: "5550000002"`, 1),
		strings.Replace(testConfig, "history: 2", "history: 21", 1),
		strings.Replace(testConfig, "jon: write, jac: read", "jon: write, jac: admin", 1),
		strings.Replace(testConfig, "garden-token", "budget-token", 1),
	} {
		if _, err := parseConfig([]byte(bad)); err == nil {
			t.Errorf("accepted a bad config:\n%s", bad)
		}
	}
}
