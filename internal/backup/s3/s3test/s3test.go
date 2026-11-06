// SPDX-License-Identifier: AGPL-3.0-only

// Package s3test is a fake of S3-compatible storage for tests: enough to hold objects
// and take uploads in parts, and to say what it was asked.
package s3test

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Server is a fake bucket store.
type Server struct {
	t  testing.TB
	mu sync.Mutex
	// objects are what's stored, by path, with the bucket's name first.
	objects map[string][]byte
	uploads map[string]map[int][]byte
	// Aborted counts uploads given up on.
	Aborted int
	// FailParts fails this many parts, as a network does.
	FailParts int
	// Refuse answers everything with this code.
	Refuse string
	// FinishFails has an upload's last step answer 200 and say it failed, as S3 can.
	FinishFails bool
	// Authorizations are the Authorization headers it was sent.
	Authorizations []string
	srv            *httptest.Server
}

// New starts a fake that stops when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{t: t, objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	s.srv = httptest.NewServer(s)
	t.Cleanup(s.srv.Close)
	return s
}

// URL is its address.
func (s *Server) URL() string { return s.srv.URL }

// Client reaches it.
func (s *Server) Client() *http.Client { return s.srv.Client() }

// Objects is a copy of what's stored.
func (s *Server) Objects() map[string][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.objects)
}

// ServeHTTP answers as S3 does.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	auth := r.Header.Get("Authorization")
	s.Authorizations = append(s.Authorizations, auth)
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=") {
		s.t.Errorf("%s %s isn't signed: %q", r.Method, r.URL, auth)
	}
	if s.Refuse != "" {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "<Error><Code>%s</Code><Message>no</Message></Error>", s.Refuse)
		return
	}
	key := r.URL.Path
	q := r.URL.Query()
	id, inUpload := q["uploadId"]
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		upload := "up" + strconv.Itoa(len(s.uploads)+1)
		s.uploads[upload] = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", upload)
	case r.Method == http.MethodPut && inUpload:
		if s.FailParts > 0 {
			s.FailParts--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		n, _ := strconv.Atoi(q.Get("partNumber"))
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s.t.Error(err)
		}
		if int64(len(body)) != r.ContentLength {
			s.t.Errorf("part %d says %d bytes and holds %d", n, r.ContentLength, len(body))
		}
		s.uploads[id[0]][n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, n))
	case r.Method == http.MethodPost && inUpload:
		var done struct {
			Parts []struct {
				Number int    `xml:"PartNumber"`
				ETag   string `xml:"ETag"`
			} `xml:"Part"`
		}
		if err := xml.NewDecoder(r.Body).Decode(&done); err != nil {
			s.t.Error(err)
		}
		if s.FinishFails {
			fmt.Fprint(w, "<Error><Code>InternalError</Code><Message>try again</Message></Error>")
			return
		}
		var whole bytes.Buffer
		for _, p := range done.Parts {
			if p.ETag != fmt.Sprintf(`"etag-%d"`, p.Number) {
				s.t.Errorf("part %d has ETag %s", p.Number, p.ETag)
			}
			whole.Write(s.uploads[id[0]][p.Number])
		}
		s.objects[key] = whole.Bytes()
		fmt.Fprint(w, "<CompleteMultipartUploadResult></CompleteMultipartUploadResult>")
	case r.Method == http.MethodDelete && inUpload:
		s.Aborted++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s.t.Error(err)
		}
		s.objects[key] = body
	case r.Method == http.MethodGet, r.Method == http.MethodHead:
		body, ok := s.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "<Error><Code>NoSuchKey</Code><Message>none</Message></Error>")
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	case r.Method == http.MethodDelete:
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		s.t.Errorf("unexpected %s %s", r.Method, r.URL)
	}
}
