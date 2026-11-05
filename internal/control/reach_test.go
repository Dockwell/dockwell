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
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

const nasVolume = "volumes:\n  nas:\n    driver_opts: {type: none, o: bind, device: /srv/nas}\n"

func nasFile(mount string) string {
	return "services:\n  app:\n    image: ghcr.io/acme/shop:1.4\n    volumes: [\"nas:/nas" + mount + "\"]\n" + nasVolume
}

// What a read-write token can do once a person has approved one bind
// volume, read-only in its one service.
func TestTokenReach(t *testing.T) { storetest.Each(t, testTokenReach) }

func testTokenReach(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	auth := bearer{token: token, rt: b.client.Transport}
	script := changeClient(auth, b.origin)
	scc := dockwellv1connect.NewComposeServiceClient(auth, b.origin, connect.WithProtoJSON())

	first := importCompose(t, cc, nasFile(":ro"), nil, false)
	if !first.GetPlan().GetHostAccess() {
		t.Fatalf("the bind volume plans no host access: %v", first.GetPlan())
	}
	w := watch(t, c, first.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: first.GetId(), Fingerprint: first.GetPlan().GetFingerprint()})); err != nil {
		t.Fatal(err)
	}
	if got := waitWatch(t, w).last(); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the person's import = %v", got)
	}
	var env string
	for _, r := range first.GetPlan().GetResources() {
		env = r.GetResource().GetScope()
	}
	project := docker.Project(env)
	appName := service.ContainerName(resource.Ref{Kind: service.Kind, Scope: env, Name: "app"})
	readOnly := func(name, volume string) (mounted, ro bool) {
		for _, m := range e.Container(name).HostConfig.Mounts {
			if m.Source == volume {
				return true, m.ReadOnly
			}
		}
		return false, false
	}
	vol := docker.ScopedName(project, "nas")
	if ok, ro := readOnly(appName, vol); !ok || !ro {
		t.Fatalf("app mounts %s: %v, read-only %v", vol, ok, ro)
	}

	// A new tag for what already mounts it stays a token's to deploy.
	tag := importCompose(t, scc, strings.Replace(nasFile(":ro"), "shop:1.4", "shop:1.5", 1), nil, false)
	if tag.GetPlan().GetHostAccess() {
		t.Errorf("a new tag of a service that keeps its mount of a bind volume plans host access: %v", tag.GetPlan())
	}
	if got := apply(t, script, tag); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the token's new tag: %v", got)
	}

	// 1. Read-only to read-write on the same source.
	rw := importCompose(t, scc, nasFile(""), nil, false)
	if rw.GetPlan().GetHostAccess() {
		t.Log("1: read-write needs a person")
	} else {
		t.Error("1: a token's change of a bind volume's mount from read-only to read-write plans no host access")
		if got := apply(t, script, rw); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("1: %v", got)
		}
		_, ro := readOnly(appName, vol)
		t.Logf("1: the token applied it; read-only is now %v", ro)
	}

	// 2. The volume in a new service.
	file := "services:\n  app:\n    image: ghcr.io/acme/shop:1.4\n    volumes: [\"nas:/nas\"]\n  thief:\n    image: ghcr.io/acme/worker:1.0\n    volumes: [\"nas:/stolen\"]\n" + nasVolume
	two := importCompose(t, scc, file, nil, false)
	if two.GetPlan().GetHostAccess() {
		t.Log("2: a new service needs a person")
	} else {
		t.Error("2: a token mounting a bind volume in a new service plans no host access")
		if got := apply(t, script, two); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("2: %v", got)
		}
	}

	// 3. A container on its own, by the volume's name in Docker: outside
	// what ADR 0021 allows, "a service of its environment".
	pry := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "pry"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Container{Container: &resourcev1.Container{
			Image:  "ghcr.io/acme/worker:1.0",
			Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: vol}, Target: "/stolen"}},
		}}},
	}
	if _, err := script.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Run pry", Edits: []*dockwellv1.Edit{pry}})); err == nil ||
		!strings.Contains(err.Error(), "so a container on its own can't mount it") {
		t.Errorf("3: a token mounting an environment's bind volume in a container on its own = %v, want it refused", err)
	}

	// 4. A macvlan network.
	services, _, _ := strings.Cut(file, "volumes:\n  nas")
	mac := services + nasVolume + "networks:\n  lan:\n    driver: macvlan\n    driver_opts: {parent: eth0}\n  default: {}\n"
	mac = strings.Replace(mac, "image: ghcr.io/acme/worker:1.0\n", "image: ghcr.io/acme/worker:1.0\n    networks: [lan]\n", 1)
	net := importCompose(t, scc, mac, nil, false)
	if net.GetPlan().GetHostAccess() {
		t.Log("4: macvlan needs a person")
	} else {
		t.Error("4: a token's macvlan network plans no host access")
		if got := apply(t, script, net); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("4: %v", got)
		}
		n := e.Network(docker.ScopedName(project, "lan"))
		t.Logf("4: the token applied it; network %+v", n)
	}
}
