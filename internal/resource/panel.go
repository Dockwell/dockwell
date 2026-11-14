// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

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
	// Apps acts on Swarm services of what the panel runs, which Parts name, as each
	// is made again in Dockwell: a stop scales each to none as Dockwell's copy
	// starts, and its undo back.
	Apps bool `json:",omitempty"`
	// Parts are what to start, and how each stood, for a start: what an undo puts
	// back. A stop finds them itself.
	Parts []PanelPart `json:",omitempty"`
	// Domains and Evidence are, for a stop of the proxy, the domains a person chose
	// to hand over and PanelEvidence of what was checked of them as the plan was
	// made: the agent checks them again before the proxy stops.
	Domains  []string `json:",omitempty"`
	Evidence string   `json:",omitempty"`
}

// Panel is what stopping or starting a panel does, as its agent planned it.
type Panel struct {
	// Kind is the panel: PanelCoolify or PanelDokploy.
	Kind string
	// Start says it starts them; otherwise it stops them.
	Start bool
	// Proxy says it acts on the panel's proxy alone, as its domains come over.
	Proxy bool `json:",omitempty"`
	// Apps says it acts on Swarm services of what the panel runs.
	Apps bool `json:",omitempty"`
	// Parts are each of the panel's own containers or services the step acts on,
	// in the order it acts, with how each stood.
	Parts []PanelPart
	// Stays is the panel's proxy, which goes on running: it serves the domains of
	// what was taken on until those come as routes, a step of its own.
	Stays string `json:",omitempty"`
	// Domains and Evidence are, for a stop of the proxy, the domains a person chose
	// to hand over and the digest of what was checked of them, as PanelAsked has.
	Domains  []string `json:",omitempty"`
	Evidence string   `json:",omitempty"`
}

// PanelCheck is what was checked of a domain a panel's proxy serves, before a
// handover (ADR 0089): where else a rule mentions it, what answered a request
// through the proxy for each path, and why it stays behind, if it does.
type PanelCheck struct {
	Domain   string
	Mentions []string
	Probes   []PanelProbe
	Held     string
	// Scanned is what the scan read, for people; it's no part of the evidence.
	Scanned string
}

// PanelProbe is a request through a panel's proxy for a domain's path.
type PanelProbe struct {
	Path   string
	Answer string
	// Open says the answer shows no login or address limit.
	Open bool
}

// PanelEvidence is a digest of what was checked of each of domains: the plan of a
// handover keeps it, and the agent computes it again just before the proxy stops,
// refusing where it differs. An open answer counts as open alone, so an app that
// answers 200 then 404 reads the same; what held a domain counts as said. A domain
// with no check reads as missing.
func PanelEvidence(checks []PanelCheck, domains []string) string {
	by := map[string]PanelCheck{}
	for _, c := range checks {
		by[strings.ToLower(c.Domain)] = c
	}
	chosen := slices.Clone(domains)
	for i := range chosen {
		chosen[i] = strings.ToLower(chosen[i])
	}
	slices.Sort(chosen)
	chosen = slices.Compact(chosen)
	h := sha256.New()
	for _, d := range chosen {
		c, ok := by[d]
		if !ok {
			fmt.Fprintf(h, "domain %q missing\n", d)
			continue
		}
		fmt.Fprintf(h, "domain %q held %q\n", d, c.Held)
		mentions := slices.Clone(c.Mentions)
		slices.Sort(mentions)
		for _, m := range mentions {
			fmt.Fprintf(h, "  mention %q\n", m)
		}
		probes := slices.Clone(c.Probes)
		slices.SortFunc(probes, func(a, b PanelProbe) int { return strings.Compare(a.Path, b.Path) })
		for _, p := range probes {
			if p.Open {
				fmt.Fprintf(h, "  probe %q open\n", p.Path)
			} else {
				fmt.Fprintf(h, "  probe %q %q\n", p.Path, p.Answer)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
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
	// WasPorts are the ports a Swarm service of what the panel ran published as
	// the stop was planned: a stop takes them off, as Swarm's routing mesh holds
	// them while the service publishes them, and a start puts them back.
	WasPorts []PanelPort `json:",omitempty"`
	// WasNetworks are the networks such a service joined as the stop was planned,
	// as Docker has them: a stop takes it off them, so its name answers there for
	// its copy alone, and a start puts them back exactly.
	WasNetworks string `json:",omitempty"`
}

// PanelPort is a port a Swarm service publishes.
type PanelPort struct {
	Protocol          string
	Target, Published uint32
	// Mode is "ingress", through the routing mesh, or "host".
	Mode string `json:",omitempty"`
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
	return PanelAsked{Kind: p.Kind, Start: true, Proxy: p.Proxy, Apps: p.Apps, Parts: p.Parts}, true
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
