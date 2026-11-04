// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"connectrpc.com/vanguard"

	"github.com/dockwell/dockwell/internal/control/apidoc"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/serve"
)

// restHandler serves the REST paths the schema's google.api.http rules
// give each method (docs/adr/0015-rest-and-openapi-from-the-schema.md).
// Vanguard turns each REST call into a Connect call to the same handler,
// so Guard, Interceptor, the rate limits and the audit log treat it as
// they treat any other call.
func restHandler(services []*vanguard.Service) (http.Handler, error) {
	transcoder, err := vanguard.NewTranscoder(services,
		vanguard.WithDefaultServiceOptions(vanguard.WithMaxMessageBufferBytes(serve.MaxMessageBytes)),
		vanguard.WithUnknownHandler(http.HandlerFunc(restNotFound)),
	)
	if err != nil {
		return nil, fmt.Errorf("can't serve the REST paths: %w", err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(serve.WithProtocol(r.Context(), "rest"))
		// Only POSTs take a body (TestEveryMethodHasARESTPath). One with no
		// body is a request with no fields, as curl -X POST sends it, with
		// or without -d '' and its form content type, or chunked; Connect
		// wants an empty JSON object.
		if r.Method == http.MethodPost && emptyBody(r) {
			r.Header = r.Header.Clone()
			r.Header.Set("Content-Type", "application/json")
			r.Header.Del("Content-Encoding")
			r.Body, r.ContentLength = io.NopCloser(strings.NewReader("{}")), 2
		}
		transcoder.ServeHTTP(w, r)
	}), nil
}

// emptyBody reports whether r's body is empty. When its length isn't known,
// as with a chunked body, it reads one byte to find out, and puts it back.
func emptyBody(r *http.Request) bool {
	switch {
	case r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0:
		return true
	case r.ContentLength > 0:
		return false
	}
	body := bufio.NewReaderSize(r.Body, 16)
	if _, err := body.Peek(1); errors.Is(err, io.EOF) {
		return true
	}
	// Any other error is the body's to report when Vanguard reads it.
	r.Body = struct {
		io.Reader
		io.Closer
	}{body, r.Body}
	return false
}

// restNotFound answers a path under /v1/ that no method has, the way the
// REST paths answer errors: a google.rpc.Status as JSON.
func restNotFound(w http.ResponseWriter, r *http.Request) {
	authapi.RESTError(w, http.StatusNotFound, connect.CodeNotFound, "",
		fmt.Sprintf("no REST path matches %s %s. The paths are listed in %s", r.Method, r.URL.Path, apidoc.Path))
}
