// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// probe asks web for a page every 100 ms for the whole run, and keeps every
// request that went unanswered while it wasn't paused: traffic keeps flowing
// through any fault that leaves web's container running (#35).
type probe struct {
	url  string
	stop context.CancelFunc
	done chan struct{}

	mu     sync.Mutex
	paused int
	ok     bool
	fails  []string
}

func newProbe(url string) *probe {
	ctx, cancel := context.WithCancel(context.Background())
	p := &probe{url: url, stop: cancel, done: make(chan struct{})}
	go p.run(ctx)
	return p
}

func (p *probe) run(ctx context.Context) {
	defer close(p.done)
	client := &http.Client{Timeout: time.Second}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		why := ""
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.url, http.NoBody)
		res, err := client.Do(req)
		switch {
		case err != nil:
			why = err.Error()
		default:
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if res.StatusCode != http.StatusOK {
				why = res.Status
			}
		}
		if ctx.Err() != nil {
			return
		}
		p.mu.Lock()
		p.ok = why == ""
		if why != "" && p.paused == 0 {
			p.fails = append(p.fails, time.Now().Format("15:04:05.000")+" "+why)
		}
		p.mu.Unlock()
	}
}

// pause lets web go unanswered, as while Docker is down or its container is
// removed on purpose, until resume.
func (p *probe) pause() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paused++
	p.ok = false
}

// resume waits for web to answer again, then counts what goes unanswered again.
func (p *probe) resume(ctx context.Context) error {
	if p == nil {
		return nil
	}
	err := wait(ctx, 60*time.Second, func() error {
		p.mu.Lock()
		defer p.mu.Unlock()
		if !p.ok {
			return fmt.Errorf("web doesn't answer at %s", p.url)
		}
		return nil
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil && p.paused > 0 {
		p.paused--
	}
	return err
}

// take is what went unanswered since it was last taken.
func (p *probe) take() []string {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.fails
	p.fails = nil
	return out
}

func (p *probe) close() {
	p.stop()
	<-p.done
}
