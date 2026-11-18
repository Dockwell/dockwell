// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// A database's external access is three edits that go together (ADR 0038,
// "External access, as built"): its service publishes the engine's port on every
// address of its server, and two firewall rules there let that port in from the
// addresses listed and from nowhere else. The rules are named for the service's ID,
// so two databases of one name on a server don't share them.
const (
	// ExternalAllow names the rule that lets the listed addresses in.
	ExternalAllow = "allow"
	// ExternalDeny names the rule that keeps everyone else out.
	ExternalDeny = "deny"
)

// ExternalRule is the name of the firewall rule a database's external access keeps
// on its server: which is ExternalAllow or ExternalDeny.
func ExternalRule(serviceID, which string) string {
	return "external-" + strings.ToLower(strings.TrimPrefix(serviceID, id.Service+"_")) + "-" + which
}

// externalOf is the service a firewall rule's name says it keeps external access
// for, and whether it's one.
func externalOf(rule, which string) (string, bool) {
	rest, ok := strings.CutPrefix(rule, "external-")
	if !ok {
		return "", false
	}
	svc, ok := strings.CutSuffix(rest, "-"+which)
	if !ok || svc == "" {
		return "", false
	}
	return id.Service + "_" + strings.ToUpper(svc), true
}

// EveryAddress says a published port's address is every address of its server.
func EveryAddress(hostIP string) bool {
	return hostIP == "" || hostIP == "0.0.0.0" || hostIP == "::" || hostIP == "[::]"
}

// checkExternalAccess refuses a change that takes away, or loosens, the rule that
// keeps everyone but the listed addresses off a database's external port, while
// the database goes on publishing that port on every address: the port would then
// answer anyone. A whole-spec edit of the service that keeps the port is no way
// round it, nor is deleting the rule alone. Turning external access off removes
// all three together, which leaves nothing to refuse.
func (e *Engine) checkExternalAccess(ctx context.Context, tx *store.Tx, items []item) error {
	for i := range items {
		it := &items[i]
		if string(it.ref.Kind) != "firewall.rule" || it.current == nil {
			continue
		}
		svcID, ok := externalOf(it.ref.Name, ExternalDeny)
		if !ok {
			continue
		}
		was, ok := it.current.(*resourcev1.FirewallRule)
		if !ok {
			continue
		}
		if now, ok := it.spec.(*resourcev1.FirewallRule); ok && now.GetAction() == resourcev1.FirewallAction_FIREWALL_ACTION_DENY &&
			now.GetPort() == was.GetPort() && now.GetProtocol() == was.GetProtocol() && len(now.GetSources()) == 0 {
			continue
		}
		name, svc, err := e.serviceAfter(ctx, tx, items, svcID)
		if err != nil {
			return err
		}
		for _, p := range svc.GetApp().GetPorts() {
			if p.GetHostPort() == was.GetPort() && EveryAddress(p.GetHostIp()) && !p.GetHeld() {
				return fault.New(ErrInvalid, "%s goes on publishing port %d on every address of its server, and without the rule %s that port answers anyone. "+
					"Turn its external access off on its page, which removes the port and both rules together", name, was.GetPort(), it.ref.Name)
			}
		}
	}
	return nil
}

// serviceAfter is a service as a change leaves it, by its ID: as the change edits it,
// or as it's stored. A service the change deletes, or that isn't there, is nil.
func (e *Engine) serviceAfter(ctx context.Context, tx *store.Tx, items []item, svcID string) (string, *resourcev1.Service, error) {
	for i := range items {
		if items[i].id == svcID && string(items[i].ref.Kind) == "service" {
			svc, _ := items[i].spec.(*resourcev1.Service)
			return items[i].ref.Name, svc, nil
		}
	}
	r, err := tx.GetResourceByID(ctx, svcID)
	if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	var m proto.Message
	if m, err = e.decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec); err != nil {
		return "", nil, err
	}
	svc, _ := m.(*resourcev1.Service)
	return r.Name, svc, nil
}
