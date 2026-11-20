// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/backup/s3"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

func TestDestinationFromVirtualHost(t *testing.T) {
	if got := DestinationFrom(nil); got != nil {
		t.Errorf("nil map = %v, want nil", got)
	}
	pathStyle := DestinationFrom(map[string]string{
		"endpoint": "https://s3.example.com", "region": "us-east-1", "bucket": "backups",
		"access_key": "AKIAEXAMPLE", "secret_key": "the-secret", "passphrase": "a-long-passphrase-value",
	})
	if pathStyle.VirtualHost {
		t.Error("unset virtual_host set VirtualHost=true")
	}
	if b := pathStyle.bucket(&Runner{}); b.VirtualHost {
		t.Error("path-style bucket has VirtualHost=true")
	}
	vh := DestinationFrom(map[string]string{
		"endpoint": "https://s3.example.com", "region": "us-east-1", "bucket": "backups",
		"access_key": "AKIAEXAMPLE", "secret_key": "the-secret", "passphrase": "a-long-passphrase-value",
		"virtual_host": "true",
	})
	if !vh.VirtualHost {
		t.Error("virtual_host=true left VirtualHost=false")
	}
	if b := vh.bucket(&Runner{}); !b.VirtualHost {
		t.Error("virtual-host bucket has VirtualHost=false")
	}
}

// H4: an S3 provider whose error message echoes the access key ID must have
// the key ID redacted before the error leaves internal/backup, while keeping
// the provider error code and HTTP status.
func TestS3ErrorRedactsAccessKeyID(t *testing.T) {
	const (
		keyID  = "AKIAECHOEDKEY99"
		secret = "super-secret-value"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>InvalidAccessKeyId</Code><Message>The AWS Access Key Id %s you provided does not exist (%s)</Message></Error>`, keyID, secret)
	}))
	defer srv.Close()

	dir := t.TempDir()
	r := &Runner{Root: dir, HTTP: srv.Client(), Work: 1}
	vref := resource.Ref{Kind: volume.Kind, Scope: "env_test", Name: "data"}
	bdir := r.dir(vref)
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bdir, "bkp_1.json"), []byte(`{"id":"bkp_1","environment":"env_test","volume":"data","format":"tar.gz","sha256":"abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bdir, "bkp_1.tar.gz"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Destination{
		Endpoint: srv.URL, Region: "auto", Bucket: "backups",
		AccessKey: keyID, SecretKey: secret, Passphrase: "a-long-passphrase-value",
	}

	checkErr := func(label string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected error, got nil", label)
		}
		if strings.Contains(err.Error(), keyID) || strings.Contains(err.Error(), secret) {
			t.Errorf("%s: leaked credential in error: %v", label, err)
		}
		var s3err *s3.Error
		if !errors.As(err, &s3err) {
			t.Fatalf("%s: expected *s3.Error, got %T (%v)", label, err, err)
		}
		if s3err.Code != "InvalidAccessKeyId" || s3err.Status != http.StatusForbidden {
			t.Errorf("%s: s3.Error = %+v, want InvalidAccessKeyId / 403", label, s3err)
		}
	}

	_, sendErr := r.Send(t.Context(), vref, "bkp_1", d)
	checkErr("Send", sendErr)

	_, fetchErr := r.Fetch(t.Context(), vref, "bkp_2", "env_test/data/bkp_2.tar.gz.sealed", "deadbeef", d)
	checkErr("Fetch", fetchErr)

	delErr := r.deleteRemote(t.Context(), "env_test/data/bkp_1.tar.gz.sealed", d)
	checkErr("deleteRemote", delErr)

	// N5: redaction preserves the agent's own operation context around the provider error.
	wrapped := fmt.Errorf("can't send part 2 of 4: %w", &s3.Error{
		Status: http.StatusForbidden, Code: "InvalidAccessKeyId",
		Message: fmt.Sprintf("Key %s rejected (%s)", keyID, secret),
	})
	redacted := refused(wrapped, d)
	checkErr("wrapped multipart", redacted)
	if !strings.Contains(redacted.Error(), "can't send part 2 of 4") {
		t.Errorf("redacted multipart error lost operation context: %v", redacted)
	}

	// End-to-end multipart part failure keeps "can't send part 1 of 1" while redacting the key.
	partSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && req.URL.Query().Has("uploads") {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<InitiateMultipartUploadResult><UploadId>up-1</UploadId></InitiateMultipartUploadResult>`))
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>InvalidAccessKeyId</Code><Message>Part rejected for %s</Message></Error>`, keyID)
	}))
	defer partSrv.Close()
	rPart := &Runner{Root: dir, HTTP: partSrv.Client(), Work: 1}
	dPart := *d
	dPart.Endpoint = partSrv.URL
	_, partErr := rPart.Send(t.Context(), vref, "bkp_1", &dPart)
	checkErr("multipart Send part 1", partErr)
	if !strings.Contains(partErr.Error(), "can't send part 1 of 1") {
		t.Errorf("multipart Send error lost operation context: %v", partErr)
	}
}

// N4: the default backup HTTP client follows same-origin, same-scheme redirects
// (preserving established inline destination behavior, such as a 307 on HEAD/GET)
// and refuses downgrades or cross-origin redirects.
func TestBackupRedirectPolicy(t *testing.T) {
	// Same-origin 307 redirect on HEAD (Size) returns 32 bytes.
	sameSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/redirected/") {
			w.Header().Set("Content-Length", "32")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, req, "/redirected"+req.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer sameSrv.Close()

	d := &Destination{
		Endpoint: sameSrv.URL, Region: "auto", Bucket: "backups",
		AccessKey: "AKIA1", SecretKey: "secret-1",
	}
	got, err := d.bucket(&Runner{}).Size(t.Context(), "env/data/bkp.sealed")
	if err != nil || got != 32 {
		t.Fatalf("same-origin 307 Size = %d, %v; want 32, nil", got, err)
	}

	// Cross-origin 307 redirect is refused without contacting the second origin.
	var secondHits int
	otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits++
		w.Header().Set("Content-Length", "32")
		w.WriteHeader(http.StatusOK)
	}))
	defer otherSrv.Close()

	crossSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, otherSrv.URL+req.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer crossSrv.Close()

	dCross := *d
	dCross.Endpoint = crossSrv.URL
	if _, err := dCross.bucket(&Runner{}).Size(t.Context(), "env/data/bkp.sealed"); err == nil {
		t.Fatal("cross-origin 307 redirect succeeded, want refusal")
	}
	if secondHits != 0 {
		t.Fatalf("cross-origin redirect contacted target origin %d times", secondHits)
	}

	// Scheme downgrade (https -> http) on the same host is refused.
	origURL, _ := url.Parse("https://s3.example.com/backups/obj")
	downURL, _ := url.Parse("http://s3.example.com/backups/obj")
	if err := checkBackupRedirect(&http.Request{URL: downURL}, []*http.Request{{URL: origURL}}); err == nil {
		t.Fatal("https -> http downgrade redirect was allowed, want refusal")
	}
	samePortURL, _ := url.Parse("https://s3.example.com:443/other/obj")
	if err := checkBackupRedirect(&http.Request{URL: samePortURL}, []*http.Request{{URL: origURL}}); err != nil {
		t.Fatalf("same-scheme default-port redirect refused: %v", err)
	}
}
