// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/files"
	"github.com/dockwell/dockwell/internal/control/authapi"
	filesapi "github.com/dockwell/dockwell/internal/control/files"
	"github.com/dockwell/dockwell/internal/serve"
)

// Uploads and downloads over REST (ADR 0045). Vanguard holds a REST call's body
// whole, both ways, up to serve.MaxMessageBytes, as REST puts the status before the
// body. A file can be any size, so its two REST paths come here instead: each
// becomes the same Connect stream call to the same handler, in the same Guard
// context, so the interceptor, the rate limits and the audit log treat it as they
// treat Vanguard's, and its bytes go through a frame at a time.

const (
	uploadPath   = "PUT /v1/servers/{server_id}/uploads/{upload_id}"
	downloadPath = "GET /v1/servers/{server_id}/files:download"
	// stallStep is how much of a download goes out at a time, each with
	// transferStall to go.
	stallStep = 64 << 10
)

// transferStall is how long an upload's body, or a download's reader, can make no
// progress before the transfer counts as gone. A server runs three at a time, and
// one that stopped would hold its place for hours otherwise, as a browser's pause
// does. A slow link is fine as long as it moves. A test makes it shorter.
var transferStall = 60 * time.Second

// streamTransfers serves the REST paths for uploads and downloads on mux, through
// guard, ahead of Vanguard's, with the FileService handler h.
func streamTransfers(mux *http.ServeMux, h http.Handler, guard func(http.Handler) http.Handler) {
	mux.Handle(uploadPath, guard(uploadREST(h)))
	mux.Handle(downloadPath, guard(downloadREST(h)))
}

// Connect's envelope: a byte of flags, then the message's length.
const (
	envelopeBytes = 5
	flagEnd       = 0b10
	flagCompress  = 0b01
)

// connectCall is r as a Connect stream call to procedure, with body.
func connectCall(r *http.Request, procedure string, body io.Reader) *http.Request {
	call := r.Clone(serve.WithProtocol(r.Context(), "rest"))
	call.Method = http.MethodPost
	call.URL = &url.URL{Path: procedure}
	call.RequestURI = procedure
	call.Header.Set("Content-Type", "application/connect+proto")
	call.Header.Set("Connect-Protocol-Version", "1")
	for _, h := range []string{"Content-Encoding", "Content-Length", "Connect-Content-Encoding", "Connect-Accept-Encoding", "Connect-Timeout-Ms"} {
		call.Header.Del(h)
	}
	call.Body, call.ContentLength = io.NopCloser(body), -1
	return call
}

// uploadREST serves UploadFile's REST path: a PUT whose body is the bytes, from the
// offset its query names. It answers with how far the upload got, as JSON.
func uploadREST(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset, err := strconv.ParseUint(cmp.Or(r.URL.Query().Get("offset"), "0"), 10, 64)
		if err != nil {
			authapi.RESTError(w, http.StatusBadRequest, connect.CodeInvalidArgument, "invalid",
				"The offset has to be a number of bytes, such as 0. BeginUpload says how far an upload got.")
			return
		}
		control := http.NewResponseController(w)
		defer func() { _ = control.SetReadDeadline(time.Time{}) }()
		body := &uploadBody{r: r.Body, contentType: r.Header.Get("Content-Type"), buf: make([]byte, files.FrameBytes), control: control}
		body.frame(&dockwellv1.UploadFileRequest{ServerId: r.PathValue("server_id"), UploadId: r.PathValue("upload_id"), Offset: offset})
		var reply []byte
		out := &connectStream{header: http.Header{}, each: func(msg []byte) error {
			reply = append(reply[:0], msg...)
			return nil
		}}
		h.ServeHTTP(out, connectCall(r, dockwellv1connect.FileServiceUploadFileProcedure, body))
		if out.answeredREST(w) {
			return
		}
		var up dockwellv1.UploadFileResponse
		if err := proto.Unmarshal(reply, &up); err != nil {
			authapi.RESTError(w, http.StatusBadGateway, connect.CodeInternal, "", "The upload answered with what isn't an upload. Ask BeginUpload how far it got.")
			return
		}
		text, _ := protojson.Marshal(&up)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(text)
	})
}

// uploadBody is a PUT's body as Connect messages, a frame of it in each, read as the
// handler reads, so nothing reads the PUT once the handler is done.
type uploadBody struct {
	r           io.Reader
	contentType string
	buf         []byte
	// control gives each frame's read transferStall.
	control *http.ResponseController
	// framed is the frame last made, and pending what of it isn't read yet.
	framed  []byte
	pending []byte
	done    bool
	err     error
}

func (u *uploadBody) frame(m proto.Message) {
	out, _ := proto.MarshalOptions{}.MarshalAppend(append(u.framed[:0], 0, 0, 0, 0, 0), m)
	binary.BigEndian.PutUint32(out[1:envelopeBytes], uint32(len(out)-envelopeBytes)) //nolint:gosec // a frame's length
	u.framed, u.pending = out, out
}

func (u *uploadBody) Read(p []byte) (int, error) {
	for len(u.pending) == 0 {
		if u.done {
			return 0, u.err
		}
		// A connection that takes no deadline has no stalled sender to end.
		_ = u.control.SetReadDeadline(time.Now().Add(transferStall))
		n, err := io.ReadFull(u.r, u.buf)
		if n > 0 {
			u.frame(&dockwellv1.UploadFileRequest{Content: &httpbody.HttpBody{ContentType: u.contentType, Data: u.buf[:n]}})
		}
		if err != nil {
			u.done = true
			u.err = err
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				u.err = io.EOF
			}
		}
	}
	n := copy(p, u.pending)
	u.pending = u.pending[n:]
	return n, nil
}

// downloadREST serves DownloadFile's REST path: a GET whose answer is the file, or
// the folder as a zip, written as each part comes. Before the first part, an error
// is a REST error. After it, the status has gone, so the connection is cut, and a
// browser shows the download failed rather than save part of it as the whole.
func downloadREST(h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		// A GET path takes HEAD too. A download goes on record as it starts, and a
		// HEAD would be one that sent nothing, so it's refused.
		if r.Method != http.MethodGet {
			rw.Header().Set("Allow", http.MethodGet)
			authapi.RESTError(rw, http.StatusMethodNotAllowed, connect.CodeUnimplemented, "",
				"A download is a GET. Its size comes in its answer's Content-Length.")
			return
		}
		w := &stallWriter{ResponseWriter: rw, control: http.NewResponseController(rw)}
		req, _ := proto.Marshal(&dockwellv1.DownloadFileRequest{ServerId: r.PathValue("server_id"), Path: r.URL.Query().Get("path")})
		envelope := binary.BigEndian.AppendUint32([]byte{0}, uint32(len(req))) //nolint:gosec // a request's length
		started := false
		var each dockwellv1.DownloadFileResponse
		out := &connectStream{header: http.Header{}}
		out.each = func(msg []byte) error {
			if err := proto.Unmarshal(msg, &each); err != nil {
				return err
			}
			part := each.GetBody()
			if !started {
				started = true
				startDownload(w, out.header, cmp.Or(part.GetContentType(), "application/octet-stream"))
			}
			if _, err := w.Write(part.GetData()); err != nil {
				return err
			}
			w.Flush()
			return nil
		}
		h.ServeHTTP(out, connectCall(r, dockwellv1connect.FileServiceDownloadFileProcedure, bytes.NewReader(append(envelope, req...))))
		switch {
		case started && (out.failed() || out.end == nil):
			panic(http.ErrAbortHandler)
		case started:
		case out.answeredREST(w):
		default:
			// It ended with nothing in it: an empty file.
			startDownload(w, out.header, "application/octet-stream")
		}
	})
}

// stallWriter gives up on a download whose reader stopped reading: each write has
// transferStall to go, a step at a time.
type stallWriter struct {
	http.ResponseWriter
	control *http.ResponseController
}

func (w *stallWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		n := min(len(p), stallStep)
		_ = w.control.SetWriteDeadline(time.Now().Add(transferStall))
		m, err := w.ResponseWriter.Write(p[:n])
		_ = w.control.SetWriteDeadline(time.Time{})
		written += m
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

// Flush sends what's buffered, within the same limit.
func (w *stallWriter) Flush() {
	_ = w.control.SetWriteDeadline(time.Now().Add(transferStall))
	_ = w.control.Flush()
	_ = w.control.SetWriteDeadline(time.Time{})
}

// Unwrap lets a ResponseController reach what is underneath.
func (w *stallWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// startDownload writes a download's status and headers, from what the handler said.
func startDownload(w http.ResponseWriter, from http.Header, contentType string) {
	for _, h := range []string{"Content-Disposition", "X-Content-Type-Options"} {
		if v := from.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if size := from.Get(filesapi.SizeHeader); size != "" {
		w.Header().Set("Content-Length", size)
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
}

// connectStream takes what a Connect handler writes for a stream call, and passes
// each message to each as it comes, keeping the end.
type connectStream struct {
	header http.Header
	status int
	each   func(msg []byte) error
	// raw is a body Connect wrote with a status other than 200.
	raw     []byte
	pending []byte
	end     *endStream
	err     error
}

// endStream is a Connect stream's last message.
type endStream struct {
	Error *struct {
		Code    connect.Code `json:"code"`
		Message string       `json:"message"`
	} `json:"error"`
	Metadata map[string][]string `json:"metadata"`
}

func (c *connectStream) Header() http.Header { return c.header }

func (c *connectStream) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

// Flush is for Connect, which flushes each message: each writes it on.
func (c *connectStream) Flush() {}

func (c *connectStream) Write(p []byte) (int, error) {
	c.WriteHeader(http.StatusOK)
	if c.status != http.StatusOK {
		c.raw = append(c.raw, p...)
		return len(p), nil
	}
	if c.err != nil {
		return 0, c.err
	}
	c.pending = append(c.pending, p...)
	read := 0
	for len(c.pending)-read >= envelopeBytes {
		flags := c.pending[read]
		size := int(binary.BigEndian.Uint32(c.pending[read+1 : read+envelopeBytes]))
		if size > serve.MaxMessageBytes {
			c.err = fmt.Errorf("a message of %d bytes is more than a stream carries", size)
			return 0, c.err
		}
		if len(c.pending)-read < envelopeBytes+size {
			break
		}
		msg := c.pending[read+envelopeBytes : read+envelopeBytes+size]
		read += envelopeBytes + size
		switch {
		case flags&flagEnd != 0:
			c.end = &endStream{}
			if err := json.Unmarshal(msg, c.end); err != nil {
				c.err = err
				return 0, err
			}
		case flags&flagCompress != 0:
			c.err = errors.New("a compressed message, which this didn't ask for")
			return 0, c.err
		default:
			if err := c.each(msg); err != nil {
				c.err = err
				return 0, err
			}
		}
	}
	c.pending = append(c.pending[:0], c.pending[read:]...)
	return len(p), nil
}

// failed reports a stream that ended in an error, or couldn't be read.
func (c *connectStream) failed() bool {
	return c.err != nil || c.status != http.StatusOK || c.end != nil && c.end.Error != nil
}

// answeredREST writes a stream that failed to w as a REST error, and reports
// whether it did.
func (c *connectStream) answeredREST(w http.ResponseWriter) bool {
	switch {
	case c.status != http.StatusOK && c.status != 0:
		w.Header().Set("Content-Type", c.header.Get("Content-Type"))
		w.WriteHeader(c.status)
		_, _ = w.Write(c.raw)
		return true
	case c.err != nil || c.end == nil:
		authapi.RESTError(w, http.StatusBadGateway, connect.CodeInternal, "", "The call ended without saying how. Try again.")
		return true
	case c.end.Error == nil:
		return false
	}
	reason := ""
	for k, v := range c.end.Metadata {
		if strings.EqualFold(k, authapi.ReasonKey) && len(v) > 0 {
			reason = v[0]
		}
	}
	authapi.RESTError(w, restStatus(c.end.Error.Code), c.end.Error.Code, reason, c.end.Error.Message)
	return true
}

// restStatus is the HTTP status REST answers a code with, as Vanguard does.
func restStatus(code connect.Code) int {
	switch code {
	case connect.CodeCanceled:
		return 499
	case connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodeOutOfRange:
		return http.StatusBadRequest
	case connect.CodeDeadlineExceeded:
		return http.StatusGatewayTimeout
	case connect.CodeNotFound:
		return http.StatusNotFound
	case connect.CodeAlreadyExists, connect.CodeAborted:
		return http.StatusConflict
	case connect.CodePermissionDenied:
		return http.StatusForbidden
	case connect.CodeResourceExhausted:
		return http.StatusTooManyRequests
	case connect.CodeUnimplemented:
		return http.StatusNotImplemented
	case connect.CodeUnavailable:
		return http.StatusServiceUnavailable
	case connect.CodeUnauthenticated:
		return http.StatusUnauthorized
	case connect.CodeUnknown, connect.CodeInternal, connect.CodeDataLoss:
	}
	return http.StatusInternalServerError
}
