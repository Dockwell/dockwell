// SPDX-License-Identifier: AGPL-3.0-only

package s3test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// failures is a test that keeps what it's told fails, rather than failing.
type failures struct {
	testing.TB
	mu   sync.Mutex
	said []string
}

func (f *failures) Error(args ...any) { f.Errorf("%s", fmt.Sprint(args...)) }

func (f *failures) Errorf(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.said = append(f.said, fmt.Sprintf(format, args...))
}

// cutShort is a body whose sender goes away partway, as a closed connection's does.
type cutShort struct{ left int }

func (c *cutShort) Read(p []byte) (int, error) {
	if c.left == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := min(len(p), c.left)
	c.left -= n
	return n, nil
}

// A body whose sender goes away partway, as a control plane stopping mid-send
// leaves one, is the sender's to see to: the store keeps nothing of it, and the
// test it serves hasn't failed (#684).
func TestASendCutShortKeepsNothingAndFailsNothing(t *testing.T) {
	f := &failures{TB: t}
	s := New(f)
	signed := func(r *http.Request) *http.Request {
		r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=key/20261115/auto/s3/aws4_request")
		return r
	}
	started := httptest.NewRecorder()
	s.ServeHTTP(started, signed(httptest.NewRequest(http.MethodPost, "/bucket/part?uploads", http.NoBody)))
	if !strings.Contains(started.Body.String(), "<UploadId>up1</UploadId>") {
		t.Fatalf("starting an upload: %s", started.Body.String())
	}
	for _, path := range []string{"/bucket/whole", "/bucket/part?partNumber=1&uploadId=up1"} {
		r := httptest.NewRequest(http.MethodPut, path, &cutShort{left: 10})
		r.ContentLength = 100
		s.ServeHTTP(httptest.NewRecorder(), signed(r))
	}
	if len(f.said) != 0 {
		t.Errorf("a send cut short failed the test: %s", strings.Join(f.said, "; "))
	}
	if got := s.Objects(); len(got) != 0 {
		t.Errorf("a send cut short left %v", got)
	}
}
