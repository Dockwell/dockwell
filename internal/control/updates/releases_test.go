// SPDX-License-Identifier: AGPL-3.0-only

package updates

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const checksums = `3f2a9c1d4e5b6a7f8091a2b3c4d5e6f708192a3b3f2a9c1d4e5b6a7f8091a2b3  dockwell-agent_0.9.5_linux_amd64.tar.gz
4f2a9c1d4e5b6a7f8091a2b3c4d5e6f708192a3b3f2a9c1d4e5b6a7f8091a2b3  dockwell_0.9.5_linux_amd64.tar.gz
5f2a9c1d4e5b6a7f8091a2b3c4d5e6f708192a3b3f2a9c1d4e5b6a7f8091a2b3  dockwell_0.9.5_linux_arm64.tar.gz
`

func TestTheNewestRelease(t *testing.T) {
	var asked atomic.Int32
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if r.URL.Path != "/releases/latest/download/checksums.txt" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(checksums))
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	r := NewReleases(srv.URL+"/releases/", slog.New(slog.DiscardHandler))
	r.Now = func() time.Time { return now }

	if v, _ := r.Newest(); v != "" {
		t.Fatalf("knew %q before looking", v)
	}
	<-r.Look()
	if v, at := r.Newest(); v != "0.9.5" || !at.Equal(now) {
		t.Errorf("newest = %q at %v", v, at)
	}
	// Within a day, looking reads nothing.
	now = now.Add(23 * time.Hour)
	if done := r.Look(); done != nil || asked.Load() != 1 {
		t.Errorf("read again within a day: %d reads", asked.Load())
	}
	// A day on, it reads again, and a read that fails keeps what it knew, and
	// is tried again within the hour.
	now = now.Add(2 * time.Hour)
	status.Store(http.StatusBadGateway)
	<-r.Look()
	if v, _ := r.Newest(); v != "0.9.5" || asked.Load() != 2 {
		t.Errorf("after a failed read: %q, %d reads", v, asked.Load())
	}
	now = now.Add(30 * time.Minute)
	if done := r.Look(); done != nil {
		t.Error("read again within the hour after a failure")
	}
	now = now.Add(31 * time.Minute)
	if done := r.Look(); done == nil {
		t.Error("didn't read again an hour after a failure")
	} else {
		<-done
	}
}

func TestReleasesThatNameNoArchive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("nothing to see\n"))
	}))
	defer srv.Close()
	r := NewReleases(srv.URL, slog.New(slog.DiscardHandler))
	if _, err := r.read(t.Context()); err == nil || err.Error() != srv.URL+"/latest/download/checksums.txt names no archive of Dockwell" {
		t.Errorf("err = %v", err)
	}
}
