// SPDX-License-Identifier: AGPL-3.0-only

package enroll

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// silentProxy carries a connection until told to go silent, then carries nothing
// and closes nothing, as a network that drops every packet.
type silentProxy struct {
	ln     net.Listener
	done   chan struct{}
	mu     sync.Mutex
	silent bool
	conns  []net.Conn
}

// close ends it, and every connection it carries.
func (p *silentProxy) close() {
	close(p.done)
	_ = p.ln.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
}

func (p *silentProxy) quiet() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.silent
}

func (p *silentProxy) serve(target string) {
	for {
		down, err := p.ln.Accept()
		if err != nil {
			return
		}
		up, err := net.Dial("tcp", target)
		if err != nil {
			_ = down.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, down, up)
		p.mu.Unlock()
		carry := func(dst, src net.Conn) {
			buf := make([]byte, 32<<10)
			for {
				n, err := src.Read(buf)
				if p.quiet() {
					<-p.done // held: nothing passes, nothing closes
					return
				}
				if n > 0 {
					if _, werr := dst.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}
		go carry(up, down)
		go carry(down, up)
	}
}

// A connection to the control plane that goes silent, as when the network between
// drops every packet, is taken for lost once a ping goes unanswered, not after TCP's
// quarter of an hour, so the agent knows it's away and can put back what goes
// missing (ADR 0054).
func TestASilentConnectionIsLost(t *testing.T) {
	was := silentFor
	silentFor = 200 * time.Millisecond
	t.Cleanup(func() { silentFor = was })

	started := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &silentProxy{ln: ln, done: make(chan struct{})}
	t.Cleanup(p.close)
	go p.serve(srv.Listener.Addr().String())

	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	client := httpClient(&tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS13})
	t.Cleanup(client.CloseIdleConnections)
	res, err := client.Get("https://" + ln.Addr().String() + "/") //nolint:bodyclose // closed below
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.ProtoMajor != 2 {
		t.Fatalf("spoke HTTP/%d, want 2", res.ProtoMajor)
	}
	<-started
	p.mu.Lock()
	p.silent = true
	p.mu.Unlock()

	read := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, res.Body)
		read <- err
	}()
	select {
	case err := <-read:
		if err == nil {
			t.Error("the silent stream ended cleanly, want an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a connection silent for 5 s, 25 times the ping's wait, wasn't taken for lost")
	}
}
