// SPDX-License-Identifier: AGPL-3.0-only

package sftpstore

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	remoteTimeout = 1
	localTimeout  = 2
)

type idleConn struct {
	net.Conn
	idle                  time.Duration
	active                atomic.Bool
	timeout               atomic.Int32
	mu                    sync.Mutex
	lastReply, localSince time.Time
}

func (c *idleConn) refresh() {
	if c.active.Load() {
		_ = c.SetDeadline(time.Now().Add(c.idle))
	}
}

func (c *idleConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.afterIO(n, err)
	return n, err
}

func (c *idleConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.afterIO(n, err)
	return n, err
}

func (c *idleConn) afterIO(n int, err error) {
	if n > 0 {
		c.refresh()
	}
	var timeout net.Error
	if c.active.Load() && errors.As(err, &timeout) && timeout.Timeout() {
		c.timeout.CompareAndSwap(0, remoteTimeout)
	}
}

func (c *idleConn) progress() {
	c.mu.Lock()
	c.lastReply = time.Now()
	c.mu.Unlock()
}

func (c *idleConn) local(start bool) {
	c.mu.Lock()
	if start {
		c.localSince = time.Now()
	} else {
		c.localSince = time.Time{}
		// Give the peer its full time to respond to the next request.
		c.lastReply = time.Now()
	}
	c.mu.Unlock()
}

func (c *idleConn) watch(ctx context.Context) {
	timer := time.NewTimer(c.idle)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			c.mu.Lock()
			since, why := c.lastReply, int32(remoteTimeout)
			if !c.localSince.IsZero() {
				since, why = c.localSince, localTimeout
			}
			remaining := c.idle - time.Since(since)
			if remaining <= 0 {
				c.timeout.CompareAndSwap(0, why)
			}
			c.mu.Unlock()
			if remaining <= 0 {
				_ = c.Close()
				return
			}
			timer.Reset(remaining)
		}
	}
}

func (c *idleConn) failure(fallback error) error {
	switch c.timeout.Load() {
	case localTimeout:
		return ErrLocalIdle
	case remoteTimeout:
		return ErrIdle
	default:
		return fallback
	}
}

// Only SFTP replies count as remote transfer progress. SSH keepalive replies
// arrive through the transport and never pass through this reader.
type progressReader struct {
	io.Reader
	conn *idleConn
}

func (r progressReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.conn.progress()
	}
	return n, err
}

// localReader and localWriter mark time spent waiting for our own input or
// output. Only a real SFTP packet can move the remote reply timer during I/O.
type localReader struct {
	io.Reader
	conn *idleConn
}

func (r localReader) Read(p []byte) (int, error) {
	r.conn.local(true)
	defer r.conn.local(false)
	return r.Reader.Read(p)
}

type localWriter struct {
	io.Writer
	conn *idleConn
}

func (w localWriter) Write(p []byte) (int, error) {
	w.conn.local(true)
	defer w.conn.local(false)
	return w.Writer.Write(p)
}

func (c *idleConn) keepalive(ctx context.Context, interval time.Duration, conn *ssh.Client) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// A negative response also proves the transport is alive.
			expires := time.AfterFunc(3*interval, func() {
				c.timeout.CompareAndSwap(0, remoteTimeout)
				_ = c.Close()
			})
			_, _, err := conn.SendRequest("keepalive@openssh.com", true, nil)
			expires.Stop()
			if err != nil {
				return
			}
		}
	}
}
