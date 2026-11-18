// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/publish"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// publisher is what publishes routes' domains at Cloudflare for the engine (ADR
// 0104): the DNS providers' tokens, the store of what was written, each server's
// tunnels as its agent reads them now, and the panel's own names, which no step
// touches.
func publisher(cfg *Config) *publish.Publisher {
	p := &publish.Publisher{DB: cfg.Store, Providers: cfg.dns}
	if cfg.Identity != nil {
		p.Install = cfg.Identity.InstallID()
	}
	p.Tunnels = func(ctx context.Context, server string) ([]*dockwellv1.CloudflaredTunnel, error) {
		if cfg.Agents == nil {
			return nil, errors.New("this control plane reaches no agents")
		}
		session, ok := cfg.Agents.Session(server)
		if !ok {
			return nil, errors.New("its agent isn't connected now. Try again once it is")
		}
		return session.ReadTunnels(ctx)
	}
	p.Server = func(_ context.Context, server string) (publish.Facts, error) {
		if cfg.Fleet == nil {
			return publish.Facts{}, errors.New("this control plane keeps no fleet")
		}
		srv, err := cfg.Fleet.Get(server)
		if err != nil {
			return publish.Facts{}, err
		}
		facts := publish.Facts{Name: srv.GetName()}
		// Where the server is reached from the internet, as ADR 0034 proves it, and
		// where it leaves from, which a tunnel's connections come from.
		for _, a := range []*dockwellv1.PublicAddress{srv.GetNetwork().GetIpv4(), srv.GetNetwork().GetIpv6()} {
			if a.GetAddress() == "" {
				continue
			}
			facts.WaysOut = append(facts.WaysOut, a.GetAddress())
			if addr, err := netip.ParseAddr(a.GetAddress()); err == nil && !a.GetUnproven() {
				facts.Public = append(facts.Public, addr)
			}
		}
		if out := srv.GetInventory().GetOutboundAddress(); out != "" {
			facts.WaysOut = append(facts.WaysOut, out)
		}
		facts.SharesWith = sharingAnAddress(cfg.Fleet.List(), srv.GetId(), facts.WaysOut)
		return facts, nil
	}
	// The panel's names: in use, saved for its restart, and the one dockwell run was
	// started with. The domain it moves from (ADR 0063) and its apps domain (ADR 0061)
	// go in Moving and Apps once those are built; neither is yet.
	p.Panel = func(context.Context) publish.PanelNames {
		var names publish.PanelNames
		if cfg.Auth != nil && cfg.Auth.Sites().Domain != "" {
			names.Served = append(names.Served, cfg.Auth.Sites().Domain)
		}
		if cfg.Changes != nil {
			if t, err := cfg.Changes.Kinds().Type("route"); err == nil {
				if pd, ok := t.(revision.PanelDomains); ok {
					names.Served = append(names.Served, pd.PanelDomains()...)
				}
			}
		}
		if cfg.PanelListen != "" {
			names.Service = "http://" + cfg.PanelListen
		}
		return names
	}
	p.ProjectOf = func(ctx context.Context, environment string) (string, error) {
		var project string
		err := cfg.Store.View(ctx, func(tx *store.Tx) error {
			env, err := tx.GetResourceByID(ctx, environment)
			if err != nil {
				return fmt.Errorf("can't find the route's environment: %w", err)
			}
			project = env.Scope
			return nil
		})
		return project, err
	}
	p.HostPorts = func(ctx context.Context, environment, name string) ([]uint32, error) {
		var out []uint32
		err := cfg.Store.View(ctx, func(tx *store.Tx) error {
			r, err := tx.GetResource(ctx, "service", environment, name)
			if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
				return nil
			}
			if err != nil {
				return err
			}
			spec, err := cfg.Secrets.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return fmt.Errorf("can't read the service %s: %w", name, err)
			}
			if svc, ok := spec.(*resourcev1.Service); ok {
				for _, port := range svc.GetApp().GetPorts() {
					if port.GetHostPort() != 0 {
						out = append(out, port.GetHostPort())
					}
				}
			}
			return nil
		})
		return out, err
	}
	p.Routes = func(ctx context.Context) (map[string]*resourcev1.Route, error) {
		out := map[string]*resourcev1.Route{}
		err := cfg.Store.View(ctx, func(tx *store.Tx) error {
			stored, err := tx.ListKind(ctx, "route")
			if err != nil {
				return err
			}
			for i := range stored {
				r := &stored[i]
				if r.Spec == nil {
					continue
				}
				spec, err := cfg.Secrets.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
				if err != nil {
					return fmt.Errorf("can't read the route %s: %w", r.Name, err)
				}
				if route, ok := spec.(*resourcev1.Route); ok {
					out[r.ID] = route
				}
			}
			return nil
		})
		return out, err
	}
	return p
}

// sharingAnAddress names the servers of fleet, other than id, that leave from one of
// the addresses in out.
func sharingAnAddress(fleet []*dockwellv1.Server, id string, out []string) []string {
	var names []string
	for _, srv := range fleet {
		if srv.GetId() == id {
			continue
		}
		theirs := []string{srv.GetNetwork().GetIpv4().GetAddress(), srv.GetNetwork().GetIpv6().GetAddress(), srv.GetInventory().GetOutboundAddress()}
		if slices.ContainsFunc(theirs, func(a string) bool { return a != "" && slices.Contains(out, a) }) {
			names = append(names, srv.GetName())
		}
	}
	slices.Sort(names)
	return names
}
