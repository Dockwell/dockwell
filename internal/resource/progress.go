// SPDX-License-Identifier: AGPL-3.0-only

package resource

import "context"

type progressKey struct{}

// WithProgress returns a context whose step reports how it is going to
// fn, such as the engine updating the step's message, or the agent sending
// it over its stream.
func WithProgress(ctx context.Context, fn func(message string)) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

// ProgressFunc returns what ctx reports progress to, or nil.
func ProgressFunc(ctx context.Context) func(string) {
	fn, _ := ctx.Value(progressKey{}).(func(string))
	return fn
}

// Progress tells whoever follows the step under way how it is going, such
// as "Pulling nginx:1.27: 12 of 48 MB". Nothing hears it when nobody
// follows. A driver's message must never hold a secret.
func Progress(ctx context.Context, message string) {
	if fn := ProgressFunc(ctx); fn != nil {
		fn(message)
	}
}
