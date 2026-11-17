// SPDX-License-Identifier: AGPL-3.0-only

package services

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
)

// elsewhere are container ports known for something other than web pages: SSH,
// mail, DNS, databases, caches and queues. An app that publishes one of them
// beside its web port, as Gitea publishes 22 beside 3000, serves its pages on the
// other.
var elsewhere = map[uint32]bool{
	21: true, 22: true, 25: true, 53: true, 110: true, 143: true, 389: true, 465: true,
	587: true, 636: true, 993: true, 995: true, 1433: true, 1521: true, 1883: true,
	3306: true, 4369: true, 5432: true, 5672: true, 6379: true, 8883: true, 9042: true,
	11211: true, 27017: true,
}

// WebPort is the port in an app's container that most likely serves its web
// pages, which Add a domain starts from (#791):
//  1. routed, the port a route already sends to, where one does;
//  2. the port its health check asks over HTTP;
//  3. its first published TCP port that isn't one of elsewhere's;
//  4. its first published TCP port.
//
// It's 0 on the server's own network, where a route names the server's port, and
// where none of these names one.
func WebPort(app *resourcev1.Container, routed uint32) uint32 {
	if app.GetHostNetwork() {
		return 0
	}
	if routed > 0 {
		return routed
	}
	if p := healthPort(app.GetHealthcheck()); p > 0 {
		return p
	}
	var first uint32
	for _, p := range app.GetPorts() {
		c := p.GetContainerPort()
		if c == 0 || p.GetProtocol() == resourcev1.Protocol_PROTOCOL_UDP {
			continue
		}
		if !elsewhere[c] {
			return c
		}
		if first == 0 {
			first = c
		}
	}
	return first
}

// local is an address in a health check's command that names the container
// itself, with its port, such as http://127.0.0.1:3000/api/healthz. The scheme is
// optional, as curl and wget take a bare host, and only a command that runs one of
// them is read without it.
var local = regexp.MustCompile(`(?i)(https?://)?(?:localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\])(?::(\d{1,5}))?(?:[/?#\s"']|$)`)

// healthPort is the port a health check asks over HTTP: an HTTP check's own, or
// the port of the first address on the container a curl or wget in its command
// names. A plain http:// address with no port is 80. A TCP check, or a command
// that asks some other way, names none.
func healthPort(h *resourcev1.Healthcheck) uint32 {
	if p := h.GetHttp().GetPort(); p > 0 {
		return p
	}
	words := h.GetTest()
	if len(words) == 0 {
		return 0
	}
	// ["CMD", ...] or ["CMD-SHELL", "..."]: what Docker runs, word by word or as a line.
	command := strings.Join(words[1:], " ")
	fetches := strings.Contains(command, "curl") || strings.Contains(command, "wget")
	for _, m := range local.FindAllStringSubmatch(command, -1) {
		scheme, port := strings.ToLower(m[1]), m[2]
		if scheme == "" && !fetches {
			continue
		}
		if port == "" {
			if scheme == "http://" {
				return 80
			}
			continue
		}
		if n, err := strconv.ParseUint(port, 10, 16); err == nil && n > 0 {
			return uint32(n)
		}
	}
	return 0
}

// routedPorts is the port a route already sends each service to, by environment,
// then service name: the oldest route to the whole of a domain where there's one,
// else the oldest to any of its paths. A redirect sends nowhere, and a route that
// can't be read is left out, as the list stands without it.
func (h *Handler) routedPorts(ctx context.Context, tx *store.Tx, scopes []string) (map[string]map[string]uint32, error) {
	out := map[string]map[string]uint32{}
	for _, scope := range scopes {
		list, err := tx.ListResources(ctx, scope, string(route.Kind))
		if err != nil {
			return nil, err
		}
		// IDs sort by when they were made, so the first is the oldest.
		slices.SortFunc(list, func(a, b store.Resource) int { return cmp.Compare(a.ID, b.ID) })
		specs := make([]store.StoredSpec, len(list))
		for i := range list {
			specs[i] = store.StoredSpec{At: store.InRevision(list[i].ID, list[i].Kind, list[i].Revision), Spec: list[i].Spec}
		}
		decoded, unreadable, err := h.keeper.DecodeEach(ctx, tx, specs)
		if err != nil {
			return nil, err
		}
		whole, some := map[string]uint32{}, map[string]uint32{}
		for i := range list {
			r, ok := decoded[i].(*resourcev1.Route)
			if unreadable[i] != nil || !ok || r.GetRedirect() != nil || r.GetService() == "" || r.GetPort() == 0 {
				continue
			}
			if _, seen := some[r.GetService()]; !seen {
				some[r.GetService()] = r.GetPort()
			}
			if _, seen := whole[r.GetService()]; !seen && (r.GetPath() == "" || r.GetPath() == "/") {
				whole[r.GetService()] = r.GetPort()
			}
		}
		for name, port := range whole {
			some[name] = port
		}
		out[scope] = some
	}
	return out, nil
}
