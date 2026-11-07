// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A person approves a folder for what runs there. Root writes what anyone owns in
// it, and leaves files that run as root, so a token that moves a service with a
// read-write folder to root reaches further than what was approved, however the
// user is written: Docker reads 00 and +0 as 0. So does one that moves it to a user
// only its image can say isn't root, or that adds a capability. Any image can hold
// a program that runs as root, so so does code from another repository, whatever
// the user (#247).
func TestWhoWritesAFolderIsOutOfATokensReach(t *testing.T) {
	storetest.Each(t, testWhoWritesAFolderIsOutOfATokensReach)
}

func testWhoWritesAFolderIsOutOfATokensReach(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	registerImages(f.addDockerServer(t))
	c := changeClient(b, b.origin)
	script := changeClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}, b.origin)
	data, config := filepath.Join(t.TempDir(), "data"), filepath.Join(t.TempDir(), "config")
	for _, dir := range []string{data, config} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	site := func(name, user string, mounts ...*resourcev1.Mount) func(func(*resourcev1.Container)) *dockwellv1.Edit {
		return func(change func(*resourcev1.Container)) *dockwellv1.Edit {
			edit := serviceEdit(name, "ghcr.io/acme/web:1.0", nil)
			app := edit.GetSpec().GetService().GetApp()
			app.User = user
			for _, m := range mounts {
				app.Mounts = append(app.Mounts, proto.Clone(m).(*resourcev1.Mount))
			}
			if change != nil {
				change(app)
			}
			return edit
		}
	}
	folder := &resourcev1.Mount{Source: &resourcev1.Mount_HostPath{HostPath: data}, Target: "/var/www"}
	readOnly := &resourcev1.Mount{Source: &resourcev1.Mount_HostPath{HostPath: config}, Target: "/etc/app", ReadOnly: true}
	throughVolume := &resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: "files"}, Target: "/srv/files"}
	files := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "volume", Name: "files"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Volume{Volume: &resourcev1.Volume{
			Options: map[string]string{"type": "none", "o": "bind", "device": data},
		}}},
	}
	web := site("web", "1000", folder)
	byVolume := site("by-volume", "1000", throughVolume)
	imageUser := site("image-user", "", folder)
	asRoot := site("as-root", "0", folder)
	readsOnly := site("reads-only", "1000", readOnly)
	applyConfirmed(t, c, propose(t, c, "Run the sites", web(nil), byVolume(nil), imageUser(nil), asRoot(nil), readsOnly(nil), files))

	refused := func(what string, edit *dockwellv1.Edit, says string) {
		t.Helper()
		planned := proposeEdit(t, script, edit)
		tokenRefused(t, script, planned, edit.GetResource().GetName())
		// says is who writes there, or "-" for what adds no one, as a capability.
		if says != "" && says != "-" && !reaches(planned.GetPlan(), says) {
			t.Errorf("%s: the plan doesn't say who writes there: %v", what, planned.GetPlan().GetResources())
		}
		image := edit.GetSpec().GetService().GetApp().GetImage()
		if !slices.ContainsFunc(planned.GetPlan().GetResources(), func(r *dockwellv1.PlannedResource) bool {
			return slices.ContainsFunc(r.GetHostAccess(), func(a *dockwellv1.HostAccess) bool {
				return a.GetWrittenBy() != "" && strings.HasPrefix(image, a.GetWrittenBy())
			})
		}) && says != "-" {
			t.Errorf("%s: the plan doesn't say whose code writes there: %v", what, planned.GetPlan().GetResources())
		}
		if slices.ContainsFunc(planned.GetPlan().GetResources(), func(r *dockwellv1.PlannedResource) bool {
			return slices.ContainsFunc(r.GetHostAccess(), (*dockwellv1.HostAccess).GetSameAsRoot)
		}) {
			t.Errorf("%s: it asks for a passkey, which a folder that isn't the system's doesn't", what)
		}
		if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: planned.GetId()})); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []string{"0", "00", "000", "+0", "0:0", "00:1000", ":1000", "root", "root:root"} {
		refused("web as "+user, web(func(a *resourcev1.Container) { a.User = user }), resource.WrittenAsRoot)
		refused("by-volume as "+user, byVolume(func(a *resourcev1.Container) { a.User = user }), resource.WrittenAsRoot)
	}
	refused("web as its image's user", web(func(a *resourcev1.Container) { a.User = "" }), resource.WrittenAsImage)
	refused("web as a user by name", web(func(a *resourcev1.Container) { a.User = "www-data" }), resource.WrittenAsNamed)
	refused("another image, as its user", imageUser(func(a *resourcev1.Container) { a.Image = "ghcr.io/stranger/ops:1.0" }), resource.WrittenAsImage)
	refused("image-user as root", imageUser(func(a *resourcev1.Container) { a.User = "0" }), resource.WrittenAsRoot)
	for _, capability := range []string{"CAP_DAC_OVERRIDE", "CAP_CHOWN", "CAP_FOWNER", "CAP_SETUID"} {
		refused("web with "+capability, web(func(a *resourcev1.Container) { a.CapAdd = []string{capability} }), "-")
	}
	// A set-uid program in another repository's image runs as root, whatever the user.
	refused("another repository, still a number", web(func(a *resourcev1.Container) { a.Image = "ghcr.io/stranger/ops:1.0" }), "")
	refused("root approved, then another repository", asRoot(func(a *resourcev1.Container) { a.Image = "ghcr.io/stranger/ops:1.0" }), resource.WrittenAsRoot)
	refused("another repository, through a bind volume", byVolume(func(a *resourcev1.Container) { a.Image = "ghcr.io/stranger/ops:1.0" }), "")

	// What reaches no further goes on as a token's: another user that isn't root, a
	// tag of the same image, root that was approved, root taken away, and root over a
	// folder it only reads. In this order, as each goes on from the one before.
	for _, c := range []struct {
		what string
		edit *dockwellv1.Edit
	}{
		{"another user", web(func(a *resourcev1.Container) { a.User = "1001:1001" })},
		{"another tag", imageUser(func(a *resourcev1.Container) { a.Image = "ghcr.io/acme/web:1.1" })},
		{"a user by number, from the image's", imageUser(func(a *resourcev1.Container) { a.User = "1000" })},
		{"root, approved", asRoot(func(a *resourcev1.Container) { a.User = "root"; a.Image = "ghcr.io/acme/web:1.2" })},
		{"the image's user, from root", asRoot(func(a *resourcev1.Container) { a.User = "" })},
		{"root, taken away", asRoot(func(a *resourcev1.Container) { a.User = "1000" })},
		{"root over what it reads", readsOnly(func(a *resourcev1.Container) { a.User = "0" })},
	} {
		what, edit := c.what, c.edit
		planned := proposeEdit(t, script, edit)
		if planned.GetPlan().GetHostAccess() {
			t.Errorf("%s needs a person: %v", what, planned.GetPlan().GetResources())
			continue
		}
		if got := apply(t, script, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Errorf("a token's %s ended %v: %s", what, got.GetState(), got.GetError())
		}
	}
}

// reaches reports a plan that says a resource writes a server path as written.
func reaches(plan *dockwellv1.Plan, written string) bool {
	return slices.ContainsFunc(plan.GetResources(), func(r *dockwellv1.PlannedResource) bool {
		return slices.ContainsFunc(r.GetHostAccess(), func(a *dockwellv1.HostAccess) bool {
			return a.GetKind() == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PATH && a.GetDetail() == written && !a.GetReadOnly()
		})
	})
}
