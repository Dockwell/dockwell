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

// Deploy says which revision and change a step carries out, for what the step
// makes to carry, such as a container's labels, and whether the change rolls
// a failed one back.
type Deploy struct {
	Revision int
	Change   string
	Rollback bool
	// Run says, as a plan judges it, that someone asked for this resource to run, as
	// a deploy or a rollback does, and it isn't only part of a file (Desired.Run).
	Run bool
}

type deployKey struct{}

// WithDeploy tells the driver of a step, through ctx, which deploy it is part
// of. Over the agent stream it travels with the call.
func WithDeploy(ctx context.Context, d Deploy) context.Context {
	return context.WithValue(ctx, deployKey{}, d)
}

// DeployOf is the deploy a step is part of, and zero outside one.
func DeployOf(ctx context.Context) Deploy {
	d, _ := ctx.Value(deployKey{}).(Deploy)
	return d
}

type scopeNamesKey struct{}

// WithScopeNames tells drivers, through ctx, what people call scopes, by ID,
// such as "default.production" for an environment, for names that people read
// on the server: a service's containers take it (ADR 0035). Over the agent
// stream it travels with the call, as the agent has no store to look in.
func WithScopeNames(ctx context.Context, names map[string]string) context.Context {
	return context.WithValue(ctx, scopeNamesKey{}, names)
}

// ScopeName is what people call scope, as WithScopeNames said, or "" when it
// didn't.
func ScopeName(ctx context.Context, scope string) string {
	names, _ := ctx.Value(scopeNamesKey{}).(map[string]string)
	return names[scope]
}
