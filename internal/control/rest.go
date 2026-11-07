// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
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
// they treat any other call.
//
// Who's calling is checked before Vanguard reads the call, so a caller who may
// not make it hears that, 401 or needs_session, and not what's wrong with a
// path variable it couldn't read, such as a PID that isn't a number: a path
// says nothing to whoever may not call it. authorize is the check Interceptor
// makes, which it makes again once Vanguard has read the call.
func restHandler(services []*vanguard.Service, authorize func(ctx context.Context, procedure string) error) (http.Handler, error) {
	transcoder, err := vanguard.NewTranscoder(services,
		vanguard.WithDefaultServiceOptions(vanguard.WithMaxMessageBufferBytes(serve.MaxMessageBytes)),
		vanguard.WithUnknownHandler(http.HandlerFunc(restNotFound)),
	)
	if err != nil {
		return nil, fmt.Errorf("can't serve the REST paths: %w", err)
	}
	routes := restRoutes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(serve.WithProtocol(r.Context(), "rest"))
		if procedure, ok := routes.procedureOf(r.Method, r.URL.Path); ok && authorize != nil {
			if err := authorize(r.Context(), procedure); err != nil {
				restRefuse(w, err)
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

// restRefuse answers a REST call its caller may not make, as Vanguard answers
// the same refusal from Interceptor.
func restRefuse(w http.ResponseWriter, err error) {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		cerr = connect.NewError(connect.CodeInternal, err)
	}
	status := http.StatusInternalServerError
	switch cerr.Code() {
	case connect.CodeUnauthenticated:
		status = http.StatusUnauthorized
	case connect.CodePermissionDenied:
		status = http.StatusForbidden
	case connect.CodeUnavailable:
		status = http.StatusServiceUnavailable
	case connect.CodeResourceExhausted:
		status = http.StatusTooManyRequests
	case connect.CodeCanceled:
		status = 499 // as Vanguard answers a call the client gave up on
	default:
		// What the check refuses with is one of the above; anything else failed.
	}
	for _, key := range []string{"Retry-After"} {
		if v := cerr.Meta().Get(key); v != "" {
			w.Header().Set(key, v)
		}
	}
	authapi.RESTError(w, status, cerr.Code(), cerr.Meta().Get(authapi.ReasonKey), cerr.Message())
}

// restRoute is a REST path a method of dockwell.v1 has, as its google.api.http
// rule says it: literal segments, a variable where a segment is empty, and the
// custom verb after the last ":", if any.
type restRoute struct {
	method    string
	segments  []string
	verb      string
	procedure string
}

type restRouteList []restRoute

// restRoutes reads every REST path from the schema, the one Vanguard serves,
// those with more literal segments first, so a call matches what Vanguard
// matches.
func restRoutes() restRouteList {
	var out restRouteList
	protoregistry.GlobalFiles.RangeFilesByPackage("dockwell.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				md := sd.Methods().Get(j)
				rule, _ := proto.GetExtension(md.Options(), annotations.E_Http).(*annotations.HttpRule)
				if rule == nil {
					continue
				}
				procedure := "/" + string(sd.FullName()) + "/" + string(md.Name())
				for _, r := range append([]*annotations.HttpRule{rule}, rule.GetAdditionalBindings()...) {
					if route, ok := parseRoute(r, procedure); ok {
						out = append(out, route)
					}
				}
			}
		}
		return true
	})
	literals := func(r restRoute) int {
		n := 0
		for _, s := range r.segments {
			if s != "" {
				n++
			}
		}
		return n
	}
	slices.SortStableFunc(out, func(a, b restRoute) int {
		return cmp.Or(cmp.Compare(len(b.verb), len(a.verb)), cmp.Compare(literals(b), literals(a)))
	})
	return out
}

func parseRoute(rule *annotations.HttpRule, procedure string) (restRoute, bool) {
	var method, path string
	switch p := rule.GetPattern().(type) {
	case *annotations.HttpRule_Get:
		method, path = http.MethodGet, p.Get
	case *annotations.HttpRule_Post:
		method, path = http.MethodPost, p.Post
	case *annotations.HttpRule_Put:
		method, path = http.MethodPut, p.Put
	case *annotations.HttpRule_Patch:
		method, path = http.MethodPatch, p.Patch
	case *annotations.HttpRule_Delete:
		method, path = http.MethodDelete, p.Delete
	default:
		return restRoute{}, false
	}
	r := restRoute{method: method, procedure: procedure}
	if i := strings.LastIndexByte(path, ':'); i > strings.LastIndexByte(path, '}') {
		path, r.verb = path[:i], path[i+1:]
	}
	for seg := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		switch {
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") && !strings.ContainsAny(seg, "=*"):
			r.segments = append(r.segments, "")
		case strings.ContainsAny(seg, "{}*="):
			// A pattern this doesn't read, which no rule has: Vanguard alone routes it.
			return restRoute{}, false
		default:
			r.segments = append(r.segments, seg)
		}
	}
	return r, true
}

// procedureOf is the method a REST call is for, by its HTTP method and path, or
// false for one no rule names, which is Vanguard's to answer.
func (routes restRouteList) procedureOf(method, path string) (string, bool) {
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, r := range routes {
		if r.method != method || len(r.segments) != len(segments) {
			continue
		}
		last := segments[len(segments)-1]
		if r.verb != "" {
			rest, ok := strings.CutSuffix(last, ":"+r.verb)
			if !ok {
				continue
			}
			last = rest
		}
		match := true
		for i, want := range r.segments {
			got := segments[i]
			if i == len(segments)-1 {
				got = last
			}
			if (want == "" && got == "") || (want != "" && got != want) {
				match = false
				break
			}
		}
		if match {
			return r.procedure, true
		}
	}
	return "", false
}
