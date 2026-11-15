// SPDX-License-Identifier: AGPL-3.0-only

package probe

import (
	"bytes"
	"encoding/json"
	"net/netip"
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
