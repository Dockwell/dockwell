// SPDX-License-Identifier: AGPL-3.0-only

package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
)

// AWS's own worked examples of Signature Version 4, from the S3 API reference.
func TestSignaturesMatchAWSsExamples(t *testing.T) {
	b := &Bucket{
		Region: "us-east-1", AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Now: func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) },
	}
	for want, u := range map[string]string{
		"fea454ca298b7da1c68078a5d1bdbfbbe0d65c699e0f91ac7a200a0136783543": "https://examplebucket.s3.amazonaws.com/?lifecycle=",
		"34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7": "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J",
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		b.sign(req, emptyHash)
		if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature="+want) {
			t.Errorf("%s: Authorization = %s, want signature %s", u, got, want)
		}
	}
}

func bench(t *testing.T) (*Bucket, *s3test.Server) {
	t.Helper()
	f := s3test.New(t)
	return &Bucket{Endpoint: f.URL(), Name: "acme-backups", AccessKey: "key", SecretKey: "secret", Client: f.Client()}, f
}

// upload is data in parts of size.
func upload(key string, data []byte, size int) Upload {
	count := (len(data) + size - 1) / size
	return Upload{
		Key: key, Count: count,
		Length: func(i int) int64 { return int64(min(size, len(data)-i*size)) },
		Part: func(i int) Part {
			return func() (io.Reader, error) { return bytes.NewReader(data[i*size : min((i+1)*size, len(data))]), nil }
		},
	}
}

func TestAnObjectGoesAndComesBack(t *testing.T) {
	b, _ := bench(t)
	ctx := t.Context()
	key := "env_1/data/bkp 1+x.tar.gz.sealed"
	data := bytes.Repeat([]byte("0123456789"), 1000)
	var seen []int64
	up := upload(key, data, 3000)
	up.Progress = func(sent int64) { seen = append(seen, sent) }
	if err := b.Store(ctx, up); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 4 || seen[3] != int64(len(data)) {
		t.Errorf("progress = %v", seen)
	}
	if size, err := b.Size(ctx, key); err != nil || size != int64(len(data)) {
		t.Errorf("Size = %d, %v", size, err)
	}
	r, err := b.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("Get = %d bytes, %v", len(got), err)
	}
	if err := b.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Size(ctx, key); !IsNotFound(err) {
		t.Errorf("after Delete: %v", err)
	}
	if err := b.Delete(ctx, key); err != nil {
		t.Errorf("deleting what's gone: %v", err)
	}
	if err := b.Put(ctx, "small.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
}

// A part that fails on the way goes again, made from its start.
// A bucket in another region gets a 301 with no Location, which Go hands back as the
// answer: that's no write, and it says where the bucket is.
func TestAnAnswerThatIsntA2xxIsntDone(t *testing.T) {
	b, f := bench(t)
	f.MovedTo = "eu-west-2"
	err := b.Put(t.Context(), "probe", []byte("x"))
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusMovedPermanently || e.Code != "PermanentRedirect" || e.Region != "eu-west-2" {
		t.Fatalf("a put to a bucket in another region = %v", err)
	}
	if left := f.Objects(); len(left) != 0 {
		t.Errorf("it stored %v", left)
	}
}

func TestAFailedPartGoesAgain(t *testing.T) {
	b, f := bench(t)
	f.FailParts = 1
	data := bytes.Repeat([]byte("x"), 9000)
	if err := b.Store(t.Context(), upload("k", data, 4000)); err != nil {
		t.Fatal(err)
	}
	if got := f.Objects()["/acme-backups/k"]; !bytes.Equal(got, data) {
		t.Errorf("stored %d bytes", len(got))
	}
}

// An upload that can't finish leaves nothing to be billed for.
func TestAFailedUploadLeavesNothing(t *testing.T) {
	b, f := bench(t)
	f.FinishFails = true
	err := b.Store(t.Context(), upload("k", []byte("data"), 2))
	if err == nil || !strings.Contains(err.Error(), "InternalError") {
		t.Fatalf("Store = %v", err)
	}
	if f.Aborted != 1 {
		t.Errorf("aborted %d uploads", f.Aborted)
	}
	if got := f.Objects(); len(got) != 0 {
		t.Errorf("objects = %v", got)
	}
}

func TestARefusalSaysWhyAndHoldsNoSecret(t *testing.T) {
	b, f := bench(t)
	f.Refuse = "AccessDenied"
	err := b.Put(context.Background(), "k", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("Put = %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("the error holds the secret: %v", err)
	}
}

func TestPartsStayInBounds(t *testing.T) {
	for _, size := range []int64{1, 8 << 20, 1 << 40, 5 << 40} {
		part := PartSize(size, 1<<20)
		if part%(1<<20) != 0 {
			t.Errorf("size %d: part %d isn't whole chunks", size, part)
		}
		if parts := size/part + 1; parts > 10000 {
			t.Errorf("size %d: %d parts", size, parts)
		}
		if part < 5<<20 {
			t.Errorf("size %d: part %d is smaller than S3 takes", size, part)
		}
	}
}

func TestKeysEscapeAsSignaturesExpect(t *testing.T) {
	if got := escapePath("a b/c+d~e.tar"); got != "a%20b/c%2Bd~e.tar" {
		t.Errorf("escapePath = %q", got)
	}
	if got := canonicalQuery(url.Values{"uploads": {""}, "b": {"x y"}}); got != "b=x%20y&uploads=" {
		t.Errorf("canonicalQuery = %q", got)
	}
}

// Storage that stops answering is given up on, whether it holds a request or stops
// partway through an answer, and says what to check.
func TestStorageThatStopsAnsweringIsGivenUpOn(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Length", "10")
			_, _ = w.Write([]byte("half"))
			w.(http.Flusher).Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	b := &Bucket{Endpoint: srv.URL, Name: "acme", AccessKey: "key", SecretKey: "secret", Stall: 100 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err := b.Put(ctx, "k", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "stopped answering") || ctx.Err() != nil {
		t.Errorf("a request nothing answered = %v", err)
	}
	body, err := b.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if _, err := io.ReadAll(body); err == nil || !strings.Contains(err.Error(), "stopped answering") || ctx.Err() != nil {
		t.Errorf("an answer that stopped = %v", err)
	}
}

// slowly reads a byte at a time, which keeps a request alive however long it takes.
type slowly struct {
	left int
	wait time.Duration
}

func (s *slowly) Read(p []byte) (int, error) {
	if s.left == 0 {
		return 0, io.EOF
	}
	time.Sleep(s.wait)
	s.left--
	p[0] = 'x'
	return 1, nil
}

// The deadline counts from the last byte: an upload that's slow and moving goes on.
func TestASlowRequestThatMovesIsLeftAlone(t *testing.T) {
	b, f := bench(t)
	b.Stall = 300 * time.Millisecond
	const each = 100 * time.Millisecond
	up := Upload{
		Key: "slow", Count: 1, Length: func(int) int64 { return 6 },
		Part: func(int) Part { return func() (io.Reader, error) { return &slowly{left: 6, wait: each}, nil } },
	}
	if err := b.Store(t.Context(), up); err != nil {
		t.Fatalf("a slow upload that took %s, with %s to stall = %v", 6*each, b.Stall, err)
	}
	if got := len(f.Objects()); got != 1 {
		t.Errorf("stored %d", got)
	}
}

func TestListBucketsAsksTheServiceItself(t *testing.T) {
	srv := s3test.New(t)
	srv.Buckets = []string{"backups", "media &amp; more"}
	b := &Bucket{Endpoint: srv.URL(), Name: "backups", AccessKey: "AK", SecretKey: "SK", Client: srv.Client()}
	got, err := b.ListBuckets(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"backups", "media & more"}; !slices.Equal(got, want) {
		t.Errorf("listed %q, want %q", got, want)
	}
	// The bucket it's for is left alone: listing asks the service, at its root.
	if b.Name != "backups" {
		t.Errorf("listing changed the bucket to %q", b.Name)
	}
	if _, err := (&Bucket{Endpoint: srv.URL(), Name: "nope", AccessKey: "AK", SecretKey: "SK", Client: srv.Client()}).Size(t.Context(), "x"); !IsNotFound(err) {
		t.Errorf("a bucket that isn't there: %v", err)
	}
}
