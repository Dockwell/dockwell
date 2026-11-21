// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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
func TestAVolumesPage(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testAVolumesPage)
}

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
	name := docker.ScopedName(docker.Project(vol.GetResource().GetScope()), "data")
	e.VolumeSizes = map[string]int64{name: 52_428_800}
	release := make(chan struct{})
	releaseCount := sync.OnceFunc(func() { close(release) })
	defer releaseCount()
	e.Intercept(func(req *http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/system/df") {
			select {
			case <-release:
			case <-req.Context().Done():
			}
		}
		return nil
	})
	vc := dockwellv1connect.NewVolumeServiceClient(b, b.origin, connect.WithProtoJSON())
	read := func(ctx context.Context) *dockwellv1.VolumeInfo {
		t.Helper()
		got, err := vc.GetVolume(ctx, connect.NewRequest(&dockwellv1.GetVolumeRequest{Id: vol.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		return got.Msg.GetVolume()
	}
	quick, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pending := read(quick).GetServers()
	if len(pending) != 1 || pending[0].GetSizeBytes() != -1 || pending[0].GetUnread() != "" ||
		!strings.HasSuffix(pending[0].GetPath(), "/_data") {
		t.Fatalf("before disk accounting the volume reads as %v", pending)
	}
	releaseCount()

	var v *dockwellv1.VolumeInfo
	eventually(t, "the volume's size is measured", func() bool {
		v = read(t.Context())
		return len(v.GetServers()) == 1 && v.GetServers()[0].GetSizeBytes() >= 0
	})
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
	// A warm count still contains this volume. The page must follow the live
	// Engine once the volume is removed outside Dockwell.
	for _, name := range e.Containers() {
		e.Remove(name)
	}
	if err := e.Client().RemoveVolume(t.Context(), name); err != nil {
		t.Fatal(err)
	}
	missing := read(t.Context()).GetServers()
	if len(missing) != 1 || !strings.Contains(missing[0].GetUnread(), "has no volume") || missing[0].GetPath() != "" {
		t.Fatalf("after removal the volume reads as %v", missing)
	}
	if _, err := vc.GetVolume(t.Context(), connect.NewRequest(&dockwellv1.GetVolumeRequest{Id: "vol_01JD6Z3R8M9QF2K7T4XW5BNC0V"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a volume that isn't there: %v", err)
	}
}
