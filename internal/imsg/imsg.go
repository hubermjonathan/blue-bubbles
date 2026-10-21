// Package imsg runs `imsg rpc` as a child process and speaks JSON-RPC over its stdio.
package imsg

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Client runs `imsg rpc`, restarts it when it exits, and matches each reply to its call.
type Client struct {
	path string

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.Writer
	next    int64
	pending map[int64]chan rpcReply

	// Events carries notifications, plus a "started" event each time the child starts.
	// Its buffer is larger than imsg's largest watch buffer, so the reader never blocks
	// on it while the event loop waits on a call that the reader has to answer.
	Events chan Event
}

type Event struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcReply struct {
	ID     *int64          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *Error) Error() string { return fmt.Sprintf("imsg: %s (%d) %s", e.Message, e.Code, e.Data) }

func New(path string) *Client {
	return &Client{path: path, pending: map[int64]chan rpcReply{}, Events: make(chan Event, 8192)}
}

func (c *Client) Run() {
	for {
		if err := c.runOnce(); err != nil {
			log.Printf("imsg: %v", err)
		}
		time.Sleep(5 * time.Second)
	}
}

func (c *Client) runOnce() error {
	cmd := exec.Command(c.path, "rpc")
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	c.mu.Lock()
	c.cmd, c.stdin = cmd, stdin
	c.mu.Unlock()
	c.Events <- Event{Method: "started"}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var r struct {
			rpcReply
			Event
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			log.Printf("imsg: unreadable line: %v", err)
			continue
		}
		if r.ID == nil {
			c.Events <- r.Event
			continue
		}
		c.mu.Lock()
		ch := c.pending[*r.ID]
		delete(c.pending, *r.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- r.rpcReply
		}
	}

	c.mu.Lock()
	c.cmd, c.stdin = nil, nil
	for id, ch := range c.pending {
		ch <- rpcReply{Error: &Error{Message: "imsg exited"}}
		delete(c.pending, id)
	}
	c.mu.Unlock()
	return cmd.Wait()
}

func (c *Client) Restart() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil {
		c.cmd.Process.Kill()
	}
}

func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	c.mu.Lock()
	if c.stdin == nil {
		c.mu.Unlock()
		return errors.New("imsg is not running")
	}
	c.next++
	id := c.next
	ch := make(chan rpcReply, 1)
	c.pending[id] = ch
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err == nil {
		_, err = c.stdin.Write(append(b, '\n'))
	}
	if err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()

	select {
	case r := <-ch:
		if r.Error != nil {
			return r.Error
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(r.Result, out)
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	}
}
