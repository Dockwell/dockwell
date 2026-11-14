// SPDX-License-Identifier: AGPL-3.0-only

package resource

import "context"

// Stopping a panel Dockwell imported from (ADR 0089): a change that stops Coolify's
// or Dokploy's own containers and services, as an edit of the server's host that
// makes no revision, as an action on a system service is (ADR 0082). Its plan keeps
// how each stood, so undoing it plans starting them again, as they were.

// The panels a change can stop, by the word the import gives them.
const (
	PanelCoolify = "coolify"
	PanelDokploy = "dokploy"
)

// PanelAsked is stopping or starting a panel's own containers and services, for a
// server's host.
type PanelAsked struct {
	// Change is the change's ID.
	Change string
	// Kind is the panel: PanelCoolify or PanelDokploy.
	Kind string
	// Start starts again what a stop stopped, as Parts say each stood before it.
	Start bool
	// Proxy acts on the panel's proxy alone, which holds ports 80 and 443, as its
	// domains come over to Dockwell's ingress; otherwise on the panel's own
	// containers, never its proxy.
	Proxy bool `json:",omitempty"`
	// Parts are what to start, and how each stood, for a start: what an undo puts
	// back. A stop finds them itself.
	Parts []PanelPart `json:",omitempty"`
}

// Panel is what stopping or starting a panel does, as its agent planned it.
type Panel struct {
	// Kind is the panel: PanelCoolify or PanelDokploy.
	Kind string
	// Start says it starts them; otherwise it stops them.
	Start bool
	// Proxy says it acts on the panel's proxy alone, as its domains come over.
	Proxy bool `json:",omitempty"`
	// Parts are each of the panel's own containers or services the step acts on,
	// in the order it acts, with how each stood.
	Parts []PanelPart
	// Stays is the panel's proxy, which goes on running: it serves the domains of
	// what was taken on until those come as routes, a step of its own.
	Stays string `json:",omitempty"`
}

// PanelPart is one of a panel's own containers or Swarm services.
type PanelPart struct {
	// Name is its name, such as "coolify-db".
	Name string
	// Swarm says it's a Swarm service, which runs replicas, rather than a container.
	Swarm bool `json:",omitempty"`
	// WasRunning says it ran as the stop was planned: what a start puts back.
	WasRunning bool `json:",omitempty"`
	// WasRestart is a container's restart policy as the stop was planned, such as
	// "always". A stop sets it to "no", so the panel doesn't start again with Docker
	// or the server, and a start puts it back.
	WasRestart string `json:",omitempty"`
	// WasReplicas is how many replicas a Swarm service ran as the stop was planned.
	// A stop scales it to none, and a start back to this.
	WasReplicas uint64 `json:",omitempty"`
}

// Name is the panel's name as people call it.
func (p Panel) Name() string { return PanelName(p.Kind) }

// PanelName is a panel's name as people call it, by its kind.
func PanelName(kind string) string {
	switch kind {
	case PanelCoolify:
		return "Coolify"
	case PanelDokploy:
		return "Dokploy"
	}
	return kind
}

// Undone is what puts back what p did: each part as it stood, its restart policy
// or its replicas, started where it ran. It's false for a start, which puts back a
// stop and has nothing of its own.
func (p Panel) Undone() (PanelAsked, bool) {
	if p.Start || len(p.Parts) == 0 {
		return PanelAsked{}, false
	}
	return PanelAsked{Kind: p.Kind, Start: true, Proxy: p.Proxy, Parts: p.Parts}, true
}

// PanelProxy is the container a panel's proxy runs as, which holds ports 80 and
// 443 until its domains come over, or "" for a panel Dockwell doesn't know.
func PanelProxy(kind string) string {
	switch kind {
	case PanelCoolify:
		return "coolify-proxy"
	case PanelDokploy:
		return "dokploy-traefik"
	}
	return ""
}

// PanelProxyGoing is the panel's proxy a server's desired resources stop, as their
// domains come over in the same change, or "": what holds ports 80 and 443 now and
// won't once that step has run.
func PanelProxyGoing(desired []Desired) string {
	for i := range desired {
		if p := desired[i].Panel; p != nil && p.Proxy && !p.Start {
			return PanelProxy(p.Kind)
		}
	}
	return ""
}

// PanelActor is a kind that stops or starts a panel's own containers as a step of a
// change: the server's host (ADR 0089).
type PanelActor interface {
	// JudgePanel plans it, and reports false when there's nothing to do: what it
	// would stop is stopped already, or what it would start runs.
	JudgePanel(ctx context.Context, ref Ref, ask PanelAsked) (Decision, bool, error)
	// ApplyPanel takes the step, and checks what came of it.
	ApplyPanel(ctx context.Context, step Step) error
}
