// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent/enroll"
	"github.com/dockwell/dockwell/internal/backup"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/pki"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

type storageFixture struct {
	full    *fullServer
	person  *browser
	api     dockwellv1connect.DockerServiceClient
	changes dockwellv1connect.ChangeServiceClient
	server  *testServer
	engine  *dockertest.Engine
}

func storageControl(t *testing.T, backend store.Backend, client *docker.Client, helper string) *storageFixture {
	t.Helper()
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	var fake *dockertest.Engine
	if client == nil {
		fake = f.addDockerServer(t)
	} else {
		key, csr, err := pki.NewServerKey()
		if err != nil {
			t.Fatal(err)
		}
		joined, err := f.ident.EnrollLocal(t.Context(), csr, "fixture-engine")
		if err != nil {
			t.Fatal(err)
		}
		runtime := &container.Docker{Client: client, RunStateDir: t.TempDir(), Settle: 20 * time.Millisecond, Poll: 20 * time.Millisecond, Soak: -1}
		ctr := container.Driver{Runtime: runtime}
		backups := &backup.Runner{Client: client, Root: t.TempDir(), Binary: helper, Work: 1000, RunControl: runtime}
		s := &testServer{
			id:     enroll.New(joined.Server.ID, joined.Server.Name, "https://"+f.agentAddr, key, joined.Cert, f.ident.CA().Certificate()),
			client: client, backups: backups,
			drivers: resource.NewRegistry(&ctr, &service.Driver{Containers: ctr}, &network.Driver{Client: client},
				&volume.Driver{Runtime: &volume.Docker{Client: client, Kept: &volume.Kept{}}, BacksUp: backups}),
		}
		s.connect(t, f)
		f.lastDocker = s
	}
	return &storageFixture{
		full: f, person: b, server: f.lastDocker, engine: fake,
		api: dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON()), changes: changeClient(b, b.origin),
	}
}

func storageScope(t *testing.T, change *v1.Change, kind, name string) *v1.PlannedResource {
	t.Helper()
	for _, item := range change.GetPlan().GetResources() {
		if item.GetResource().GetKind() == kind && item.GetResource().GetName() == name {
			return item
		}
	}
	t.Fatalf("%s %s is missing from the plan", kind, name)
	return nil
}

func (s *storageFixture) workload(t *testing.T, scope string) *docker.Container {
	t.Helper()
	const name = "web"
	client := s.server.client
	project := docker.Project(scope)
	all, err := client.ListContainers(t.Context(), map[string][]string{"label": {docker.LabelProject + "=" + project, docker.LabelService + "=" + name}})
	if err != nil || len(all) != 1 {
		t.Fatalf("inspect %s/%s: %v, %v", scope, name, all, err)
	}
	held, err := client.InspectContainer(t.Context(), all[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	s.full.agents.ReportHealth(s.server.id.ServerID, &agentv1.Health{ScannedAt: timestamppb.Now(), Instances: []*v1.InstanceHealth{{
		Project: project, Service: name, ContainerId: held.ID, Container: strings.TrimPrefix(held.Name, "/"),
	}}})
	return held
}

func (s *storageFixture) guardedNetwork(t *testing.T, scope string) *v1.DockerObjectKey {
	t.Helper()
	const name = "back"
	done := apply(t, s.changes, propose(t, s.changes, "Create network", &v1.Edit{
		Resource: &v1.ResourceRef{Kind: "network", Scope: scope, Name: name}, Servers: []string{s.server.id.ServerID}, MustCreate: true,
		Spec: &spec.Spec{Kind: &spec.Spec_Network{Network: &spec.Network{Driver: "bridge"}}},
	}))
	if done.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("guarded network did not apply: %v", done)
	}
	engineName := network.TargetOf(resource.Ref{Kind: network.Kind, Scope: scope, Name: name}).Name
	held, err := s.server.client.NetworkNamed(t.Context(), engineName)
	if err != nil || held.Labels[docker.LabelStorageCreation] == "" {
		t.Fatalf("guarded network is missing: %v, %v", held, err)
	}
	return &v1.DockerObjectKey{ServerId: s.server.id.ServerID, Kind: v1.DockerObjectKind_DOCKER_OBJECT_KIND_NETWORK, EngineId: held.ID}
}

func (s *storageFixture) attachment(t *testing.T, net *v1.DockerObjectKey, held *docker.Container, action v1.DockerNetworkAttachmentAction) *v1.PlanDockerNetworkAttachmentRequest {
	t.Helper()
	key := &v1.DockerObjectKey{ServerId: s.server.id.ServerID, Kind: v1.DockerObjectKind_DOCKER_OBJECT_KIND_CONTAINER, EngineId: held.ID}
	got, err := s.api.GetDockerObject(t.Context(), connect.NewRequest(&v1.GetDockerObjectRequest{Key: key}))
	if err != nil || got.Msg.GetObject().GetManagedRevision() <= 0 {
		t.Fatalf("managed container: %v, %v", got, err)
	}
	return &v1.PlanDockerNetworkAttachmentRequest{Network: net, Container: key, BaseRevision: got.Msg.GetObject().GetManagedRevision(), Action: action}
}

func (s *storageFixture) planAttachment(t *testing.T, req *v1.PlanDockerNetworkAttachmentRequest) *v1.Change {
	t.Helper()
	res, err := s.api.PlanDockerNetworkAttachment(t.Context(), connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.GetChange()
}

const (
	connectStorage    = v1.DockerNetworkAttachmentAction_DOCKER_NETWORK_ATTACHMENT_ACTION_CONNECT
	disconnectStorage = v1.DockerNetworkAttachmentAction_DOCKER_NETWORK_ATTACHMENT_ACTION_DISCONNECT
)

func TestDockerAttachmentKeepsThePanelAndCatalogWorkloadsOnBridge(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "thing.yaml"), []byte(testEntry(1, "1.0", firstRelease, "      network_mode: bridge\n")), 0o600); err != nil {
			t.Fatal(err)
		}
		useCatalogDir(t, dir)
		s := storageControl(t, b, nil, "")
		pullsAnything(s.engine)
		for _, origin := range []string{"panel", "catalog"} {
			t.Run(origin, func(t *testing.T) {
				var made *v1.Change
				if origin == "panel" {
					made = apply(t, s.changes, propose(t, s.changes, "Run workload", runImage("ghcr.io/acme/shop:1.4")))
				} else {
					made = apply(t, s.changes, installApp(t, catalogClient(s.person), &v1.InstallCatalogEntryRequest{Id: "thing", Project: "fixture-catalog"}))
				}
				scope := storageScope(t, made, "service", "web").GetResource().GetScope()
				held := s.workload(t, scope)
				n := s.guardedNetwork(t, scope)
				req := s.attachment(t, n, held, connectStorage)
				plan := s.planAttachment(t, req)
				var shown []string
				for _, server := range plan.GetPlan().GetServers() {
					for _, step := range server.GetSteps() {
						for _, field := range step.GetChanges() {
							if strings.Contains(field.GetPath(), "network") {
								shown = append(shown, field.GetTo())
							}
						}
					}
				}
				words := strings.Join(shown, " ")
				if !strings.Contains(words, "bridge") || !strings.Contains(words, "back") || strings.Contains(words, "default") {
					t.Fatalf("reviewed memberships = %q", words)
				}
				done := apply(t, s.changes, plan)
				after := s.workload(t, scope)
				want := []string{"bridge", network.TargetOf(resource.Ref{Kind: network.Kind, Scope: scope, Name: "back"}).Name}
				slices.Sort(want)
				if done.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED || !slices.Equal(slices.Sorted(maps.Keys(after.NetworkSettings.Networks)), want) {
					t.Fatalf("apply %v, memberships %v, want %v", done.GetError(), after.NetworkSettings.Networks, want)
				}
				if _, err := s.api.PlanDockerNetworkAttachment(t.Context(), connect.NewRequest(req)); err == nil {
					t.Fatal("the old container ID and revision can still plan a change")
				}
			})
		}
	})
}

func TestDockerStorageControlGuards(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		s := storageControl(t, b, nil, "")
		made := apply(t, s.changes, propose(t, s.changes, "Run workload", runImage("ghcr.io/acme/shop:1.4")))
		scope := storageScope(t, made, "service", "web").GetResource().GetScope()
		held := s.workload(t, scope)
		n := s.guardedNetwork(t, scope)
		base := s.attachment(t, n, held, connectStorage)
		stale := &v1.PlanDockerNetworkAttachmentRequest{Network: n, Container: base.Container, BaseRevision: base.BaseRevision + 1, Action: connectStorage}
		if _, err := s.api.PlanDockerNetworkAttachment(t.Context(), connect.NewRequest(stale)); err == nil {
			t.Fatal("a stale base planned an attachment")
		}
		external := s.engine.AddContainer(docker.Container{Name: "fixture-external"})
		outside := &v1.PlanDockerNetworkAttachmentRequest{Network: n, Container: &v1.DockerObjectKey{ServerId: n.ServerId, Kind: v1.DockerObjectKind_DOCKER_OBJECT_KIND_CONTAINER, EngineId: external.ID}, BaseRevision: 1, Action: connectStorage}
		if _, err := s.api.PlanDockerNetworkAttachment(t.Context(), connect.NewRequest(outside)); err == nil || !strings.Contains(err.Error(), "Take on") {
			t.Fatalf("outside container plan: %v", err)
		}
		candidate := &v1.DockerNetworkCandidate{EnvironmentId: scope, Name: "back", Spec: &spec.Network{Driver: "bridge"}}
		checked, err := s.api.CheckDockerNetwork(t.Context(), connect.NewRequest(&v1.CheckDockerNetworkRequest{ServerIds: []string{n.ServerId}, Candidate: candidate}))
		if err != nil || len(checked.Msg.GetFindings()) != 1 || checked.Msg.GetFindings()[0].GetCode() != v1.DockerNetworkCheckCode_DOCKER_NETWORK_CHECK_CODE_NAME_IN_USE {
			t.Fatalf("check existing name = %v, %v", checked, err)
		}
		create := &v1.Edit{
			Resource: &v1.ResourceRef{Kind: "network", Scope: scope, Name: "back"}, Servers: []string{n.ServerId}, MustCreate: true,
			Spec: &spec.Spec{Kind: &spec.Spec_Network{Network: &spec.Network{Driver: "bridge"}}},
		}
		if _, err := s.changes.ProposeChange(t.Context(), connect.NewRequest(&v1.ProposeChangeRequest{Edits: []*v1.Edit{create}})); err == nil {
			t.Fatal("must_create accepted an existing resource")
		}

		netName := network.TargetOf(resource.Ref{Kind: network.Kind, Scope: scope, Name: "back"}).Name
		var reads atomic.Int32
		s.engine.Intercept(func(r *http.Request) error {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/networks/"+n.EngineId) && reads.Add(1) == 2 {
				s.engine.AddNetwork(docker.Network{Name: netName, Driver: "bridge", Scope: "local"})
			}
			return nil
		})
		if _, err := s.api.PlanDockerNetworkAttachment(t.Context(), connect.NewRequest(base)); err == nil || !strings.Contains(err.Error(), "reviewed network changed") {
			t.Fatalf("a same-name replacement reached the plan: %v", err)
		}
	})
}

func TestDockerAttachmentResumesAfterItsContainerWasReplaced(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		s := storageControl(t, b, nil, "")
		made := apply(t, s.changes, propose(t, s.changes, "Run workload", runImage("ghcr.io/acme/shop:1.4")))
		scope := storageScope(t, made, "service", "web").GetResource().GetScope()
		net := s.guardedNetwork(t, scope)
		before := s.workload(t, scope)
		approved := s.planAttachment(t, s.attachment(t, net, before, connectStorage))
		driver, err := s.server.drivers.Driver(service.Kind)
		if err != nil {
			t.Fatal(err)
		}
		runtime := driver.(*service.Driver).Containers.Runtime.(*container.Docker)
		point := container.PointStarted
		for _, server := range approved.GetPlan().GetServers() {
			for _, step := range server.GetSteps() {
				if step.GetRollout().GetRolling() {
					point = container.PointRemoved
				}
			}
		}
		replaced, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		resume := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(resume)
		var reached atomic.Bool
		runtime.Points = func(name string) {
			if name == point && reached.CompareAndSwap(false, true) {
				close(replaced)
				<-release
			}
		}
		if _, err := applyChange(t, s.changes, &v1.ApplyChangeRequest{Id: approved.GetId(), Fingerprint: approved.GetPlan().GetFingerprint()}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-replaced:
		case <-time.After(20 * time.Second):
			t.Fatal("the replacement never finished")
		}
		if _, err := s.server.client.InspectContainer(t.Context(), before.ID); !docker.IsNotFound(err) {
			t.Fatalf("the previous container is still there: %v", err)
		}
		after := s.workload(t, scope)
		gone, cancel := context.WithCancel(t.Context())
		cancel()
		if err := s.full.engine.Stop(gone); err != nil {
			t.Fatal(err)
		}
		resume()
		record, err := s.full.engine.Change(t.Context(), approved.GetId())
		if err != nil || record.State != store.ChangeApplying {
			t.Fatalf("interrupted change: %v, %v", record, err)
		}
		next := revision.New(s.full.store, s.full.kinds, s.full.agents, s.full.keeper, slog.New(slog.DiscardHandler))
		if err := next.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
		next.Wait()
		record, err = next.Change(t.Context(), approved.GetId())
		if err != nil || record.State != store.ChangeApplied || s.workload(t, scope).ID != after.ID {
			t.Fatalf("resume did not finish that replacement: %v, %v", record, err)
		}
	})
}

func removeStorageFixture(t *testing.T, client *docker.Client, scope string, networks, volumes []string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		project := docker.Project(scope)
		listed, err := client.ListContainers(ctx, map[string][]string{"label": {docker.LabelProject + "=" + project}})
		if err != nil {
			t.Error(err)
		}
		for i := range listed {
			if err := client.RemoveContainer(ctx, listed[i].ID, true); err != nil && !docker.IsNotFound(err) {
				t.Error(err)
			}
		}
		for _, name := range networks {
			if err := client.RemoveNetwork(ctx, docker.ScopedName(project, name)); err != nil && !docker.IsNotFound(err) {
				t.Error(err)
			}
		}
		for _, name := range volumes {
			if err := client.RemoveVolume(ctx, docker.ScopedName(project, name)); err != nil && !docker.IsNotFound(err) {
				t.Error(err)
			}
		}
	})
}

func TestDockerNetworkAttachmentRevisionOnRealDocker(t *testing.T) {
	testDockerNetworkAttachmentRevision(t, storageControl(t, store.SQLite, dockertest.Real(t), ""))
}

func TestDockerNetworkAttachmentRevisionKeepsStoppedIntent(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		s := storageControl(t, b, nil, "")
		s.engine.AddLocalImage("busybox:1.37", dockertest.Image{})
		testDockerNetworkAttachmentRevision(t, s)
	})
}

func testDockerNetworkAttachmentRevision(t *testing.T, s *storageFixture) {
	t.Helper()
	ed := runImage("busybox:1.37")
	app := ed.GetSpec().GetService().GetApp()
	app.Command = []string{"sleep", "600"}
	app.DesiredRunState = spec.DesiredRunState_DESIRED_RUN_STATE_STOPPED
	made := apply(t, s.changes, propose(t, s.changes, "Create stopped workload", ed))
	scope := storageScope(t, made, "service", "web").GetResource().GetScope()
	if s.engine == nil {
		removeStorageFixture(t, s.server.client, scope, []string{"back"}, nil)
	}
	net := s.guardedNetwork(t, scope)
	name := network.TargetOf(resource.Ref{Kind: network.Kind, Scope: scope, Name: "back"}).Name
	check := func(want bool) *docker.Container {
		t.Helper()
		c := s.workload(t, scope)
		if c.State.Running || c.NetworkEndpoint("bridge", "") == nil || (c.NetworkEndpoint(name, net.EngineId) != nil) != want {
			t.Fatalf("stopped workload, attachment %v: state %v, networks %v", want, c.State, c.NetworkSettings.Networks)
		}
		return c
	}
	before := check(false)
	add := s.planAttachment(t, s.attachment(t, net, before, connectStorage))
	applied := apply(t, s.changes, add)
	if applied.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatal(applied.GetError())
	}
	joined := check(true)
	if joined.ID == before.ID {
		t.Fatal("the change did not replace the workload")
	}
	detail, err := s.api.GetDockerObject(t.Context(), connect.NewRequest(&v1.GetDockerObjectRequest{Key: net}))
	if err != nil {
		t.Fatal(err)
	}
	listed := detail.Msg.GetObject().GetNetwork().GetAttachments()
	if len(listed) != 1 || listed[0].GetContainerId() != joined.ID {
		t.Fatalf("the network did not report its stopped member: %v", listed)
	}
	cut := s.planAttachment(t, s.attachment(t, net, joined, disconnectStorage))
	applied = apply(t, s.changes, cut)
	if applied.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatal(applied.GetError())
	}
	check(false)
	undo, err := s.changes.UndoChange(t.Context(), connect.NewRequest(&v1.UndoChangeRequest{Id: cut.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	applied = apply(t, s.changes, undo.Msg.GetChange())
	if applied.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatal(applied.GetError())
	}
	check(true)
}

func TestDockerVolumeInventoryMountsAndBackupOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	helper := filepath.Join(t.TempDir(), "dockwell")
	build := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-tags", "agent", "-o", helper, "./cmd/dockwell")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the backup helper: %v\n%s", err, out)
	}
	s := storageControl(t, store.SQLite, client, helper)
	ed := runImage("busybox:1.37")
	ed.GetSpec().GetService().GetApp().Command = []string{"sh", "-c", "trap 'exit 0' TERM; while :; do sleep 1; done"}
	ed.GetSpec().GetService().GetApp().Mounts = []*spec.Mount{{Source: &spec.Mount_Volume{Volume: "data"}, Target: "/data"}}
	created := apply(t, s.changes, propose(t, s.changes, "Run with a named volume", ed, &v1.Edit{
		Resource: &v1.ResourceRef{Kind: "volume", Name: "data"}, Spec: &spec.Spec{Kind: &spec.Spec_Volume{Volume: &spec.Volume{}}},
	}))
	v := storageScope(t, created, "volume", "data")
	scope := v.GetResource().GetScope()
	removeStorageFixture(t, client, scope, nil, []string{"data"})
	c := s.workload(t, scope)
	code, err := client.Exec(t.Context(), c.ID, []string{"sh", "-c", "printf '%s' 'fixture volume data' >/data/fixture.txt"}, docker.Streams{})
	if err != nil || code != 0 {
		t.Fatalf("seed the volume: exit %d, %v", code, err)
	}
	name := volume.TargetOf(resource.Ref{Kind: volume.Kind, Scope: scope, Name: "data"}).Name
	listed, err := s.api.ListDockerObjects(t.Context(), connect.NewRequest(&v1.ListDockerObjectsRequest{
		ServerIds: []string{s.server.id.ServerID}, Kind: v1.DockerObjectKind_DOCKER_OBJECT_KIND_VOLUME, Filter: &v1.DockerObjectFilter{Search: name},
	}))
	if err != nil || len(listed.Msg.GetObjects()) != 1 {
		t.Fatalf("list volume: %v, %v", listed, err)
	}
	key := listed.Msg.GetObjects()[0].GetKey()
	detail, err := s.api.GetDockerObject(t.Context(), connect.NewRequest(&v1.GetDockerObjectRequest{Key: key}))
	if err != nil {
		t.Fatal(err)
	}
	row := detail.Msg.GetObject()
	if row.GetManagedResourceId() != v.GetId() || row.GetVolume().GetMountpoint() == "" ||
		len(row.GetVolume().GetAttachments()) != 1 || row.GetVolume().GetAttachments()[0].GetContainerId() != c.ID ||
		row.GetVolume().GetAttachments()[0].GetMountTarget() != "/data" {
		t.Fatalf("volume detail = %v", row)
	}
	bc := backupClient(s.person, s.person.origin)
	preview, err := bc.PreviewBackup(t.Context(), connect.NewRequest(&v1.PreviewBackupRequest{VolumeId: v.GetId(), Server: s.server.id.ServerID}))
	if err != nil || preview.Msg.GetMethod() != v1.BackupMethod_BACKUP_METHOD_COPY_RUNNING || !slices.Equal(preview.Msg.GetRunning(), []string{"web"}) {
		t.Fatalf("backup preview = %v, %v", preview, err)
	}
	started, err := bc.CreateBackup(t.Context(), connect.NewRequest(&v1.CreateBackupRequest{VolumeId: v.GetId(), Server: s.server.id.ServerID, Stop: true}))
	if err != nil {
		t.Fatal(err)
	}
	got := waitBackup(t, bc, started.Msg.GetBackup().GetId())
	if got.GetState() != v1.BackupState_BACKUP_STATE_DONE || got.GetSha256() == "" || got.GetSizeBytes() == 0 {
		t.Fatalf("backup = %v", got)
	}
	path := filepath.Join(s.server.backups.Root, scope, "data", got.GetId()+".tar.gz")
	archive, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	z, err := gzip.NewReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	tarred := tar.NewReader(z)
	for {
		h, err := tarred.Next()
		if errors.Is(err, io.EOF) {
			t.Fatal("the archive has no fixture.txt")
		}
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Base(h.Name) == "fixture.txt" {
			data, err := io.ReadAll(io.LimitReader(tarred, 256))
			if err != nil || string(data) != "fixture volume data" {
				t.Fatalf("archived fixture = %q, %v", data, err)
			}
			break
		}
	}
}

func TestDockerDisconnectReviewSurvivesRestart(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		s := storageControl(t, b, nil, "")
		made := apply(t, s.changes, propose(t, s.changes, "Run workload", runImage("ghcr.io/acme/shop:1.4")))
		scope := storageScope(t, made, "service", "web").GetResource().GetScope()
		n := s.guardedNetwork(t, scope)
		held := s.workload(t, scope)
		joined := apply(t, s.changes, s.planAttachment(t, s.attachment(t, n, held, connectStorage)))
		if joined.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatal(joined.GetError())
		}
		held = s.workload(t, scope)
		name := network.TargetOf(resource.Ref{Kind: network.Kind, Scope: scope, Name: "back"}).Name
		address := held.NetworkEndpoint(name, n.EngineId).IPAddress
		plan := s.planAttachment(t, s.attachment(t, n, held, disconnectStorage))
		if err := s.server.client.StopContainer(t.Context(), held.ID, 0); err != nil {
			t.Fatal(err)
		}
		if err := s.server.client.StartContainer(t.Context(), held.ID); err != nil {
			t.Fatal(err)
		}
		after := s.workload(t, scope)
		if after.ID != held.ID || after.NetworkEndpoint(name, n.EngineId).IPAddress == address {
			t.Fatal("the fake Engine did not change the address across restart")
		}
		cut := apply(t, s.changes, plan)
		if cut.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED || s.workload(t, scope).NetworkEndpoint(name, n.EngineId) != nil {
			t.Fatalf("disconnect after restart: %v", cut.GetError())
		}
	})
}

func TestDockerMaintenanceRejectsUnreviewedVolumeSelections(t *testing.T) {
	s := storageControl(t, store.SQLite, nil, "")
	id := s.server.id.ServerID
	_, err := s.api.PlanDockerMaintenance(t.Context(), connect.NewRequest(&v1.PlanDockerMaintenanceRequest{
		ServerIds: []string{id}, ExpectedSnapshot: strings.Repeat("1", 64), Operation: &v1.PlanDockerMaintenanceRequest_Cleanup{Cleanup: &v1.DockerCleanupSelection{
			Targets: []*v1.DockerCleanupTarget{{ServerId: id, Kind: v1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME, EngineId: "fixture-data"}},
		}},
	}))
	if connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("unreviewed volume submitted to Docker cleanup: %v", err)
	}
}
