// SPDX-License-Identifier: AGPL-3.0-only

// Package s3 is what the agent needs of S3-compatible storage: put a large object in
// parts, read one, look at one and delete one, signed with AWS Signature Version 4.
// It speaks what AWS, Cloudflare R2, Backblaze B2, MinIO and the like all speak. It's small and
// has no SDK under it, as everything the agent build links costs every server memory
// (ADR 0023).
//
// Nothing here logs, and an error never holds a key or a signature.
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Bucket is where objects go.
type Bucket struct {
	// Endpoint is the service's address, such as
	// https://<account>.r2.cloudflarestorage.com. Plain HTTP is for a test.
	Endpoint string
	// Region is the bucket's region: "auto" unless set.
	Region string
	// Name is the bucket's name.
	Name string
	// AccessKey and SecretKey sign each request.
	AccessKey, SecretKey string
	// Client makes the requests; http.DefaultClient unless set. A test of a form
	// sets a guarded one (netguard), which follows no redirect.
	Client Doer
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Stall is how long a request can go with no byte moving before it's given up on.
	// Zero means StallWait.
	Stall time.Duration
}

// StallWait is how long storage that has stopped answering gets. A large upload takes
// as long as it takes, so this counts from the last byte, not from the start.
const StallWait = 2 * time.Minute

// errStalled ends a request nothing moved on for too long.
var errStalled = errors.New("nothing moved for too long")

func (b *Bucket) stall() time.Duration {
	if b.Stall > 0 {
		return b.Stall
	}
	return StallWait
}

// watched is a body that keeps its request alive while its bytes move.
type watched struct {
	r     io.Reader
	timer *time.Timer
	wait  time.Duration
}

func (w *watched) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if n > 0 {
		w.timer.Reset(w.wait)
	}
	return n, err
}

// watchedBody is an answer's body, which keeps its request alive the same way, and
// ends with it.
type watchedBody struct {
	watched
	io.Closer
	ctx  context.Context //nolint:containedctx // says why a read ended, for the length of a request
	host string
	done func()
}

func (w *watchedBody) Read(p []byte) (int, error) {
	n, err := w.watched.Read(p)
	if err != nil && errors.Is(context.Cause(w.ctx), errStalled) {
		return n, stalledAt(w.host, w.wait)
	}
	return n, err
}

func (w *watchedBody) Close() error {
	w.done()
	return w.Closer.Close()
}

func stalledAt(host string, wait time.Duration) error {
	return fmt.Errorf("the storage at %s stopped answering: nothing moved for %s. Check that it's up, and that this server can reach it", host, wait)
}

// unsignedPayload says the body isn't part of the signature: TLS covers it on the way,
// a sealed file's chunks each authenticate themselves, and hashing a large part twice
// would cost a read of it.
const unsignedPayload = "UNSIGNED-PAYLOAD"

// emptyHash is the SHA-256 of nothing, for a request with no body.
const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Doer sends a request: an *http.Client, or a guarded client such as netguard's.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Error is what the service said. Any answer but a 2xx is one: a 3xx too, such as
// the 301 S3 answers for a bucket in another region, which carries no Location.
type Error struct {
	Status  int
	Code    string
	Message string
	// Region is where the bucket is, when the service says so (x-amz-bucket-region).
	Region string
}

func (e *Error) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return e.Code + ": " + e.Message
	case e.Code != "":
		return e.Code
	}
	return fmt.Sprintf("the storage answered %d", e.Status)
}

// IsNotFound reports an object that isn't there.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (b *Bucket) client() Doer {
	// A nil *http.Client, as a caller with none passes, is unset too, though as a Doer it isn't nil.
	if c, isClient := b.Client.(*http.Client); b.Client == nil || isClient && c == nil {
		return http.DefaultClient
	}
	return b.Client
}

func (b *Bucket) region() string {
	if b.Region != "" {
		return b.Region
	}
	return "auto"
}

func (b *Bucket) now() time.Time {
	if b.Now != nil {
		return b.Now().UTC()
	}
	return time.Now().UTC()
}

// escapePath escapes an object's key as Signature Version 4 has it: each segment,
// with the slashes between them left.
func escapePath(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = escape(p)
	}
	return strings.Join(parts, "/")
}

// escape is AWS's URI encoding: everything but letters, digits and -._~.
func escape(s string) string {
	var out strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.', c == '_', c == '~':
			out.WriteByte(c)
		default:
			fmt.Fprintf(&out, "%%%02X", c)
		}
	}
	return out.String()
}

// canonicalQuery sorts a query and escapes it as a signature has it.
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range q[k] {
			parts = append(parts, escape(k)+"="+escape(v))
		}
	}
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// sign adds what Signature Version 4 asks of a request.
func (b *Bucket) sign(req *http.Request, payload string) {
	SignV4(req, payload, b.AccessKey, b.SecretKey, b.region(), "s3", b.now())
}

// SignV4 adds what AWS Signature Version 4 asks of a request to service in region,
// as of now, with payload the SHA-256 of its body in hex.
func SignV4(req *http.Request, payload, accessKey, secretKey, region, service string, now time.Time) {
	now = now.UTC()
	stamp, day := now.Format("20060102T150405Z"), now.Format("20060102")
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	host := req.URL.Host
	headers := map[string]string{"host": host, "x-amz-content-sha256": payload, "x-amz-date": stamp}
	names := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	var canonical strings.Builder
	for _, n := range names {
		canonical.WriteString(n + ":" + headers[n] + "\n")
	}
	signed := strings.Join(names, ";")
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	request := strings.Join([]string{
		req.Method, path, canonicalQuery(req.URL.Query()), canonical.String(), signed, payload,
	}, "\n")
	sum := sha256.Sum256([]byte(request))
	scope := strings.Join([]string{day, region, service, "aws4_request"}, "/")
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", stamp, scope, hex.EncodeToString(sum[:])}, "\n")
	key := hmacSHA256([]byte("AWS4"+secretKey), day)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signed, hex.EncodeToString(hmacSHA256(key, toSign))))
}

// do sends a signed request to an object, and returns the answer of one that went
// well. The caller closes its body.
func (b *Bucket) do(ctx context.Context, method, key string, q url.Values, body io.Reader, length int64, payload string) (*http.Response, error) {
	u, err := url.Parse(strings.TrimRight(b.Endpoint, "/"))
	if err != nil {
		return nil, fmt.Errorf("the storage's address doesn't read as a URL: %w", err)
	}
	// No bucket is the service itself, as listing the buckets asks it.
	if b.Name == "" {
		u.RawPath = u.EscapedPath() + "/"
	} else {
		u.RawPath = u.EscapedPath() + "/" + escape(b.Name) + "/" + escapePath(key)
	}
	if u.Path, err = url.PathUnescape(u.RawPath); err != nil {
		return nil, err
	}
	u.RawQuery = canonicalQuery(q)
	ctx, cancel := context.WithCancelCause(ctx)
	wait := b.stall()
	timer := time.AfterFunc(wait, func() { cancel(errStalled) })
	ended := func() {
		timer.Stop()
		cancel(nil)
	}
	if body != nil {
		body = &watched{r: body, timer: timer, wait: wait}
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		ended()
		return nil, err
	}
	if body != nil {
		req.ContentLength = length
	}
	b.sign(req, payload)
	res, err := b.client().Do(req)
	if err != nil {
		stalled := errors.Is(context.Cause(ctx), errStalled)
		ended()
		if stalled {
			return nil, stalledAt(u.Host, wait)
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			// A URL error says the address, which is fine; a signature is in a
			// header, and never in one.
			return nil, fmt.Errorf("can't reach the storage at %s: %w. Check the destination's endpoint, and that this server can reach it", u.Host, ue.Err)
		}
		return nil, err
	}
	timer.Reset(wait)
	if res.StatusCode >= http.StatusMultipleChoices {
		defer ended()
		defer res.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 16<<10))
		return nil, &Error{
			Status: res.StatusCode, Code: tag(raw, "Code"), Message: tag(raw, "Message"),
			Region: res.Header.Get("x-amz-bucket-region"),
		}
	}
	res.Body = &watchedBody{watched: watched{r: res.Body, timer: timer, wait: wait}, Closer: res.Body, ctx: ctx, host: u.Host, done: ended}
	return res, nil
}

// Put stores a small object in one request.
func (b *Bucket) Put(ctx context.Context, key string, body []byte) error {
	sum := sha256.Sum256(body)
	res, err := b.do(ctx, http.MethodPut, key, nil, bytes.NewReader(body), int64(len(body)), hex.EncodeToString(sum[:]))
	if err != nil {
		return err
	}
	return res.Body.Close()
}

// ListBuckets names the buckets the keys may see, as the service lists them. A key
// scoped to one bucket, as R2's and B2's often are, may see none and be refused:
// that says nothing of the bucket it's for.
func (b *Bucket) ListBuckets(ctx context.Context) ([]string, error) {
	service := *b
	service.Name = ""
	res, err := service.do(ctx, http.MethodGet, "", nil, nil, 0, emptyHash)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, bucket := range tags(raw, "Bucket") {
		if name := tag([]byte(bucket), "Name"); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// Size is how large an object is.
func (b *Bucket) Size(ctx context.Context, key string) (int64, error) {
	res, err := b.do(ctx, http.MethodHead, key, nil, nil, 0, emptyHash)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	return res.ContentLength, nil
}

// Get reads an object. The caller closes it.
func (b *Bucket) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	res, err := b.do(ctx, http.MethodGet, key, nil, nil, 0, emptyHash)
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// Delete removes an object. One that's gone already is no error, as S3 has it.
func (b *Bucket) Delete(ctx context.Context, key string) error {
	res, err := b.do(ctx, http.MethodDelete, key, nil, nil, 0, emptyHash)
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	return res.Body.Close()
}

// How large an upload's parts are. A service takes at most 10,000 parts of at least
// 5 MiB, so a large file takes larger parts.
const (
	minPart  = 8 << 20
	maxParts = 9000
)

// PartSize is the size of each part of a file of size bytes, as a multiple of unit,
// so a part can be made again from where it starts.
func PartSize(size, unit int64) int64 {
	part := max(int64(minPart), size/maxParts+1)
	if unit > 0 {
		part = (part + unit - 1) / unit * unit
	}
	return part
}

// Part makes one part of an upload, from the start, as often as it's asked: a part
// that fails is sent again.
type Part func() (io.Reader, error)

// Upload stores an object in parts, as many as Parts says, each of length Length(i),
// and removes what it sent if it can't finish, so a failed upload leaves nothing to
// be billed for. progress hears how many bytes have gone, after each part.
type Upload struct {
	Key      string
	Count    int
	Length   func(i int) int64
	Part     func(i int) Part
	Progress func(sent int64)
}

// partRetries is how often a part is sent again.
const partRetries = 3

// tag finds the first element of a name in what S3 said. The agent build doesn't link
// encoding/xml, and S3's few answers are flat enough to need none of it.
func tag(raw []byte, name string) string {
	_, rest, ok := bytes.Cut(raw, []byte("<"+name+">"))
	if !ok {
		return ""
	}
	value, _, ok := bytes.Cut(rest, []byte("</"+name+">"))
	if !ok {
		return ""
	}
	return unescapeXML.Replace(string(value))
}

// tags finds every element of a name, each as it's written, for tag to read inside.
func tags(raw []byte, name string) []string {
	var out []string
	for {
		_, rest, ok := bytes.Cut(raw, []byte("<"+name+">"))
		if !ok {
			return out
		}
		value, after, ok := bytes.Cut(rest, []byte("</"+name+">"))
		if !ok {
			return out
		}
		out = append(out, string(value))
		raw = after
	}
}

var (
	unescapeXML = strings.NewReplacer("&quot;", `"`, "&apos;", "'", "&lt;", "<", "&gt;", ">", "&#34;", `"`, "&#39;", "'", "&amp;", "&")
	escapeXML   = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
)

// finishing is the last request of an upload, naming each part by its ETag.
func finishing(etags []string) []byte {
	var out bytes.Buffer
	out.WriteString("<CompleteMultipartUpload>")
	for i, etag := range etags {
		fmt.Fprintf(&out, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i+1, escapeXML.Replace(etag))
	}
	out.WriteString("</CompleteMultipartUpload>")
	return out.Bytes()
}

// Store runs an upload.
func (b *Bucket) Store(ctx context.Context, up Upload) (err error) {
	res, err := b.do(ctx, http.MethodPost, up.Key, url.Values{"uploads": {""}}, nil, 0, emptyHash)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("the storage didn't start an upload: %w", err)
	}
	upload := tag(raw, "UploadId")
	if upload == "" {
		return errors.New("the storage didn't start an upload: it gave no upload ID")
	}
	defer func() {
		if err != nil {
			// Whatever ended this ended ctx too, perhaps, and this still has to go.
			clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
			defer cancel()
			if r, derr := b.do(clean, http.MethodDelete, up.Key, url.Values{"uploadId": {upload}}, nil, 0, emptyHash); derr == nil {
				_ = r.Body.Close()
			}
		}
	}()
	etags := make([]string, 0, up.Count)
	var sent int64
	for i := range up.Count {
		etag, err := b.sendPart(ctx, up, upload, i)
		if err != nil {
			return fmt.Errorf("can't send part %d of %d: %w", i+1, up.Count, err)
		}
		etags = append(etags, etag)
		sent += up.Length(i)
		if up.Progress != nil {
			up.Progress(sent)
		}
	}
	body := finishing(etags)
	sum := sha256.Sum256(body)
	final, err := b.do(ctx, http.MethodPost, up.Key, url.Values{"uploadId": {upload}}, bytes.NewReader(body), int64(len(body)), hex.EncodeToString(sum[:]))
	if err != nil {
		return fmt.Errorf("can't finish the upload: %w", err)
	}
	defer final.Body.Close()
	// S3 can answer 200 and then say it failed, in the body.
	said, _ := io.ReadAll(io.LimitReader(final.Body, 64<<10))
	if bytes.Contains(said, []byte("<Error>")) {
		return &Error{Status: http.StatusOK, Code: tag(said, "Code"), Message: tag(said, "Message")}
	}
	return nil
}

// sendPart sends one part, and again if that fails on the way.
func (b *Bucket) sendPart(ctx context.Context, up Upload, id string, i int) (string, error) {
	var last error
	for try := range partRetries {
		if try > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(try) * time.Second):
			}
		}
		body, err := up.Part(i)()
		if err != nil {
			return "", err
		}
		q := url.Values{"partNumber": {strconv.Itoa(i + 1)}, "uploadId": {id}}
		res, err := b.do(ctx, http.MethodPut, up.Key, q, body, up.Length(i), unsignedPayload)
		if err == nil {
			_ = res.Body.Close() // all a part's answer says is in its headers
			return res.Header.Get("ETag"), nil
		}
		last = err
		var said *Error
		if errors.As(err, &said) && said.Status < http.StatusInternalServerError {
			return "", err // it won't go better a second time
		}
	}
	return "", last
}
