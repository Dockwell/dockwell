// SPDX-License-Identifier: AGPL-3.0-only

package serve

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
)

// logInterceptor records every RPC: successful calls at debug, client errors
// at info, and server faults at error, so a default log shows only what an
// operator should look at. A call its caller left, as a log view does as it
// closes, is no failure: it says so at debug, and one whose handler returns
// the bare context.Canceled answers canceled, which would otherwise be unknown,
// and read as the server's fault. Only the request's own context says the
// caller left: a handler's own deadline or cancel, or a fault that wraps one,
// keeps its level.
type logInterceptor struct {
	logger *slog.Logger
}

var _ connect.Interceptor = logInterceptor{}

func (l logInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		res, err := next(ctx, req)
		err = canceledAs(ctx, err)
		l.log(ctx, req.Spec().Procedure, req.Peer().Protocol, start, err)
		return res, err
	}
}

func (logInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (l logInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		err := canceledAs(ctx, next(ctx, conn))
		l.log(ctx, conn.Spec().Procedure, conn.Peer().Protocol, start, err)
		return err
	}
}

// canceledAs gives an error of a call its caller left that has no code of its
// own, and is context's, the code that says so: canceled, or deadline_exceeded.
// One while the caller is there is the handler's own, and keeps its code.
func canceledAs(ctx context.Context, err error) error {
	if err == nil || ctx.Err() == nil || connect.CodeOf(err) != connect.CodeUnknown {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	return err
}

type protocolKey struct{}

// WithProtocol records the protocol a request arrived with, for one that
// reaches its handler through another: a REST call reaches it through
// Vanguard as Connect. RPC logs name the protocol it arrived with.
func WithProtocol(ctx context.Context, protocol string) context.Context {
	return context.WithValue(ctx, protocolKey{}, protocol)
}

func (l logInterceptor) log(ctx context.Context, procedure, protocol string, start time.Time, err error) {
	if p, ok := ctx.Value(protocolKey{}).(string); ok {
		protocol = p
	}
	attrs := []slog.Attr{
		slog.String("procedure", procedure),
		slog.String("protocol", protocol),
		slog.Duration("duration", time.Since(start)),
	}
	if err == nil {
		l.logger.LogAttrs(ctx, slog.LevelDebug, "rpc", attrs...)
		return
	}
	code := connect.CodeOf(err)
	attrs = append(attrs, slog.String("code", code.String()), slog.String("error", err.Error()))
	if left(ctx, code, err) {
		l.logger.LogAttrs(ctx, slog.LevelDebug, "rpc canceled", attrs...)
		return
	}
	l.logger.LogAttrs(ctx, levelFor(code, err), "rpc failed", attrs...)
}

// left reports a call its caller left, which ended because of that: the
// request's context ended, as when the caller goes away or the server stops,
// and the call ended canceled, or past its deadline, or of context's own. A
// fault of the server's own while the caller happened to leave still counts, and
// so does a handler's own deadline or cancel while it hadn't.
func left(ctx context.Context, code connect.Code, err error) bool {
	if ctx.Err() == nil {
		return false
	}
	return code == connect.CodeCanceled || code == connect.CodeDeadlineExceeded ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func levelFor(code connect.Code, err error) slog.Level {
	switch code {
	case connect.CodeInternal, connect.CodeUnknown, connect.CodeDataLoss, connect.CodeUnimplemented:
		return slog.LevelError
	case connect.CodeCanceled, connect.CodeDeadlineExceeded:
		if errors.Is(err, context.Canceled) {
			return slog.LevelDebug
		}
		return slog.LevelInfo
	default:
		return slog.LevelInfo
	}
}
