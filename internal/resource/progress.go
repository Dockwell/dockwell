// SPDX-License-Identifier: AGPL-3.0-only

package resource

import "context"

type progressKey struct{}

// Phase is what a step is doing, of what a person follows an update by, as
// the API's StepPhase numbers it (ADR 0080). PhaseNone is anything else.
type Phase int32

// The phases, as dockwell.v1.StepPhase numbers them.
const (
	PhaseNone      Phase = 0
	PhaseBackingUp Phase = 1
	PhasePulling   Phase = 2
	PhaseStarting  Phase = 3
	PhaseChecking  Phase = 4
)

// Activity is one word of how a step is going: what it says, for people, and
// the phase it is in, with how much of a pull is done, where it says.
type Activity struct {
	Message string
	Phase   Phase
	// Done and Total are bytes of a pull, while Phase is PhasePulling and
	// Docker has said. 0 otherwise.
	Done, Total int64
}

// WithProgress returns a context whose step reports how it is going to
// fn, such as the engine updating the step's message, or the agent sending
// it over its stream. fn hears the words alone.
func WithProgress(ctx context.Context, fn func(message string)) context.Context {
	return WithActivities(ctx, func(a Activity) { fn(a.Message) })
}

// WithActivities is WithProgress for whoever hears each word whole, with its
// phase.
func WithActivities(ctx context.Context, fn func(Activity)) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

// ActivityFunc returns what ctx reports progress to, or nil.
func ActivityFunc(ctx context.Context) func(Activity) {
	fn, _ := ctx.Value(progressKey{}).(func(Activity))
	return fn
}

// Progress tells whoever follows the step under way how it is going, such
// as "Pulling nginx:1.27: 12 of 48 MB". Nothing hears it when nobody
// follows. A driver's message must never hold a secret.
func Progress(ctx context.Context, message string) {
	Tell(ctx, Activity{Message: message})
}

// Stage is Progress for a step that has reached phase.
func Stage(ctx context.Context, phase Phase, message string) {
	Tell(ctx, Activity{Message: message, Phase: phase})
}

// Tell passes on a word of progress whole.
func Tell(ctx context.Context, a Activity) {
	if fn := ActivityFunc(ctx); fn != nil {
		fn(a)
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
	// LastAccess says this is the change's last step on its server of a kind
	// under the firewall's timer (AccessGuard): the agent waits for the change
	// to be confirmed, or its timer to run out, before the step ends (ADR 0052).
	LastAccess bool
	// Reconcile identifies a local action durably recorded before creation. It
	// stays on the container so an agent restart can account for its outcome.
	Reconcile string
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
