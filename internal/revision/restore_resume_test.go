// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/backup"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// seeds stands in for what restores a backup into a new database on the server: it
// loads nothing, and its note says how far it got, as a test sets it.
type seeds struct {
	service.Movers
	mu     sync.Mutex
	loaded int
	stage  string
}

func (s *seeds) JudgeSeed(_ context.Context, sd backup.Seed) (resource.RestorePlan, error) {
	return resource.RestorePlan{Backup: sd.Backup, Method: resource.BackupDump, Database: "PostgreSQL", IntoNew: true, From: sd.From}, nil
}

func (s *seeds) SeedNew(context.Context, backup.Seed) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loaded++
	s.stage = backup.SeedDone
	return nil
}

func (s *seeds) SeedStage(context.Context, backup.Seed) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stage, nil
}

func (s *seeds) ForgetSeed(backup.Seed) error { return nil }

// withSeeds gives setupDocker's srv_A a service driver that restores into a new
// database through seeds, and runs PostgreSQL's image, and says the agent it made.
func withSeeds(e *Engine, engines map[string]*dockertest.Engine) (*seeds, resource.Local) {
	engines["srv_A"].Registry["postgres:17"] = dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}}
	s := &seeds{}
	client, kept := engines["srv_A"].Client(), &volume.Kept{}
	containers := container.Driver{Runtime: &container.Docker{Client: client, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, Deleted: kept}}
	local := resource.Local{Drivers: resource.NewRegistry(&containers, &service.Driver{Containers: containers, Movers: s},
		&volume.Driver{Runtime: &volume.Docker{Client: client, Kept: kept}}, &network.Driver{Client: client})}
	e.agents.(onDocker)["srv_A"] = local //nolint:forcetypeassert // setupDocker's
	return s, local
}

// A change that restores into a new database, stopped once its step made the
// database, as a restart of the control plane leaves it, resumes: where the restore
// finished, the change goes on to the end, loading nothing again; where it hadn't,
// the change rolls back, rather than stay applying (the review's M1).
func TestARestoreIntoANewDatabaseResumes(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		applied     bool
	}{
		{"finished", backup.SeedDone, true},
		{"cut short", backup.SeedFilling, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Each(t, func(t *testing.T, b store.Backend) {
				e, engines, db := setupDocker(t, b, "srv_A")
				s, plain := withSeeds(e, engines)
				agents := e.agents.(onDocker) //nolint:forcetypeassert // setupDocker's
				prod := production(t, db)
				project := tree(t, db).Projects[0].ID
				apply(t, e, propose(t, e, "Add staging", environment(project, "staging")))
				_, staging, err := tree(t, db).Resolve("", "staging")
				if err != nil {
					t.Fatal(err)
				}

				// The engine stops while the step after the new database's runs.
				started := make(chan struct{}, 1)
				agents["srv_A"] = hooked{Agent: plain, apply: func(ctx context.Context, step resource.Step, _ proto.Message) error {
					if step.Ref.Name != "adminer" {
						return nil
					}
					started <- struct{}{}
					<-ctx.Done()
					return ctx.Err()
				}}
				dbRef := resource.Ref{Kind: service.Kind, Scope: staging.ID, Name: "db"}
				p, err := e.Propose(t.Context(), Proposal{Summary: "Restore warehouse as a new database", Actor: "usr_test", Edits: []Edit{
					data(staging.ID, "srv_A"),
					{
						Ref: dbRef, Servers: []string{"srv_A"},
						Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
							Image: "postgres:17", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/var/lib/postgresql/data"}},
						}}},
						Restore: &resource.RestoreSource{Backup: "bkp_1", Server: "srv_A", From: resource.Ref{Kind: volume.Kind, Scope: prod, Name: "data"}},
					},
					{
						Ref: resource.Ref{Kind: service.Kind, Scope: staging.ID, Name: "adminer"}, Servers: []string{"srv_A"},
						Spec: &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "nginx:1.27"}}},
					},
				}})
				if err != nil {
					t.Fatal(err)
				}
				if err := e.Start(t.Context(), p.Change, By{Actor: "user:usr_mira", Via: ViaPanel}); err != nil {
					t.Fatal(err)
				}
				<-started
				grace, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()
				if err := e.Stop(grace); err != nil {
					t.Fatalf("Stop = %v", err)
				}
				if c := stored(t, db, p.Change); c.State != store.ChangeApplying {
					t.Fatalf("stopped partway, the change is %s", c.State)
				}
				if s.loaded != 1 {
					t.Fatalf("the restore ran %d times before the stop", s.loaded)
				}

				// The next start resumes it, with the agent as it is.
				s.mu.Lock()
				s.stage = tc.stage
				s.mu.Unlock()
				agents["srv_A"] = plain
				kinds := resource.NewRegistry(&container.Driver{}, &service.Driver{}, &volume.Driver{}, &network.Driver{})
				next := New(db, kinds, agents, secretstest.Keeper(db, kinds), slog.New(slog.DiscardHandler))
				// One that rolls back says why.
				if err := next.Resume(t.Context()); tc.applied && err != nil {
					t.Fatalf("Resume = %v", err)
				}
				next.Wait()
				c := stored(t, db, p.Change)
				if tc.applied && c.State != store.ChangeApplied {
					t.Errorf("resumed after the restore finished, the change is %s: %s", c.State, c.Error)
				}
				if !tc.applied && (c.State == store.ChangeApplied || c.State == store.ChangeApplying) {
					t.Errorf("resumed after the restore was cut short, the change is %s, want it rolled back", c.State)
				}
				if s.loaded != 1 {
					t.Errorf("the resume loaded the backup again: %d loads", s.loaded)
				}
			})
		})
	}
}
