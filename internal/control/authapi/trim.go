// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"context"

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
	"dockwell.v1.BackupDestinationInfo": {"id": true, "name": true, "last_upload": true, "last_failure": true},
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
