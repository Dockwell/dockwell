// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
)

// logInterceptor records every RPC: successful calls at debug, client errors
// at info, and server faults at error, so a default log shows only what an
// operator should look at.
type logInterceptor struct {
	logger *slog.Logger
}

var _ connect.Interceptor = logInterceptor{}

func (l logInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		res, err := next(ctx, req)
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
		err := next(ctx, conn)
		l.log(ctx, conn.Spec().Procedure, conn.Peer().Protocol, start, err)
		return err
	}
}

func (l logInterceptor) log(ctx context.Context, procedure, protocol string, start time.Time, err error) {
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
	l.logger.LogAttrs(ctx, levelFor(code, err), "rpc failed", attrs...)
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
