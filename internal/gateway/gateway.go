// Package gateway routes texts between people, dedicated agents, and coding sessions.
// Inbound messages arrive from imsg, and replies go out through one send loop.
package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/hubermjonathan/blue-bubbles/internal/config"
	"github.com/hubermjonathan/blue-bubbles/internal/store"
)

// Caller makes one JSON-RPC call to imsg.
type Caller interface {
	Call(ctx context.Context, method string, params, out any) error
}

type Gateway struct {
	config  func() *config.Config
	store   *store.Store
	imsg    Caller
	service func(messageGUID string) (string, error)
	outbox  chan *outgoing

	mu       sync.Mutex
	changed  chan struct{}
	watchers map[string]watcher
}

func New(cfg func() *config.Config, st *store.Store, im Caller, service func(string) (string, error)) *Gateway {
	return &Gateway{
		config:   cfg,
		store:    st,
		imsg:     im,
		service:  service,
		outbox:   make(chan *outgoing, 64),
		changed:  make(chan struct{}),
		watchers: map[string]watcher{},
	}
}

// changes returns a channel that closes on the next inbox, thread, or watcher change.
func (g *Gateway) changes() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.changed
}

func (g *Gateway) wake() {
	g.mu.Lock()
	defer g.mu.Unlock()
	close(g.changed)
	g.changed = make(chan struct{})
}

// waitFor calls ready until it reports true, the wait passes, or ctx ends.
func (g *Gateway) waitFor(ctx context.Context, wait time.Duration, ready func() (bool, error)) error {
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

func (g *Gateway) closeThread(t *store.Thread, notify bool) error {
	wasOpen, err := g.store.CloseThread(t.ID)
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
