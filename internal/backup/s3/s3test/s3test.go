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
	"slices"
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
	// Refuse answers everything with this code, or with RefuseOnly, requests of that
	// method: POST is how every upload starts.
	Refuse     string
	RefuseOnly string
	// RefuseQuery refuses only requests whose query has this parameter, as a key
	// that may not list is refused list-type=2 alone.
	RefuseQuery string
	// RefuseStatus is the status a refusal answers with: 403 unless set, such as 500.
	RefuseStatus int
	// MovedTo answers every request for a bucket as S3 does when it's in another
	// region: 301 PermanentRedirect, with no Location, and that region in its header.
	MovedTo string
	// FinishFails has an upload's last step answer 200 and say it failed, as S3 can.
	FinishFails bool
	// Authorizations are the Authorization headers it was sent, and Methods the
	// method of each request, in order.
	Authorizations []string
	Methods        []string
	// Expires are how long each presigned URL it was sent lasts, in seconds, as
	// X-Amz-Expires says.
	Expires []string
	// Hold keeps every request waiting until it closes, or with HoldOnly, those whose
	// path ends with that. Nil holds none.
	Hold     chan struct{}
	HoldOnly string
	// Buckets are the buckets it lists, and the only ones it holds objects in, when set.
	// Nil lists none and takes any.
	Buckets []string
	srv     *httptest.Server
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

// Set stores an object as it is, as whoever can write to a bucket can.
func (s *Server) Set(path string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[path] = body
}

// ServeHTTP answers as S3 does.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Hold != nil && strings.HasSuffix(r.URL.Path, s.HoldOnly) {
		<-s.Hold
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	auth := r.Header.Get("Authorization")
	s.Authorizations = append(s.Authorizations, auth)
	s.Methods = append(s.Methods, r.Method)
	// Signed in its header, or presigned in its query.
	credential, presigned := strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 Credential="), r.URL.Query().Get("X-Amz-Signature") != ""
	if presigned {
		credential = r.URL.Query().Get("X-Amz-Credential")
		s.Expires = append(s.Expires, r.URL.Query().Get("X-Amz-Expires"))
	} else if credential == auth {
		s.t.Errorf("%s %s isn't signed: %q", r.Method, r.URL, auth)
	}
	// A request signed for the region the bucket moved to is served there.
	if s.MovedTo != "" && r.URL.Path != "/" && !strings.Contains(credential, "/"+s.MovedTo+"/") {
		w.Header().Set("x-amz-bucket-region", s.MovedTo)
		w.WriteHeader(http.StatusMovedPermanently)
		fmt.Fprint(w, "<Error><Code>PermanentRedirect</Code><Message>The bucket you are attempting to access must be addressed using the specified endpoint.</Message></Error>")
		return
	}
	if s.Refuse != "" && (s.RefuseOnly == "" || s.RefuseOnly == r.Method) && (s.RefuseQuery == "" || r.URL.Query().Has(s.RefuseQuery)) {
		status := http.StatusForbidden
		if s.RefuseStatus != 0 {
			status = s.RefuseStatus
		}
		w.WriteHeader(status)
		fmt.Fprintf(w, "<Error><Code>%s</Code><Message>no</Message></Error>", s.Refuse)
		return
	}
	key := r.URL.Path
	q := r.URL.Query()
	if key == "/" && r.Method == http.MethodGet {
		fmt.Fprint(w, "<ListAllMyBucketsResult><Buckets>")
		for _, b := range s.Buckets {
			fmt.Fprintf(w, "<Bucket><Name>%s</Name><CreationDate>2026-01-01T00:00:00Z</CreationDate></Bucket>", b)
		}
		fmt.Fprint(w, "</Buckets></ListAllMyBucketsResult>")
		return
	}
	if bucket, _, _ := strings.Cut(strings.TrimPrefix(key, "/"), "/"); s.Buckets != nil && !slices.Contains(s.Buckets, bucket) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "<Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist</Message></Error>")
		return
	}
	// ListObjectsV2 of a bucket, path style: at most max-keys of its keys under prefix.
	if bucket, rest, _ := strings.Cut(strings.TrimPrefix(key, "/"), "/"); r.Method == http.MethodGet && q.Get("list-type") == "2" && rest == "" {
		limit, _ := strconv.Atoi(q.Get("max-keys"))
		var keys []string
		for path := range s.objects {
			if k, ok := strings.CutPrefix(path, "/"+bucket+"/"); ok && strings.HasPrefix(k, q.Get("prefix")) {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		if limit > 0 && len(keys) > limit {
			keys = keys[:limit]
		}
		type contents struct {
			Key string
		}
		list := struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Contents []contents
			KeyCount int
		}{KeyCount: len(keys)}
		for _, k := range keys {
			list.Contents = append(list.Contents, contents{Key: k})
		}
		raw, err := xml.Marshal(list)
		if err != nil {
			s.t.Errorf("can't write a list: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write(raw)
		return
	}
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
			return // its sender went away, as one stopped or cancelled does: nothing of it is kept
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
			return // its sender went away, as one stopped or cancelled does: nothing of it is kept
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
