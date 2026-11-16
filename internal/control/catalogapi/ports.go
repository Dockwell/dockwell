// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/revision"
)

// PortToServerAloneNote is a plan note's reason for a port that answered on every
// address and answers on the server alone after the plan, which an automatic update
// leaves for a person, who may reach it no other way the next morning (ADR 0101;
// #752's re-check, N2).
const PortToServerAloneNote = "port_to_server_alone"

// portName is a port as a public port names it: "80", or "443/udp".
func portName(p *resourcev1.PortMapping) string {
	out := strconv.Itoa(int(p.GetHostPort()))
	if p.GetProtocol() == resourcev1.Protocol_PROTOCOL_UDP {
		out += "/udp"
	}
	return out
}

// onEveryAddress reports a port that answers on every address of its server, as
// its spec has it: on no address or one that is every address. A held one counts
// as its address says, as opening it gives that back (#752's review, L3).
func onEveryAddress(p *resourcev1.PortMapping) bool {
	ip := p.GetHostIp()
	return ip == "" || ip == "0.0.0.0" || ip == "::"
}

// newlyPublic are the ports a release declares public that the release the install
// runs didn't publish on every address, by service, as was has them: only those are
// a person's to approve, so a later release asks nothing it asked before (ADR 0101).
// It compares what each release declares, not how the port is now, so a port held
// to the server alone for now asks nothing again (#752's review, L3).
func newlyPublic(public, was map[string][]string) map[string][]string {
	out := map[string][]string{}
	for service, ports := range public {
		for _, port := range ports {
			if !slices.Contains(was[service], port) {
				out[service] = append(out[service], port)
			}
		}
	}
	return out
}

// ranOnEveryAddress are the ports the release an install runs published on every
// address, by service, from its pin's file old with oldVariables, as next names its
// services.
func ranOnEveryAddress(next *catalogv1.Entry, old *resourcev1.CatalogPin, oldVariables map[string]string) map[string][]string {
	base := proto.CloneOf(next)
	base.Compose = string(old.GetFile())
	return catalog.EveryAddress(base, oldVariables)
}

// movedAlone are the notes for the ports a plan of next, with variables, moves to the
// server alone, where the install ran old with oldVariables: an upgrade, a change of
// companion and a change of need alike (toServerAlone).
func (f found) movedAlone(next *catalogv1.Entry, variables map[string]string, old *resourcev1.CatalogPin, oldVariables map[string]string) []revision.Note {
	public := catalog.PublicPorts(next, variables)
	return f.toServerAlone(public, catalog.Published(next, variables), ranOnEveryAddress(next, old, oldVariables), domainFor(next, public), next.GetWeb().GetService())
}

// domainFor says a service a domain may reach, so a note offers one: the page a person
// opens of an app meant for the web, never a companion tool or a database, such as
// Adminer beside PostgreSQL, which shouldn't be on the internet, and not where the
// app holds 80 or 443 itself, as a proxy may, which leaves Dockwell's ingress none
// (#752's review, M2).
func domainFor(e *catalogv1.Entry, public map[string][]string) func(service string) bool {
	for _, ports := range public {
		if slices.Contains(ports, "80") || slices.Contains(ports, "443") {
			return func(string) bool { return false }
		}
	}
	return func(service string) bool {
		return service == e.GetWeb().GetService() && !slices.Contains(e.GetDatabases(), service) &&
			!slices.ContainsFunc(e.GetCompanions(), func(c *catalogv1.Companion) bool { return c.GetService() == service })
	}
}

// toServerAlone says, a note a service, which of its ports answered on every address
// and answer on the server alone after an upgrade, as every port of a catalog app does
// but those its entry declares public (ADR 0101), and how to reach it now. published
// are the ports the release publishes, by service, as Published has them, and was
// those the install's own file published on every address: a port a person opened
// themselves stays as they set it, which the merge keeps, so it isn't said.
//
// web is the service the app's page shows a tunnel for, on its first port on the
// server alone, which the note names; any other port it gives as a tunnel to its
// port, as the page shows none for it (#752's re-check, N3).
func (f found) toServerAlone(public, published, was map[string][]string, domain func(service string) bool, web string) []revision.Note {
	var out []revision.Note
	for _, service := range slices.Sorted(maps.Keys(f.ports)) {
		var moved []string
		for _, p := range f.ports[service] {
			port := portName(p)
			if onEveryAddress(p) && slices.Contains(was[service], port) && slices.Contains(published[service], port) && !slices.Contains(public[service], port) {
				moved = append(moved, port)
			}
		}
		if len(moved) == 0 {
			continue
		}
		words := "port " + moved[0]
		if len(moved) > 1 {
			words = "ports " + strings.Join(moved[:len(moved)-1], ", ") + " and " + moved[len(moved)-1]
		}
		// An SSH tunnel reaches it in any case: the one its page shows, where the page
		// shows one for this port, or one to its port; a domain, only where one is meant
		// to (#752's review, M2).
		reach := "Reach it through an SSH tunnel to " + words + "."
		if len(moved) > 1 {
			reach = "Reach them through SSH tunnels to " + words + "."
		}
		if len(moved) == 1 && service == web && f.pageTunnel(service, moved[0]) {
			reach = "Reach it with the SSH tunnel on its page."
		}
		if domain(service) {
			reach = strings.TrimSuffix(reach, ".") + ", or add a domain to " + service + "."
		}
		verb := "answers"
		if len(moved) > 1 {
			verb = "answer"
		}
		out = append(out, revision.Note{
			Reason: PortToServerAloneNote, Names: []string{service}, Named: true,
			Message: fmt.Sprintf("%s's %s %s on this server alone after this, not on every address. %s", service, words, verb, reach),
		})
	}
	return out
}

// serverAlone refuses to plan an entry whose file, read as Dockwell runs it, publishes
// a port past the server that the entry doesn't declare public. The rewrite writes
// each port on 127.0.0.1 where the service writes it, and the catalog's check
// refuses what it can't see; this reads the ports Compose itself would publish, so
// the rule holds for any form neither knows (#752's review, H1).
func serverAlone(ctx context.Context, e *catalogv1.Entry, variables map[string]string) error {
	stack, err := compose.Read(ctx, compose.Input{File: []byte(e.GetCompose()), Variables: variables})
	if err != nil {
		return authapi.ReasonError(connect.CodeInvalidArgument, err, "invalid")
	}
	public := catalog.PublicPorts(e, variables)
	for _, s := range stack.Services {
		for _, p := range s.Spec.GetApp().GetPorts() {
			if p.GetHostIp() == "127.0.0.1" || onEveryAddress(p) && slices.Contains(public[s.Name], portName(p)) {
				continue
			}
			return authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s's port %s would answer past this server, which its catalog entry doesn't declare, and Dockwell couldn't write it on 127.0.0.1, so nothing was planned. Ask the catalog to write %s's ports out in the service", s.Name, portName(p), s.Name), "public_port")
		}
	}
	return nil
}

// pageTunnel says the app's page shows its tunnel for port of service once the plan
// moves it to the server alone: the service's first port on the server alone, as the
// page picks it (web/src/features/catalog/access.tsx).
func (f found) pageTunnel(service, port string) bool {
	for _, p := range f.ports[service] {
		if !onEveryAddress(p) || portName(p) == port {
			return portName(p) == port
		}
	}
	return false
}
