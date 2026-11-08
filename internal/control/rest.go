// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"connectrpc.com/vanguard"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"github.com/dockwell/dockwell/internal/control/apidoc"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/serve"
)

// restHandler serves the REST paths the schema's google.api.http rules
// give each method (docs/adr/0015-rest-and-openapi-from-the-schema.md).
// Vanguard turns each REST call into a Connect call to the same handler,
// so Guard, Interceptor, the rate limits and the audit log treat it as
// they treat any other call. Before that, admit checks who makes the call
// and their role, so a caller who can't make it learns that, and not what's
// wrong with a query or body Vanguard would read first.
func restHandler(services []*vanguard.Service, admit func(ctx context.Context, procedure string) error) (http.Handler, error) {
	transcoder, err := vanguard.NewTranscoder(services,
		vanguard.WithDefaultServiceOptions(vanguard.WithMaxMessageBufferBytes(serve.MaxMessageBytes)),
		vanguard.WithUnknownHandler(http.HandlerFunc(restNotFound)),
	)
	if err != nil {
		return nil, fmt.Errorf("can't serve the REST paths: %w", err)
	}
	routes := newRESTRoutes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(serve.WithProtocol(r.Context(), "rest"))
		if procedure := routes.match(r.Method, r.URL.Path); procedure != "" {
			if err := admit(r.Context(), procedure); err != nil {
				restRefusal(w, err)
				return
			}
		}
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

// restRefusal answers a REST call admit refused, as Vanguard answers an error
// from the handler: the HTTP status for its code, its metadata as headers,
// and a google.rpc.Status.
func restRefusal(w http.ResponseWriter, err error) {
	cerr := new(connect.Error)
	if !errors.As(err, &cerr) {
		cerr = connect.NewError(connect.CodeInternal, err)
	}
	for k, v := range cerr.Meta() {
		w.Header()[k] = v
	}
	authapi.RESTError(w, restStatus(cerr.Code()), cerr.Code(), "", cerr.Message())
}

// restStatus is the HTTP status for code, as google.rpc maps them and
// Vanguard answers them.
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

// restRoute is one REST path: an HTTP method, and a path template's
// segments, where "" stands for a {field}, which matches any one segment.
type restRoute struct {
	method    string
	segments  []string
	custom    string // after the last segment's colon, such as "setTimeZone"
	procedure string
}

type restRoutes []restRoute

// newRESTRoutes reads every method's google.api.http rule in dockwell.v1,
// as Vanguard does. The schema's paths use only whole {field} segments
// (TestEveryMethodHasARESTPath).
func newRESTRoutes() restRoutes {
	var routes restRoutes
	protoregistry.GlobalFiles.RangeFilesByPackage("dockwell.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				md := sd.Methods().Get(j)
				rule, _ := proto.GetExtension(md.Options(), annotations.E_Http).(*annotations.HttpRule)
				if rule == nil {
					continue
				}
				method, path := restRule(rule)
				segments, custom := splitRESTPath(path)
				for k, seg := range segments {
					if strings.HasPrefix(seg, "{") {
						segments[k] = ""
					}
				}
				routes = append(routes, restRoute{method, segments, custom, "/" + string(sd.FullName()) + "/" + string(md.Name())})
			}
		}
		return true
	})
	return routes
}

// match is the procedure for a REST call, or "" when no rule matches it, which
// Vanguard then answers.
func (routes restRoutes) match(method, path string) string {
	segments, custom := splitRESTPath(path)
next:
	for _, r := range routes {
		if r.method != method || r.custom != custom || len(r.segments) != len(segments) {
			continue
		}
		for k, seg := range r.segments {
			if seg != "" && seg != segments[k] || seg == "" && segments[k] == "" {
				continue next
			}
		}
		return r.procedure
	}
	return ""
}

// splitRESTPath splits a path, or a template, into its segments and what
// follows a colon in the last.
func splitRESTPath(path string) ([]string, string) {
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	last := segments[len(segments)-1]
	if i := strings.LastIndexByte(last, ':'); i >= 0 && !strings.HasSuffix(last, "}") {
		segments[len(segments)-1] = last[:i]
		return segments, last[i+1:]
	}
	return segments, ""
}

func restRule(r *annotations.HttpRule) (method, path string) {
	switch p := r.GetPattern().(type) {
	case *annotations.HttpRule_Get:
		return http.MethodGet, p.Get
	case *annotations.HttpRule_Post:
		return http.MethodPost, p.Post
	case *annotations.HttpRule_Put:
		return http.MethodPut, p.Put
	case *annotations.HttpRule_Patch:
		return http.MethodPatch, p.Patch
	case *annotations.HttpRule_Delete:
		return http.MethodDelete, p.Delete
	case *annotations.HttpRule_Custom:
		return p.Custom.GetKind(), p.Custom.GetPath()
	}
	return "", ""
}
