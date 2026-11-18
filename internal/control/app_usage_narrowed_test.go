// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A token narrowed to one project reads that project's apps' figures alone, on
// every read that carries a server's samples, and none of the server's own. Once
// its app stops while another project's runs, the watch still sends the newest
// sample, empty of theirs, so the panel stops showing the last figures as
// current (#848's review: its test, and M1).
func TestANarrowedTokenReadsItsAppsAloneOnEveryServerRead(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		pullsAnything(e)
		c, cat := changeClient(b, b.origin), catalogClient(b)
		apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Project: "shop"}))
		apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Project: "blog"}))
		ids := map[string]string{}
		if err := f.store.View(t.Context(), func(tx *store.Tx) error {
			org, err := tx.Organization(t.Context())
			if err != nil {
				return err
			}
			for _, name := range []string{"shop", "blog"} {
				p, err := tx.GetResource(t.Context(), "project", org.ID, name)
				if err != nil {
					return err
				}
				envs, err := tx.ListResources(t.Context(), p.ID, "environment")
				if err != nil {
					return err
				}
				db, err := tx.GetResource(t.Context(), "service", envs[0].ID, "db")
				if err != nil {
					return err
				}
				ids[name] = db.ID
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		server := f.lastDocker.id.ServerID
		now := time.Now()
		f.config.Fleet.Record(server, &dockwellv1.ServerStats{
			Time: timestamppb.New(now), CpuPercent: 37, MemoryUsedBytes: 3 << 30, MemoryTotalBytes: 8 << 30,
			Containers: []*dockwellv1.ContainerUsage{
				{ServiceId: ids["shop"], CpuPercent: 25, MemoryBytes: 100 << 20, Replicas: 1},
				{ServiceId: ids["blog"], CpuPercent: 5, MemoryBytes: 50 << 20, Replicas: 1},
			},
		})

		secret, _ := narrowedToken(t, b, &dockwellv1.CreateTokenRequest{Projects: []string{"shop"}})
		servers := dockwellv1connect.NewServerServiceClient(bearer{token: secret, rt: b.client.Transport}, b.origin, connect.WithProtoJSON())
		check := func(where string, s *dockwellv1.Server) (theirs int) {
			t.Helper()
			for _, st := range append(s.GetHistory(), s.GetStats()) {
				if st == nil {
					continue
				}
				if st.GetCpuPercent() != 0 || st.GetMemoryTotalBytes() != 0 || st.GetMemoryUsedBytes() != 0 || st.GetUptime() != nil {
					t.Errorf("%s: the server's own figures reach a narrowed token: %v", where, st)
				}
				for _, u := range st.GetContainers() {
					if u.GetServiceId() != ids["shop"] {
						t.Errorf("%s: another project's app reaches a narrowed token: %v", where, u)
					}
					theirs++
				}
			}
			return theirs
		}
		list, err := servers.ListServers(t.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		sawShop := false
		for _, s := range list.Msg.GetServers() {
			if check("ListServers", s) > 0 && s.GetId() == server {
				sawShop = true
			}
		}
		if !sawShop {
			t.Errorf("ListServers: the narrowed token sees none of shop's figures")
		}
		got, err := servers.GetServer(t.Context(), connect.NewRequest(&dockwellv1.GetServerRequest{Id: server}))
		if err != nil {
			t.Fatal(err)
		}
		if check("GetServer", got.Msg.GetServer()) == 0 {
			t.Errorf("GetServer: the narrowed token sees none of shop's figures")
		}

		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		watch, err := servers.WatchServers(ctx, connect.NewRequest(&dockwellv1.WatchServersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		synced, recorded, stopped := false, false, false
		for watch.Receive() {
			m := watch.Msg()
			if s := m.GetServer(); s != nil {
				check("WatchServers", s)
				// Shop's app stopped: the newest sample has blog's alone, so it reaches the token empty.
				if recorded && s.GetId() == server {
					if s.GetStats() == nil {
						t.Errorf("after shop's app stopped, the watch sends no sample, so the panel keeps its last figures as current")
					} else if len(s.GetStats().GetContainers()) != 0 {
						t.Errorf("after shop's app stopped, the newest sample says %v", s.GetStats().GetContainers())
					}
					stopped = true
					break
				}
			}
			if m.GetSnapshotComplete() {
				synced = true
			}
			if synced && !recorded {
				recorded = true
				f.config.Fleet.Record(server, &dockwellv1.ServerStats{
					Time: timestamppb.New(now.Add(5 * time.Second)), CpuPercent: 30, MemoryUsedBytes: 3 << 30, MemoryTotalBytes: 8 << 30,
					Containers: []*dockwellv1.ContainerUsage{{ServiceId: ids["blog"], CpuPercent: 5, MemoryBytes: 50 << 20, Replicas: 1}},
				})
			}
		}
		if err := watch.Err(); err != nil && ctx.Err() == nil {
			t.Fatal(err)
		}
		if !stopped {
			t.Errorf("the watch never sent the sample after shop's app stopped")
		}
	})
}
