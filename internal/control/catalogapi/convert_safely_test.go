// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog/convert"
	"github.com/dockwell/dockwell/internal/gitfetch"
	"github.com/dockwell/dockwell/internal/gitfetch/gitfetchtest"
)

// lockedBuffer is a log a test reads while the code under it writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Converting a store's commit that stops on what Dockwell didn't expect leaves the
// commit unread, saying so, and logs the source; an app whose reading stopped is
// left out, and logged with its source. Neither stops the control plane, which reads
// a source with nobody watching once it follows a branch (#570's review, H1).
func TestAStoreThatStopsConvertingNeverStopsTheControlPlane(t *testing.T) {
	repo := gitfetchtest.New(t)
	head := repo.Commit(map[string]string{
		"LICENSE":                       "                                 Apache License\n                           Version 2.0, January 2004\n",
		"templates/compose/my-app.yaml": "# slogan: An app.\n# port: 80\nservices:\n  web:\n    image: nginx:1.27\n",
	})
	spec := &resourcev1.CatalogSource{Repository: repo.URL, Commit: head, Format: resourcev1.CatalogFormat_CATALOG_FORMAT_COOLIFY}
	logs := &lockedBuffer{}
	s := &Sources{Dir: t.TempDir(), Client: gitfetchtest.Client(), logger: slog.New(slog.NewTextHandler(logs, nil))}

	// The whole conversion panics.
	s.OnServer = func(context.Context, convert.Store, *convert.Listing, convert.Source, time.Time, uint32) (convert.Run, error) {
		panic("index out of range [7] with length 1")
	}
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("converting a store's commit panicked past its recover: %v", p)
			}
		}()
		_, err = s.files(t.Context(), spec)
	}()
	if err == nil || !strings.Contains(err.Error(), "stopped on what Dockwell didn't expect (index out of range [7] with length 1)") {
		t.Errorf("a conversion that panicked: %v", err)
	}
	if got := logs.String(); !strings.Contains(got, "reading a catalog source stopped on what Dockwell didn't expect") || !strings.Contains(got, repo.URL) {
		t.Errorf("the log doesn't name the source:\n%s", got)
	}

	// One app's reading panicked: it's left out, logged with the source, and the
	// commit reads.
	s.OnServer = func(ctx context.Context, store convert.Store, l *convert.Listing, src convert.Source, committed time.Time, before uint32) (convert.Run, error) {
		run, err := convert.OnServer(ctx, store, l, src, committed, before)
		run.Results = append(run.Results, convert.Result{ID: "boom", Path: "templates/compose/boom.yaml", LeftOut: "reading its files stopped", Class: convert.ClassFile, Panic: "nil map"})
		return run, err
	}
	other := &resourcev1.CatalogSource{Repository: repo.URL, Commit: head, Format: resourcev1.CatalogFormat_CATALOG_FORMAT_COOLIFY, Path: "."}
	files, err := s.files(t.Context(), other)
	if err != nil || files.entries["my-app.yaml"] == nil {
		t.Fatalf("the commit beside an app that stopped: %v, %v", files.entries, err)
	}
	if got := logs.String(); !strings.Contains(got, "reading a catalog source's app stopped") || !strings.Contains(got, "templates/compose/boom.yaml") || !strings.Contains(got, "nil map") {
		t.Errorf("the log doesn't name the app:\n%s", got)
	}
}

// A read of a Dockwell-format source whose fetch panics says so, and a later read of
// the same commit fetches again and returns, rather than waiting on the fetch that
// stopped, so the follow loop never waits on it (#570's re-check, N2).
func TestAFetchThatPanicsLeavesNoReadWaiting(t *testing.T) {
	asked := 0
	fetch := func(context.Context, *http.Client, string, string, string, gitfetch.Limits) (map[string][]byte, error) {
		asked++
		panic("index out of range [7] with length 1")
	}
	logs := &lockedBuffer{}
	s := &Sources{Dir: t.TempDir(), Fetch: fetch, logger: slog.New(slog.NewTextHandler(logs, nil))}
	spec := &resourcev1.CatalogSource{Repository: "https://example.com/catalog", Commit: strings.Repeat("a", 40)}
	for i := range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		_, err := s.files(ctx, spec)
		cancel()
		if err == nil || !strings.Contains(err.Error(), "stopped on what Dockwell didn't expect (index out of range [7] with length 1)") {
			t.Fatalf("read %d of a commit whose fetch panics: %v", i+1, err)
		}
	}
	if asked != 2 {
		t.Errorf("the fetch was asked %d times, want once a read", asked)
	}
	if got := logs.String(); !strings.Contains(got, "https://example.com/catalog") {
		t.Errorf("the log doesn't name the source:\n%s", got)
	}
}
