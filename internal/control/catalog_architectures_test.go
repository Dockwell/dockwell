// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
)

// archEntry is the test entry at a release, saying the architectures it runs on.
func archEntry(release int, version, image string, arch ...string) string {
	e := testEntry(release, version, image, "")
	if len(arch) == 0 {
		return e
	}
	return strings.Replace(e, "compose: |", "architectures:\n  - "+strings.Join(arch, "\n  - ")+"\ncompose: |", 1)
}

// otherArch is the architecture this machine's agent doesn't report.
func otherArch() string {
	if runtime.GOARCH == "arm64" {
		return "amd64"
	}
	return "arm64"
}

// An app its catalog says runs on one architecture goes only on a server of it: the
// install's services carry the entry's architectures, and the engine refuses any plan
// that puts one on a server reporting another, or none yet, saying which and what to
// do: an install, a move, an upgrade whose release drops the architecture, and an
// undo all meet the one rule. An app that says none runs anywhere, as before (ADR
// 0102).
func TestAnAppRunsOnlyWhereItsArchitectureRuns(t *testing.T) {
	t.Parallel()
	here, other := runtime.GOARCH, otherArch()
	u := startUpgrading(t, store.SQLite, archEntry(1, "1.0", firstRelease, here))
	spec := func() (*resourcev1.Service, store.Resource) {
		t.Helper()
		var svc *resourcev1.Service
		var r store.Resource
		err := u.f.store.View(t.Context(), func(tx *store.Tx) error {
			var err error
			if r, err = tx.GetResource(t.Context(), string(service.Kind), u.env, "web"); err != nil {
				return err
			}
			m, err := u.f.keeper.Decode(t.Context(), tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			svc, _ = m.(*resourcev1.Service)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return svc, r
	}
	if svc, _ := spec(); !slices.Equal(svc.GetArchitectures(), []string{here}) {
		t.Fatalf("web runs on %v, want %s, as its entry says", svc.GetArchitectures(), here)
	}
	if web := u.web(); web == nil {
		t.Fatal("web doesn't run")
	}

	// An install of an entry that runs on the other architecture alone is refused.
	elsewhere := strings.Replace(archEntry(1, "1.0", firstRelease, other), "id: thing", "id: elsewhere", 1)
	if err := os.WriteFile(filepath.Join(u.dir, "elsewhere.yaml"), []byte(elsewhere), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := u.cat.InstallCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.InstallCatalogEntryRequest{Id: "elsewhere", Values: map[string]string{"PORT": "18101"}}))
	if err == nil || !strings.Contains(err.Error(), "web runs on "+other+" alone") || !strings.Contains(err.Error(), "is "+here+", so its images have no build there") {
		t.Errorf("installing an app of the other architecture: %v", err)
	}

	// Two more servers: one that reports the other architecture, and one that hasn't
	// reported yet.
	now := time.Now()
	inv, err := proto.Marshal(&dockwellv1.ServerInventory{Architecture: other})
	if err != nil {
		t.Fatal(err)
	}
	err = u.f.store.Update(t.Context(), func(tx *store.Tx) error {
		for _, s := range []store.Server{{ID: "srv_other", Name: "edge-1"}, {ID: "srv_silent", Name: "edge-2"}} {
			s.CreatedAt, s.CertSerial, s.CertNotAfter = now, s.ID, now.Add(time.Hour)
			if err := tx.CreateServer(t.Context(), s); err != nil {
				return err
			}
		}
		return tx.SaveServerReport(t.Context(), "srv_other", store.ServerReport{Inventory: inv}, now)
	})
	if err != nil {
		t.Fatal(err)
	}
	move := func(server string) error {
		svc, r := spec()
		_, err := u.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Move web", Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: u.env, Name: "web"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: svc}}, Servers: []string{server}, BaseRevision: int64(r.Revision),
		}}}))
		return err
	}
	if err := move("srv_other"); err == nil || !strings.Contains(err.Error(), "web runs on "+here+" alone, and edge-1 is "+other+", so its images have no build there and it wouldn't start. Put web on") {
		t.Errorf("moving web to a server of the other architecture: %v", err)
	}
	if err := move("srv_silent"); err == nil || !strings.Contains(err.Error(), "web runs on "+here+" alone, and edge-2 hasn't said its architecture yet") {
		t.Errorf("moving web to a server that hasn't said its architecture: %v", err)
	}

	// A release that runs on the other architecture alone can't be planned where web
	// runs now.
	u.release(archEntry(2, "2.0", secondRelease, other))
	if _, err := u.cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "thing"})); err == nil || !strings.Contains(err.Error(), "web runs on "+other+" alone") {
		t.Errorf("an upgrade to a release that drops this architecture: %v", err)
	}

	// A release that runs anywhere takes the limit away, so web may go where it's asked,
	// and an undo of that upgrade puts the limit back, which the engine checks again.
	u.release(archEntry(3, "3.0", secondRelease))
	upgraded := u.upgrade()
	u.applied(upgraded)
	if svc, _ := spec(); len(svc.GetArchitectures()) != 0 {
		t.Errorf("after a release that runs anywhere, web runs on %v", svc.GetArchitectures())
	}
	undo, err := u.c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: upgraded.GetId()}))
	if err != nil {
		t.Fatalf("undoing the upgrade: %v", err)
	}
	u.applied(undo.Msg.GetChange())
	if svc, _ := spec(); !slices.Equal(svc.GetArchitectures(), []string{here}) {
		t.Errorf("after the undo, web runs on %v, want %s", svc.GetArchitectures(), here)
	}
}

// archSpec is web's spec as stored, and the resource it's in.
func archSpec(t *testing.T, u *upgrading) (*resourcev1.Service, store.Resource) {
	t.Helper()
	var svc *resourcev1.Service
	var r store.Resource
	err := u.f.store.View(t.Context(), func(tx *store.Tx) error {
		var err error
		if r, err = tx.GetResource(t.Context(), string(service.Kind), u.env, "web"); err != nil {
			return err
		}
		m, err := u.f.keeper.Decode(t.Context(), tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		svc, _ = m.(*resourcev1.Service)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, r
}

// archEdit proposes web's spec as given, on servers where named.
func archEdit(t *testing.T, u *upgrading, svc *resourcev1.Service, base int, servers ...string) (*dockwellv1.Change, error) {
	t.Helper()
	res, err := u.c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Edit web", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: u.env, Name: "web"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: svc}}, Servers: servers, BaseRevision: int64(base),
	}}}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetChange(), nil
}

// A plain edit of a one-architecture service that stays where it runs plans though
// its server has no report, as after a restore that keeps none: only where it's
// newly placed, or its list changes, is a missing report refused (#570's arm64
// review, M1).
func TestAPlainEditWithNoReportPlans(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, archEntry(1, "1.0", firstRelease, runtime.GOARCH))
	err := u.f.store.Update(t.Context(), func(tx *store.Tx) error {
		list, err := tx.ListServers(t.Context())
		if err != nil {
			return err
		}
		for _, s := range list {
			if err := tx.SaveServerReport(t.Context(), s.ID, store.ServerReport{}, time.Now()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, r := archSpec(t, u)
	svc.GetApp().Env = map[string]string{"REVIEW": "1"}
	if _, err := archEdit(t, u, svc, r.Revision); err != nil {
		t.Errorf("a plain edit that leaves web where it runs, on a server with no report yet: %v", err)
	}
	// A list that changes is checked, report or none.
	svc.Architectures = []string{otherArch()}
	if _, err := archEdit(t, u, svc, r.Revision); err == nil || !strings.Contains(err.Error(), "hasn't said its architecture yet") {
		t.Errorf("an edit that changes web's architectures, on a server with no report: %v", err)
	}
}

// An edit that leaves a service's architectures out, as a script, a file written by
// hand or a client older than the field sends, keeps them: they're a fact about its
// images, so the plan says nothing changes there, and web still runs only where it
// did (#570's arm64 review, M2). A release that runs anywhere clears them, as the
// architecture test shows.
func TestAnEditThatLeavesArchitecturesOutKeepsThem(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, archEntry(1, "1.0", firstRelease, runtime.GOARCH))
	svc, r := archSpec(t, u)
	svc.Architectures = nil
	svc.GetApp().Env = map[string]string{"REVIEW": "1"}
	ch, err := archEdit(t, u, svc, r.Revision)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range ch.GetPlan().GetResources() {
		for _, c := range it.GetChanges() {
			if strings.HasPrefix(c.GetPath(), "architectures") {
				t.Errorf("the plan changes %s, which the edit left out", c.GetPath())
			}
		}
	}
	u.applied(ch)
	if got, _ := archSpec(t, u); !slices.Equal(got.GetArchitectures(), []string{runtime.GOARCH}) {
		t.Errorf("after an edit that left architectures out, web runs on %v, want %s kept", got.GetArchitectures(), runtime.GOARCH)
	}
}
