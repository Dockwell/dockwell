// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// What each app uses on a server reaches whoever holds its project, and that alone:
// below an admin, a server's samples keep their time and the containers of the
// caller's own apps, never the server's own figures or another project's apps. An
// admin reads all of it (#797).
func TestAnAppsFiguresReachWhoHoldsItsProjectAlone(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		pullsAnything(e)
		c, cat := changeClient(b, b.origin), catalogClient(b)
		apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Project: "shop"}))
		apply(t, c, installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "postgres", Project: "blog"}))
		ids, projects := map[string]string{}, map[string]string{}
		var shopProject string
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
				if name == "shop" {
					shopProject = p.ID
				}
				envs, err := tx.ListResources(t.Context(), p.ID, "environment")
				if err != nil || len(envs) == 0 {
					t.Fatalf("%s's environments: %v, %v", name, envs, err)
				}
				db, err := tx.GetResource(t.Context(), "service", envs[0].ID, "db")
				if err != nil {
					return err
				}
				ids[name] = db.ID
				projects[name] = docker.Project(envs[0].ID)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		// A container's service is found from its Compose project, its environment's ID in lowercase.
		find := (&serviceIDs{db: f.store}).find
		if got := find(t.Context(), projects["shop"], "db"); got != ids["shop"] {
			t.Errorf("shop's db from %s is %q, want %q", projects["shop"], got, ids["shop"])
		}
		if got := find(t.Context(), projects["shop"], "nothing"); got != "" {
			t.Errorf("a service there's none of is %q", got)
		}

		server := f.lastDocker.id.ServerID
		shopApp, blogApp := ids["shop"], ids["blog"]
		f.config.Fleet.Record(server, &dockwellv1.ServerStats{
			Time: timestamppb.New(time.Now()), CpuPercent: 37, MemoryUsedBytes: 3 << 30, MemoryTotalBytes: 8 << 30,
			Containers: []*dockwellv1.ContainerUsage{
				{ServiceId: shopApp, CpuPercent: 25, MemoryBytes: 100 << 20, MemoryLimitBytes: 512 << 20, Replicas: 1},
				{ServiceId: blogApp, CpuPercent: 5, MemoryBytes: 50 << 20, Replicas: 1},
			},
		})

		servers := dockwellv1connect.NewServerServiceClient(b, b.origin, connect.WithProtoJSON())
		usages := func() map[string]*dockwellv1.ContainerUsage {
			t.Helper()
			res, err := servers.GetServer(t.Context(), connect.NewRequest(&dockwellv1.GetServerRequest{Id: server}))
			if err != nil {
				t.Fatal(err)
			}
			s := res.Msg.GetServer()
			found := map[string]*dockwellv1.ContainerUsage{}
			for _, st := range append(s.GetHistory(), s.GetStats()) {
				for _, u := range st.GetContainers() {
					found[u.GetServiceId()] = u
				}
			}
			return found
		}

		all := usages()
		if all[shopApp] == nil || all[blogApp] == nil {
			t.Fatalf("an admin reads %v, want both apps' figures", all)
		}

		aMember(t, f)(store.Grant{ProjectID: shopProject, Role: store.RoleViewer})
		theirs := usages()
		if len(theirs) != 1 || theirs[shopApp].GetMemoryBytes() != 100<<20 || theirs[shopApp].GetCpuPercent() != 25 {
			t.Errorf("a viewer of shop reads %v, want shop's container alone, as it is", theirs)
		}
		res, err := servers.GetServer(t.Context(), connect.NewRequest(&dockwellv1.GetServerRequest{Id: server}))
		if err != nil {
			t.Fatal(err)
		}
		for _, st := range append(res.Msg.GetServer().GetHistory(), res.Msg.GetServer().GetStats()) {
			if st == nil {
				continue
			}
			if st.GetCpuPercent() != 0 || st.GetMemoryTotalBytes() != 0 || st.GetMemoryUsedBytes() != 0 || st.GetTime() == nil {
				t.Errorf("a viewer reads a sample as %v, want its time and their apps alone", st)
			}
		}
	})
}
