// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Every way a change can reach further over what a service writes asks for a person,
// and a token can't apply it: root however it's written, a number Docker can read as
// 0, a capability, another repository's code whatever the user, and what's written
// that isn't a server path. From #252's review.
func TestEveryWayToReachFurtherOverWhatsWrittenAsks(t *testing.T) {
	storetest.Each(t, testEveryWayToReachFurtherOverWhatsWrittenAsks)
}

func testEveryWayToReachFurtherOverWhatsWrittenAsks(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	registerImages(e)
	c := changeClient(b, b.origin)
	script := changeClient(bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}, b.origin)
	dirs := map[string]string{}
	for _, n := range []string{"data", "data2", "data3", "ext"} {
		dirs[n] = filepath.Join(t.TempDir(), n)
		if err := os.MkdirAll(dirs[n], 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.AddVolume(docker.Volume{Name: "extvol", Driver: "local", Options: map[string]string{"type": "none", "o": "bind", "device": dirs["ext"]}})
	hp := func(n string) *resourcev1.Mount {
		return &resourcev1.Mount{Source: &resourcev1.Mount_HostPath{HostPath: dirs[n]}, Target: "/m/" + n}
	}
	vol := func(n string, ext bool) *resourcev1.Mount {
		return &resourcev1.Mount{Source: &resourcev1.Mount_Volume{Volume: n}, Target: "/v/" + n, External: ext}
	}
	type svc struct {
		name, image, user string
		mounts            []*resourcev1.Mount
		capDrop           []string
	}
	edit := func(s svc, change func(*resourcev1.Container)) *dockwellv1.Edit {
		ed := serviceEdit(s.name, s.image, nil)
		app := ed.GetSpec().GetService().GetApp()
		app.User = s.user
		app.CapDrop = s.capDrop
		for _, m := range s.mounts {
			app.Mounts = append(app.Mounts, proto.Clone(m).(*resourcev1.Mount))
		}
		if change != nil {
			change(app)
		}
		return ed
	}
	volEdit := func(name string, v *resourcev1.Volume) *dockwellv1.Edit {
		return &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "volume", Name: name}, Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Volume{Volume: v}}}
	}
	web := svc{"web", "ghcr.io/acme/web:1.0", "1000", []*resourcev1.Mount{hp("data")}, nil}
	two := svc{"two", "ghcr.io/acme/web:1.0", "1000", []*resourcev1.Mount{hp("data"), hp("data2")}, nil}
	rootA := svc{"root-a", "ghcr.io/acme/web:1.0", "0", []*resourcev1.Mount{hp("data")}, nil}
	rootDrop := svc{"root-drop", "ghcr.io/acme/web:1.0", "0", []*resourcev1.Mount{hp("data")}, []string{"ALL"}}
	numImg := svc{"num-img", "ghcr.io/acme/web:1.0", "1000", []*resourcev1.Mount{hp("data")}, nil}
	nfs := svc{"nfs", "ghcr.io/acme/web:1.0", "1000", []*resourcev1.Mount{vol("share", false)}, nil}
	ext := svc{"ext", "ghcr.io/acme/web:1.0", "1000", []*resourcev1.Mount{vol("extvol", true)}, nil}
	edits := []*dockwellv1.Edit{
		edit(web, nil), edit(two, nil), edit(rootA, nil), edit(rootDrop, nil), edit(numImg, nil), edit(nfs, nil), edit(ext, nil),
		volEdit("share", &resourcev1.Volume{Options: map[string]string{"type": "nfs", "o": "addr=10.0.0.2,rw", "device": ":/export"}}),
		volEdit("files2", &resourcev1.Volume{Options: map[string]string{"type": "none", "o": "bind", "device": dirs["data3"]}}),
	}
	applyConfirmed(t, c, propose(t, c, "Set up", edits...))

	try := func(what string, ed *dockwellv1.Edit) (asks bool, plan *dockwellv1.Plan) {
		t.Helper()
		planned := proposeEdit(t, script, ed)
		plan = planned.GetPlan()
		if plan.GetHostAccess() {
			_, err := script.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: planned.GetId(), Fingerprint: plan.GetFingerprint()}))
			if err == nil {
				t.Errorf("%s: plan has host access, and a token applied it", what)
			}
			if _, err := c.DiscardChange(t.Context(), connect.NewRequest(&dockwellv1.DiscardChangeRequest{Id: planned.GetId()})); err != nil {
				t.Fatal(err)
			}
			return true, plan
		}
		if got := apply(t, script, planned); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Errorf("%s: a token's apply ended %v: %s", what, got.GetState(), got.GetError())
		}
		return false, plan
	}
	details := func(plan *dockwellv1.Plan) []string {
		var out []string
		for _, r := range plan.GetResources() {
			for _, a := range r.GetHostAccess() {
				out = append(out, fmt.Sprintf("%s:%v:%s:ro=%t:%s", r.GetResource().GetName(), a.GetKind(), a.GetPath(), a.GetReadOnly(), a.GetDetail()))
			}
		}
		return out
	}
	report := func(what string, ed *dockwellv1.Edit, wantAsk bool) {
		t.Helper()
		if asks, plan := try(what, ed); asks != wantAsk {
			t.Errorf("%s: asks for a person = %t, want %t: %v", what, asks, wantAsk, details(plan))
		}
	}

	// 1. Spellings of root, through a host path. Want: asks.
	for _, u := range []string{"-0", " 0", "0x0", "0:1000", ":0", "root:root", "+0", "18446744073709551616"} {
		report("web user "+fmt.Sprintf("%q", u), edit(web, func(a *resourcev1.Container) { a.User = u }), true)
	}
	// A uid the image can list, which Docker casts to uint32: 2^32 and -2^32 are 0.
	for _, u := range []string{"4294967296", "-4294967296", "4294967296:0"} {
		report("web user (wraps to 0 if image lists it) "+u, edit(web, func(a *resourcev1.Container) { a.User = u }), true)
	}
	// 2. Several mounts at once.
	report("two folders, to root", edit(two, func(a *resourcev1.Container) { a.User = "0" }), true)
	// 3. Root already, gaining a second folder, directly and by a bind volume.
	report("root-a gains data2", edit(rootA, func(a *resourcev1.Container) { a.Mounts = append(a.Mounts, hp("data2")) }), true)
	report("root-a gains files2 (bind volume)", edit(rootA, func(a *resourcev1.Container) { a.Mounts = append(a.Mounts, vol("files2", false)) }), true)
	report("root-a: data ro->rw stays root", edit(rootA, func(a *resourcev1.Container) { a.Mounts[0].Target = "/elsewhere" }), false)
	// 4. Capabilities.
	for _, cp := range []string{"CAP_SETGID", "DAC_OVERRIDE", "cap_chown", "FOWNER", "SETUID", "ALL"} {
		report("web cap_add "+cp, edit(web, func(a *resourcev1.Container) { a.CapAdd = []string{cp} }), true)
	}
	report("web privileged", edit(web, func(a *resourcev1.Container) { a.Privileged = true }), true)
	report("web group_add 0", edit(web, func(a *resourcev1.Container) { a.GroupAdd = []string{"0"} }), true)
	// Left out, as ADR 0032 says: root's words already promise it can change any file.
	report("root-drop: removes cap_drop ALL", edit(rootDrop, func(a *resourcev1.Container) { a.CapDrop = nil }), false)
	// 5. Code that writes as a number other than 0, from another repository: a set-uid
	// file in the image runs as root, as Dockwell sets no no-new-privileges.
	report("num-img: another repository, still user 1000", edit(numImg, func(a *resourcev1.Container) { a.Image = "ghcr.io/stranger/ops:1.0" }), true)
	// 6. Root approved, then another repository's code as root over the same folder.
	report("root-a: another repository, still root", edit(rootA, func(a *resourcev1.Container) { a.Image = "ghcr.io/stranger/ops:1.0" }), true)
	// 7. Mounts that aren't AccessPath.
	report("nfs volume, to root", edit(nfs, func(a *resourcev1.Container) { a.User = "0" }), true)
	report("external bind volume, to root", edit(ext, func(a *resourcev1.Container) { a.User = "0" }), true)
}
