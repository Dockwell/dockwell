// SPDX-License-Identifier: AGPL-3.0-only

package probe

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
)

// The fake's address is this machine's loopback, which the probe reaches only as
// one of the app's networks.
var here = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}

func s3Request(srv *s3test.Server, presign bool) *Request {
	return &Request{
		Kind:  KindS3,
		Allow: here,
		S3: &S3{
			Endpoint: srv.URL(), Bucket: "uploads", Prefix: "outline/", Region: "auto",
			AccessKey: "AKIDEXAMPLE", SecretKey: "the-secret", Presign: presign,
		},
	}
}

// A bucket that takes a write, gives it back, fetches it by a presigned URL and
// deletes it passes, and keeps nothing of the check.
func TestABucketThatWorksPasses(t *testing.T) {
	t.Parallel()
	srv := s3test.New(t)
	got, err := Check(t.Context(), s3Request(srv, true))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Passed || got.Step != "" || got.Code != "" {
		t.Fatalf("a working bucket checks as %+v", got)
	}
	if left := srv.Objects(); len(left) != 0 {
		t.Errorf("the check left %v", left)
	}
	presigned := 0
	for _, a := range srv.Authorizations {
		if a == "" {
			presigned++
		}
	}
	if presigned != 1 {
		t.Errorf("the check fetched %d times by a presigned URL, want once", presigned)
	}
	if !slices.Equal(srv.Expires, []string{"60"}) {
		t.Errorf("the check's presigned URLs last %v seconds, want a minute", srv.Expires)
	}
}

// Where the app's networks don't take it, the probe reaches nothing on this
// server, and says so as a refusal before any request leaves.
func TestAnAddressTheAppCantReachIsRefused(t *testing.T) {
	t.Parallel()
	srv := s3test.New(t)
	req := s3Request(srv, false)
	req.Allow = nil
	got, err := Check(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Passed || got.Step != StepWrite || got.Code != CodeRefused || !strings.Contains(got.Words, "this server's own address") {
		t.Errorf("this server's loopback checks as %+v", got)
	}
	if len(srv.Authorizations) != 0 {
		t.Errorf("the refused check sent %d requests", len(srv.Authorizations))
	}
	// Nor one the server refuses outright, though the app's networks hold it.
	req.Allow, req.Refuse = here, here
	if got, _ := Check(t.Context(), req); got.Code != CodeRefused || !strings.Contains(got.Words, "this server's own addresses") {
		t.Errorf("an address of the server's own checks as %+v", got)
	}
}

// A service's refusal comes back as its code, with its message where that holds
// nothing of the request: SignatureDoesNotMatch's carries the string signed.
func TestARefusalSaysTheCodeAndASafeMessage(t *testing.T) {
	t.Parallel()
	for code, message := range map[string]string{"AccessDenied": "no", "NoSuchBucket": "no", "SignatureDoesNotMatch": "", "InvalidAccessKeyId": ""} {
		srv := s3test.New(t)
		srv.Refuse = code
		got, err := Check(t.Context(), s3Request(srv, false))
		if err != nil {
			t.Fatal(err)
		}
		if got.Passed || got.Step != StepWrite || got.Code != code || got.Status != 403 || got.Message != message {
			t.Errorf("%s checks as %+v", code, got)
		}
	}
	// A read refused after the write deletes what the check wrote.
	srv := s3test.New(t)
	srv.Refuse, srv.RefuseOnly = "AccessDenied", "GET"
	if got, _ := Check(t.Context(), s3Request(srv, false)); got.Step != StepRead || got.Code != "AccessDenied" {
		t.Errorf("a bucket that can't be read checks as %+v", got)
	}
	if left := srv.Objects(); len(left) != 0 {
		t.Errorf("a failed check left %v", left)
	}
}

// A bucket in another region than asked is checked there, and says which.
func TestABucketElsewhereSaysItsRegion(t *testing.T) {
	t.Parallel()
	srv := s3test.New(t)
	srv.MovedTo = "eu-central-1"
	got, err := Check(t.Context(), s3Request(srv, true))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Passed || got.Region != "eu-central-1" {
		t.Errorf("a bucket in eu-central-1 checks as %+v", got)
	}
}

// Main reads its request on its input and writes its result, nothing else: no
// secret of the request comes back, whatever happened.
func TestMainSaysOnlyItsResult(t *testing.T) {
	t.Parallel()
	srv := s3test.New(t)
	srv.Refuse = "SignatureDoesNotMatch"
	in, err := json.Marshal(s3Request(srv, false))
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := Main(bytes.NewReader(in), &out, &errs); code != 0 {
		t.Fatalf("Main = %d: %s", code, errs.String())
	}
	var got Result
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Code != "SignatureDoesNotMatch" {
		t.Fatalf("Main wrote %q, %v", out.String(), err)
	}
	for _, secret := range []string{"the-secret", "AKIDEXAMPLE"} {
		if strings.Contains(out.String()+errs.String(), secret) {
			t.Errorf("Main said %s", secret)
		}
	}
	if code := Main(strings.NewReader("{"), &out, &errs); code != 1 {
		t.Errorf("Main of a broken request = %d", code)
	}
}

// What the probe printed reaches the agent's log without a key in any form it may
// be written in: as it is, in a URL, in base64, escaped in JSON, or the start of
// one the cap cut short.
func TestTheLogHoldsNoFormOfAKey(t *testing.T) {
	t.Parallel()
	secret := `wJalr/XUtnFEMI+K7MDENG/bPx"RfiCYEXAMPLEKEY`
	req := Request{Kind: KindS3, S3: &S3{AccessKey: "AKIDEXAMPLE", SecretKey: secret}}
	escaped, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	for what, form := range map[string]string{
		"as it is":         secret,
		"in a query":       url.QueryEscape(secret),
		"in a path":        url.PathEscape(secret),
		"in JSON":          strings.Trim(string(escaped), `"`),
		"in base64":        base64.StdEncoding.EncodeToString([]byte(secret)),
		"in raw base64":    base64.RawStdEncoding.EncodeToString([]byte(secret)),
		"in URL base64":    base64.URLEncoding.EncodeToString([]byte(secret)),
		"cut short":        secret[:len(secret)-9],
		"cut in its query": url.QueryEscape(secret)[:20],
	} {
		got := withoutKeys("the check signed with "+form, req)
		if strings.Contains(got, secret[:minKeyCut]) || !strings.Contains(got, "[key]") {
			t.Errorf("%s, the log says %q", what, got)
		}
	}
	if got := withoutKeys("the probe doesn't check AKIDEXAMPLE", req); got != "the probe doesn't check [key]" {
		t.Errorf("the access key, the log says %q", got)
	}
	if got := withoutKeys("a request it can't read", req); got != "a request it can't read" {
		t.Errorf("with no key in it, the log says %q", got)
	}
}

// A check on a schedule reads the bucket and writes nothing (ADR 0099, #799): one
// signed list of at most one key, GET alone, whatever it finds.
func TestAReadOnScheduleWritesNothing(t *testing.T) {
	t.Parallel()
	srv := s3test.New(t)
	srv.Set("/uploads/outline/a.png", []byte("a"))
	req := s3Request(srv, true)
	req.Kind = KindS3Read
	got, err := Check(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Passed || got.Step != StepList || got.Code != "" {
		t.Errorf("a bucket that answers its list reads as %+v", got)
	}
	if !slices.Equal(srv.Methods, []string{"GET"}) {
		t.Errorf("the read sent %v, want one GET", srv.Methods)
	}
	if left := srv.Objects(); len(left) != 1 {
		t.Errorf("the read changed the bucket to %v", left)
	}
}

// What each code a provider answers a list with means for a check on a schedule,
// as R2, B2, GCS and AWS answer it: AccessDenied on both the list and the one object
// is unconfirmed, as only a write could say more; a refused key or
// signature is Keys refused, R2's Unauthorized among them; a missing bucket and the
// rest fail with the provider's code.
func TestAReadSaysWhatTheProviderSaid(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		code   string
		status int
		passed bool
	}{
		{"AccessDenied", 403, false},          // every provider: a key that may do neither, unconfirmed
		{"InvalidAccessKeyId", 403, false},    // AWS, GCS and B2's unknown key
		{"Unauthorized", 401, false},          // R2's unknown key (its 10002)
		{"SignatureDoesNotMatch", 403, false}, // a wrong secret, everywhere
		{"NoSuchBucket", 404, false},          // a missing bucket, everywhere
		{"SlowDown", 503, false},              // AWS and GCS asking for fewer
		{"TooManyRequests", 429, false},       // R2 asking for fewer
		{"InternalError", 500, false},
	} {
		srv := s3test.New(t)
		srv.Refuse, srv.RefuseStatus = c.code, c.status
		req := s3Request(srv, false)
		req.Kind = KindS3Read
		got, err := Check(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got.Passed != c.passed || got.Step != StepList || got.Code != c.code || got.Status != c.status {
			t.Errorf("%s reads as %+v, want passed %v at %s", c.code, got, c.passed, StepList)
		}
		// AccessDenied asks once more, for an object that isn't there: a GET too.
		want := []string{"GET"}
		if c.code == "AccessDenied" {
			want = []string{"GET", "GET"}
		}
		if !slices.Equal(srv.Methods, want) {
			t.Errorf("%s: the read sent %v, want %v", c.code, srv.Methods, want)
		}
	}
	// A bucket in another region is read there, once more, as the full check does.
	srv := s3test.New(t)
	srv.MovedTo = "eu-central-1"
	req := s3Request(srv, false)
	req.Kind = KindS3Read
	if got, _ := Check(t.Context(), req); !got.Passed || got.Region != "eu-central-1" {
		t.Errorf("a bucket in eu-central-1 reads as %+v", got)
	}
}

// A key that may not list is asked for one object that isn't there, a GET that
// writes nothing (#799's review, M3): NoSuchKey says it may read, and passes; an
// AccessDenied on that too stays unconfirmed, at "list" with its code.
func TestAReadThatMayNotListAsksForOneObject(t *testing.T) {
	t.Parallel()
	srv := s3test.New(t)
	srv.Refuse = "AccessDenied"
	srv.RefuseQuery = "list-type"
	req := s3Request(srv, false)
	req.Kind = KindS3Read
	got, err := Check(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Passed || got.Step != StepList || got.Code != "NoSuchKey" || got.Status != 404 {
		t.Errorf("a key that may read but not list reads as %+v", got)
	}
	if !slices.Equal(srv.Methods, []string{"GET", "GET"}) || len(srv.Objects()) != 0 {
		t.Errorf("it sent %v and left %v, want two GETs and nothing", srv.Methods, srv.Objects())
	}
}
