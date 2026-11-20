// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"context"
	"log/slog"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/store"
)

// byNameAndState are what someone below an admin sees of what projects name on
// the install (ADR 0049): a server's name, whether it's up, and its public
// address; a backup destination's name and whether backups get there; a registry
// login's name and registry (ADR 0086); a sign-in provider's name and what the
// panel calls it (ADR 0087); a GitHub App's name, owner, page and whether GitHub
// reaches the panel, and its installations' accounts and states, with the link
// that adds a repository on GitHub, which GitHub checks (ADR 0098). Inventory,
// stats, history, builds, endpoints, buckets and the rest stay an admin's.
var byNameAndState = map[protoreflect.FullName]map[protoreflect.Name]bool{
	"dockwell.v1.Server":                {"id": true, "name": true, "connection": true, "last_seen_time": true, "control_server": true, "network": true},
	"dockwell.v1.ServerNetwork":         {"ipv4": true, "ipv6": true},
	"dockwell.v1.PublicAddress":         {"address": true},
	"dockwell.v1.BackupDestinationInfo": {"id": true, "name": true, "connection": true, "last_upload": true, "last_failure": true},
	"dockwell.v1.RegistryLoginInfo":     {"id": true, "name": true, "registry": true},
	"dockwell.v1.DnsProviderInfo":       {"id": true, "name": true, "zones": true},
	"dockwell.v1.CertificateInfo":       {"id": true, "name": true, "domains": true, "state": true, "expire_time": true},
	"dockwell.v1.SigninProviderInfo":    {"id": true, "name": true, "display_name": true},
	"dockwell.v1.GitHubAppInfo": {
		"id": true, "name": true, "owner": true, "owner_is_organization": true, "html_url": true,
		"installations": true, "delivery": true, "gone": true,
	},
	"dockwell.v1.GitHubInstallation": {
		"id": true, "account": true, "organization": true, "suspended": true, "removed": true, "settings_url": true,
	},
}

// trims reports a call whose response is trimmed to names and states: one that
// reads what projects name, made by someone below an admin.
func trims(ctx context.Context, procedure string) bool {
	if roleRules[procedure].reach != dockwellv1.Reach_REACH_NAMED {
		return false
	}
	grants, ok := grantsFrom(ctx)
	return ok && store.RoleRank(highestOn(grants)) < store.RoleRank(store.RoleAdmin)
}

// highestOn is the highest role among grants on the whole install.
func highestOn(grants []store.Grant) string {
	best := ""
	for _, g := range grants {
		if g.ProjectID == store.Install && store.RoleRank(g.Role) > store.RoleRank(best) {
			best = g.Role
		}
	}
	return best
}

// trim clears what a message holds beyond names and states, all the way down.
func trim(msg any) {
	pm, ok := msg.(proto.Message)
	if !ok {
		return
	}
	trimMessage(pm.ProtoReflect())
}

func trimMessage(m protoreflect.Message) {
	keep, limited := byNameAndState[m.Descriptor().FullName()]
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if limited && !keep[fd.Name()] {
			m.Clear(fd)
			return true
		}
		if fd.Kind() != protoreflect.MessageKind || fd.IsMap() {
			return true
		}
		if fd.IsList() {
			list := v.List()
			for i := range list.Len() {
				trimMessage(list.Get(i).Message())
			}
			return true
		}
		trimMessage(v.Message())
		return true
	})
}

// briefs reports a caller below an admin on the whole install, who isn't shown a
// server's architecture (ADR 0049), so an update's reason that names one is told
// in brief (#771's review, L2).
func briefs(ctx context.Context) bool {
	grants, ok := grantsFrom(ctx)
	return ok && store.RoleRank(highestOn(grants)) < store.RoleRank(store.RoleAdmin)
}

// brief tells each update a message holds that can't be planned where its app runs
// without the server's architecture: "1.5.0 doesn't run on the server web is on".
func brief(msg any) {
	if pm, ok := msg.(proto.Message); ok {
		briefMessage(pm.ProtoReflect())
	}
}

func briefMessage(m protoreflect.Message) {
	if a, ok := m.Interface().(*dockwellv1.AvailableUpdate); ok {
		if a.GetUnavailable() != "" {
			a.Unavailable = catalog.UnfitBrief(a.GetUnavailable())
		}
		return
	}
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() != protoreflect.MessageKind || fd.IsMap() {
			return true
		}
		if fd.IsList() {
			list := v.List()
			for i := range list.Len() {
				briefMessage(list.Get(i).Message())
			}
			return true
		}
		briefMessage(v.Message())
		return true
	})
}

// trimKeepingApps trims a response as trim does, and gives back of each server's
// samples what its caller's own apps use, and nothing else of them (#797): each
// sample's time and pace, with the apps whose service is in the caller's places,
// as filter narrows a list of a project's objects. On a server whose samples
// have had any of theirs, each sample keeps its time even with none of theirs in
// it, as one where their app stopped says so; a time says no more than the
// server's last_seen_time does (#848's review, M1). A server where none of
// theirs has run keeps no samples, its name and state alone, as before.
//
// seen keeps, for a stream, what's known of each service by its ID until the
// next recheck, so a sample asks the store only of a service it hasn't placed
// (M5), and which servers have shown theirs. Nil keeps it for this call.
func (i *interceptor) trimKeepingApps(ctx context.Context, msg any, seen *appsSeen) {
	if seen == nil {
		seen = &appsSeen{}
	}
	if seen.placed == nil {
		seen.placed = map[string]bool{}
	}
	if seen.shown == nil {
		seen.shown = map[string]bool{}
	}
	placed := seen.placed
	servers := serversIn(msg)
	type samples struct {
		stats   *dockwellv1.ServerStats
		history []*dockwellv1.ServerStats
	}
	before := make(map[*dockwellv1.Server]samples, len(servers))
	unknown := false
	for _, s := range servers {
		if s.GetStats() == nil && len(s.GetHistory()) == 0 {
			continue
		}
		before[s] = samples{stats: s.GetStats(), history: s.GetHistory()}
		for _, st := range append(s.GetHistory(), s.GetStats()) {
			for _, u := range st.GetContainers() {
				if _, known := placed[u.GetServiceId()]; !known && u.GetServiceId() != "" {
					unknown = true
				}
			}
		}
	}
	trim(msg)
	if len(before) == 0 {
		return
	}
	grants, ok := grantsFrom(ctx)
	if !ok {
		return
	}
	// The store is asked only of services this caller's samples haven't placed yet.
	if unknown {
		err := i.svc.View(ctx, func(tx *store.Tx) error {
			within, _ := scopeOf(ctx, tx, grants)
			for _, was := range before {
				for _, st := range append(was.history, was.stats) {
					for _, u := range st.GetContainers() {
						id := u.GetServiceId()
						if _, known := placed[id]; known || id == "" {
							continue
						}
						ok, err := within(id)
						if err != nil {
							return err
						}
						placed[id] = ok
					}
				}
			}
			return nil
		})
		if err != nil {
			// Nothing of a server's samples, as before, rather than what can't be checked.
			clear(placed)
			for s := range before {
				s.Stats, s.History = nil, nil
			}
			i.logger.ErrorContext(ctx, "can't narrow a server's samples to its caller's apps", slog.Any("error", err))
			return
		}
	}
	keep := func(st *dockwellv1.ServerStats) *dockwellv1.ServerStats {
		if st == nil {
			return nil
		}
		var theirs []*dockwellv1.ContainerUsage
		for _, u := range st.GetContainers() {
			if placed[u.GetServiceId()] {
				theirs = append(theirs, u)
			}
		}
		return &dockwellv1.ServerStats{Time: st.GetTime(), Interval: st.GetInterval(), Containers: theirs}
	}
	for s, was := range before {
		stats := keep(was.stats)
		history := make([]*dockwellv1.ServerStats, 0, len(was.history))
		theirs := len(stats.GetContainers()) > 0
		for _, h := range was.history {
			kept := keep(h)
			theirs = theirs || len(kept.GetContainers()) > 0
			history = append(history, kept)
		}
		if theirs {
			seen.shown[s.GetId()] = true
		}
		if !seen.shown[s.GetId()] {
			continue
		}
		s.Stats, s.History = stats, history
	}
}

// appsSeen is what a stream knows of its caller's apps: which services they hold,
// by ID, as read at the last recheck, and which servers have shown any of them.
type appsSeen struct {
	placed map[string]bool
	shown  map[string]bool
}

// serversIn is each server a response carries.
func serversIn(msg any) []*dockwellv1.Server {
	switch m := msg.(type) {
	case *dockwellv1.WatchServersResponse:
		if m.GetServer() != nil {
			return []*dockwellv1.Server{m.GetServer()}
		}
	case *dockwellv1.GetServerResponse:
		if m.GetServer() != nil {
			return []*dockwellv1.Server{m.GetServer()}
		}
	case *dockwellv1.ListServersResponse:
		return m.GetServers()
	}
	return nil
}
