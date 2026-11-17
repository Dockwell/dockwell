// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"fmt"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
)

// Publishing a route's domain at Cloudflare is steps of the route's change, which
// the control plane takes, not the route's server: its DNS record, and the rule a
// tunnel needs to send the zone to the server's proxy. They run before the server's
// steps, and the ones that take something away after them, so an app never points
// at a name that doesn't resolve (ADR 0104, sections 7 and 8).

// PublishRoute is a route a change makes, changes or removes, as publishing reads
// it: its spec before and after the change, nil where it has none.
type PublishRoute struct {
	Ref         resource.Ref
	ID          string
	Server      string
	Environment string
	Before      *resourcev1.Route
	After       *resourcev1.Route
}

// Publisher plans and takes a change's steps at Cloudflare.
type Publisher interface {
	// Plan works out the steps routes need at Cloudflare, by route: what each
	// publishes against what Dockwell wrote, and what Cloudflare holds. A route
	// whose publishing can't be done refuses the plan, saying why.
	Plan(ctx context.Context, changeID string, routes []PublishRoute) (map[resource.Ref][]resource.Step, error)
	// Apply takes one step, and says what it did when that's more than the step's
	// own words, as a write that raced someone's in the dashboard.
	Apply(ctx context.Context, changeID string, route PublishRoute, step resource.ProviderStep) (said string, err error)
}

// SetPublisher has the engine plan and take steps at Cloudflare. Nil, as tests that
// publish nothing leave it, plans none, and a route that asks to publish refuses
// its plan.
func (e *Engine) SetPublisher(p Publisher) { e.publisher = p }

// publishing gathers the change's routes that publish, before or after, and plans
// their steps at Cloudflare, by route.
func (e *Engine) publishing(ctx context.Context, changeID string, items []item) (map[resource.Ref][]resource.Step, error) {
	var routes []PublishRoute
	for i := range items {
		it := &items[i]
		if it.ref.Kind != routeKind {
			continue
		}
		pr := publishRouteOf(it, "")
		if pr.Before.GetPublish() == nil && pr.After.GetPublish() == nil {
			continue
		}
		if len(it.servers) > 0 {
			pr.Server = it.servers[0]
		}
		routes = append(routes, pr)
	}
	if len(routes) == 0 {
		return nil, nil
	}
	if e.publisher == nil {
		return nil, fault.New(ErrInvalid, "this control plane doesn't publish domains at Cloudflare, so %s can't be published here", routes[0].Ref.Name)
	}
	steps, err := e.publisher.Plan(ctx, changeID, routes)
	if err != nil && ctx.Err() == nil && !errors.Is(err, ErrInvalid) {
		// What stops publishing stops the plan, in its own words.
		return nil, fault.New(ErrInvalid, "%w", err)
	}
	return steps, err
}

// publishRouteOf is the route a step at Cloudflare is for, as the run has it.
func publishRouteOf(it *item, server string) PublishRoute {
	before, _ := it.current.(*resourcev1.Route)
	after, _ := it.spec.(*resourcev1.Route)
	return PublishRoute{Ref: it.ref, ID: it.id, Server: server, Environment: it.ref.Scope, Before: before, After: after}
}

// providerWords says what a step at Cloudflare does, as a step's message.
func providerWords(state StepState, p *resource.ProviderStep) string {
	var doing, did string
	switch p.Action {
	case resource.ProviderCreateRecord:
		doing, did = "Writing the DNS record for "+p.Name, "Wrote the DNS record for "+p.Name
	case resource.ProviderDeleteRecord:
		doing, did = "Removing the DNS record for "+p.Name, "Removed the DNS record for "+p.Name
	case resource.ProviderReplaceRecord:
		doing, did = "Replacing the DNS record for "+p.Name, "Replaced the DNS record for "+p.Name
	case resource.ProviderTakeOnRecord:
		doing, did = "Taking on the DNS record for "+p.Name, "Took on the DNS record for "+p.Name
	case resource.ProviderRestoreRecord:
		doing, did = "Putting back the DNS record for "+p.Name, "Put back the DNS record for "+p.Name
	case resource.ProviderAddRule:
		doing, did = fmt.Sprintf("Adding %s to the tunnel %s", p.Name, p.Target), fmt.Sprintf("Added %s to the tunnel %s", p.Name, p.Target)
	case resource.ProviderRemoveRule:
		doing, did = fmt.Sprintf("Removing %s from the tunnel %s", p.Name, p.Target), fmt.Sprintf("Removed %s from the tunnel %s", p.Name, p.Target)
	case resource.ProviderTakeOnRule:
		doing, did = fmt.Sprintf("Taking on %s in the tunnel %s", p.Name, p.Target), fmt.Sprintf("Took on %s in the tunnel %s", p.Name, p.Target)
	case resource.ProviderMoveRule:
		doing, did = fmt.Sprintf("Moving %s in the tunnel %s to the server's proxy", p.Name, p.Target), fmt.Sprintf("Moved %s in the tunnel %s to the server's proxy", p.Name, p.Target)
	}
	switch state {
	case StepQueued, StepWaiting:
		return "Waiting: " + doing + " at Cloudflare"
	case StepRunning:
		return doing + " at Cloudflare"
	case StepDone:
		return did + " at Cloudflare"
	case StepFailed:
		return "Couldn't finish: " + doing + " at Cloudflare"
	case StepSkipped:
		return "Skipped: " + doing + " at Cloudflare"
	}
	return doing
}
