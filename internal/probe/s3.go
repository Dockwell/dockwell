// SPDX-License-Identifier: AGPL-3.0-only

package probe

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/backup/s3"
	"github.com/dockwell/dockwell/internal/netguard"
)

// S3 is a bucket to check, and the keys that reach it.
type S3 struct {
	Endpoint string `json:"endpoint"`
	// Region is the bucket's: "auto" unless set.
	Region string `json:"region,omitempty"`
	Bucket string `json:"bucket"`
	// VirtualHost names the bucket in the host, as AWS's newer regions take alone.
	VirtualHost bool `json:"virtual_host,omitempty"`
	// Prefix is where the link's objects go, and the check's with them.
	Prefix    string `json:"prefix,omitempty"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	// Presign asks for a presigned URL fetched from here too, for an app whose
	// need has presigned_urls.
	Presign bool `json:"presign,omitempty"`
}

// The steps an S3 check takes, in order.
const (
	StepWrite   = "write"
	StepRead    = "read"
	StepPresign = "presign"
	StepDelete  = "delete"
	// StepList is a check on a schedule's one read, which writes nothing.
	StepList = "list"
)

// What kept a check from an answer, where the service gave none.
const (
	CodeRefused     = "refused"
	CodeTimeout     = "timeout"
	CodeTLS         = "tls"
	CodeUnreachable = "unreachable"
	// CodeMismatch is an object read back unlike what was written.
	CodeMismatch = "mismatch"
)

// checkBody is what the check writes and reads back.
var checkBody = []byte("Dockwell checks this connection, and deletes this object once it has.\n")

// presignWait is how long the check's presigned URL lasts: a minute, as ADR 0099
// says, which is ample for a URL fetched at once, and short for one that leaked
// through a far end's log.
const presignWait = time.Minute

// checkS3 signs in, puts a small object under the link's prefix and a random name,
// reads it back, fetches it by a presigned URL where asked, and deletes it. A
// region the service says is the bucket's is taken, once, and said.
func checkS3(ctx context.Context, c *S3, guard netguard.Options) Result {
	client := netguard.Client(guard)
	b := &s3.Bucket{
		Endpoint: c.Endpoint, Region: c.Region, Name: c.Bucket, VirtualHost: c.VirtualHost,
		AccessKey: c.AccessKey, SecretKey: c.SecretKey, Client: client, Stall: ConnectWait * 3,
	}
	key := strings.TrimLeft(c.Prefix+".dockwell-check-"+randomHex(), "/")
	out := Result{}
	err := b.Put(ctx, key, checkBody)
	var se *s3.Error
	if errors.As(err, &se) && se.Region != "" && se.Region != b.Region {
		// The bucket is in another region than asked: S3 says which, and the check
		// takes it once, as the person would be told to.
		b.Region, out.Region = se.Region, se.Region
		err = b.Put(ctx, key, checkBody)
	}
	if err != nil {
		return failed(out, StepWrite, err, c)
	}
	// From here, an object is there to delete, whatever else fails.
	deleted := false
	defer func() {
		if !deleted {
			_ = b.Delete(context.WithoutCancel(ctx), key)
		}
	}()
	r, err := b.Get(ctx, key)
	if err != nil {
		return failed(out, StepRead, err, c)
	}
	got, err := io.ReadAll(io.LimitReader(r, int64(len(checkBody))+1))
	_ = r.Close()
	switch {
	case err != nil:
		return failed(out, StepRead, err, c)
	case !bytes.Equal(got, checkBody):
		out.Step, out.Code = StepRead, CodeMismatch
		return out
	}
	if c.Presign {
		if res := fetchPresigned(ctx, b, key, client); res != nil {
			res.Region = out.Region
			return *res
		}
	}
	deleted = true
	if err := b.Delete(ctx, key); err != nil {
		return failed(out, StepDelete, err, c)
	}
	out.Passed = true
	return out
}

// readS3 lists at most one key under the prefix, which writes nothing, for a check
// on a schedule (ADR 0099). AccessDenied passes: the service took the keys and the
// bucket answered, and a key that may only put and get objects may not list. A
// passed read says so in its step and code, as it saw no object.
func readS3(ctx context.Context, c *S3, guard netguard.Options) Result {
	b := &s3.Bucket{
		Endpoint: c.Endpoint, Region: c.Region, Name: c.Bucket, VirtualHost: c.VirtualHost,
		AccessKey: c.AccessKey, SecretKey: c.SecretKey, Client: netguard.Client(guard), Stall: ConnectWait * 3,
	}
	out := Result{}
	err := b.ListOne(ctx, strings.TrimLeft(c.Prefix, "/"))
	var se *s3.Error
	if errors.As(err, &se) && se.Region != "" && se.Region != b.Region {
		b.Region, out.Region = se.Region, se.Region
		err = b.ListOne(ctx, strings.TrimLeft(c.Prefix, "/"))
	}
	if errors.As(err, &se) && se.Code == "AccessDenied" {
		out.Passed, out.Step, out.Code, out.Status = true, StepList, se.Code, se.Status
		return out
	}
	if err != nil {
		return failed(out, StepList, err, c)
	}
	out.Passed, out.Step = true, StepList
	return out
}

// fetchPresigned fetches the object by a presigned URL, as a browser would by one
// the app gave it, or says why it couldn't.
func fetchPresigned(ctx context.Context, b *s3.Bucket, key string, client *netguard.HTTP) *Result {
	u, err := b.Presign(http.MethodGet, key, presignWait)
	if err != nil {
		return &Result{Step: StepPresign, Code: CodeUnreachable, Words: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, http.NoBody)
	if err != nil {
		return &Result{Step: StepPresign, Code: CodeUnreachable, Words: err.Error()}
	}
	res, err := client.Do(req)
	if err != nil {
		r := failed(Result{}, StepPresign, err, nil)
		return &r
	}
	defer res.Body.Close()
	got, _ := io.ReadAll(io.LimitReader(res.Body, 16<<10))
	switch {
	case res.StatusCode != http.StatusOK:
		return &Result{Step: StepPresign, Status: res.StatusCode, Code: tagOf(got, "Code")}
	case !bytes.Equal(got, checkBody):
		return &Result{Step: StepPresign, Code: CodeMismatch}
	}
	return nil
}

// failed is a Result that stopped at step for err: the service's code, with its
// message where that holds nothing of the request, or what kept it from answering.
func failed(out Result, step string, err error, c *S3) Result {
	out.Step = step
	var se *s3.Error
	if errors.As(err, &se) {
		out.Code, out.Status = se.Code, se.Status
		if safeMessage(se, c) {
			out.Message = se.Message
		}
		return out
	}
	out.Code, out.Words = connectCode(err), netguard.Words(err)
	return out
}

// unsafeCodes carry the request in their message: the key's ID, and the string
// signed with the secret.
var unsafeCodes = map[string]bool{"SignatureDoesNotMatch": true, "InvalidAccessKeyId": true}

func safeMessage(e *s3.Error, c *S3) bool {
	if unsafeCodes[e.Code] || len(e.Message) > 200 {
		return false
	}
	return c == nil || c.AccessKey == "" || !strings.Contains(e.Message, c.AccessKey) && (c.SecretKey == "" || !strings.Contains(e.Message, c.SecretKey))
}

// connectCode says what kept a request from an answer.
func connectCode(err error) string {
	var (
		ne   net.Error
		cert *tls.CertificateVerificationError
		ua   x509.UnknownAuthorityError
		host x509.HostnameError
	)
	switch {
	case errors.Is(err, netguard.ErrForbidden):
		return CodeRefused
	case errors.As(err, &cert), errors.As(err, &ua), errors.As(err, &host):
		return CodeTLS
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout(), strings.Contains(err.Error(), "nothing moved"):
		return CodeTimeout
	}
	return CodeUnreachable
}

// tagOf is an XML element's text in an error's body, such as its Code.
func tagOf(raw []byte, name string) string {
	open, end := "<"+name+">", "</"+name+">"
	i := bytes.Index(raw, []byte(open))
	if i < 0 {
		return ""
	}
	rest := raw[i+len(open):]
	j := bytes.Index(rest, []byte(end))
	if j < 0 || j > 128 {
		return ""
	}
	return string(rest[:j])
}

func randomHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
