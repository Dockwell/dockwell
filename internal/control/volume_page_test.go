// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A volume's page reads what it holds on its server, where its files are there, and
// which apps mount it, and never its options; with its server away, it says why it
// can't say what it holds, and the rest still answers.
func TestAVolumesPage(t *testing.T) { storetest.Each(t, testAVolumesPage) }

func testAVolumesPage(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	made, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Run shop with its data",
		Edits: []*dockwellv1.Edit{
			{
				Resource: &dockwellv1.ResourceRef{Kind: "volume", Name: "data"},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Volume{Volume: &resourcev1.Volume{}}},
			},
			{
				Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "shop"},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
					Image:  "ghcr.io/acme/shop:1.4",
					Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data"}},
				}}}}},
			},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, made.Msg.GetChange())
	res, err := dockwellv1connect.NewResourceServiceClient(b, b.origin).ListResources(t.Context(), connect.NewRequest(&dockwellv1.ListResourcesRequest{Kind: "volume"}))
	if err != nil || len(res.Msg.GetResources()) != 1 {
		t.Fatalf("ListResources = %v, %v", res, err)
	}
	vol := res.Msg.GetResources()[0]
	e.VolumeSizes = map[string]int64{docker.ScopedName(docker.Project(vol.GetResource().GetScope()), "data"): 52_428_800}
	vc := dockwellv1connect.NewVolumeServiceClient(b, b.origin, connect.WithProtoJSON())
	got, err := vc.GetVolume(t.Context(), connect.NewRequest(&dockwellv1.GetVolumeRequest{Id: vol.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	v := got.Msg.GetVolume()
	if v.GetDriver() != "local" || v.GetHasOptions() || v.GetResource().GetName() != "data" {
		t.Errorf("the volume reads as %v", v)
	}
	if len(v.GetServers()) != 1 || v.GetServers()[0].GetSizeBytes() != 52_428_800 || v.GetServers()[0].GetServerName() != "hz-1" ||
		!strings.HasSuffix(v.GetServers()[0].GetPath(), "/_data") || v.GetServers()[0].GetUnread() != "" {
		t.Errorf("on its server it reads as %v", v.GetServers())
	}
	if m := v.GetMountedBy(); len(m) != 1 || m[0].GetService() != "shop" || m[0].GetTarget() != "/data" || m[0].GetReadOnly() {
		t.Errorf("it's mounted by %v", m)
	}
	if _, err := vc.GetVolume(t.Context(), connect.NewRequest(&dockwellv1.GetVolumeRequest{Id: "vol_01JD6Z3R8M9QF2K7T4XW5BNC0V"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a volume that isn't there: %v", err)
	}
}
