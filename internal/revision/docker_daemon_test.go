// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"google.golang.org/protobuf/proto"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/dockerdaemon"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

type restoredDaemon struct {
	dockerdaemon.Driver
	plans  int
	calls  int
	review bool
}

func (d *restoredDaemon) Judge(_ context.Context, _ resource.Ref, _, desired proto.Message, _ []resource.Change) (resource.Decision, error) {
	d.plans++
	p := &v1.DockerDaemonPlan{
		Evidence: "approved-daemon-settings", Impact: &v1.DockerDaemonImpact{Action: v1.DockerDaemonAction_DOCKER_DAEMON_ACTION_RESTART},
	}
	action := resource.ActionCreate
	if reviewsDaemonFile(desired) {
		action = resource.ActionUpdate
		p.FileChanges = []*v1.FieldChange{{Path: "registry-mirrors", To: `["https://mirror.example"]`}}
	}
	return resource.Decision{Action: action, DockerDaemon: p}, nil
}

func (*restoredDaemon) Observe(context.Context, resource.Ref) (proto.Message, error) {
	return nil, nil
}

func (d *restoredDaemon) Apply(ctx context.Context, _ resource.Step, desired proto.Message) error {
	d.calls++
	if d.review {
		if !reviewsDaemonFile(desired) {
			return errors.New("the reviewed file activation was lost")
		}
		if d.calls == 1 {
			return resource.ErrLost
		}
		return nil
	}
	resource.Report(ctx, resource.Outcome{Untouched: true})
	return errors.New("previous settings restored")
}

func TestDaemonFailureKeepsTheReviewedRevisionWithoutAnotherActivation(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		_, f, db := setup(t, b, "srv_A")
		d := &restoredDaemon{}
		kinds := resource.NewRegistry(&dockerdaemon.Driver{})
		keeper := secretstest.Keeper(db, kinds)
		f["srv_A"].agent = resource.Local{Drivers: resource.NewRegistry(d), Server: "srv_A", Values: keeper}
		e := New(db, kinds, f, keeper, slog.New(slog.DiscardHandler))
		change := propose(t, e, "Change Docker settings", Edit{
			Ref:  resource.Ref{Kind: dockerdaemon.Kind, Scope: "srv_A", Name: dockerdaemon.Name},
			Spec: &spec.DockerDaemon{Mirrors: &spec.DockerRegistryMirrors{Urls: []string{"https://mirror.example"}}},
		})
		err := e.Apply(t.Context(), change.Change)
		var failed *FailedError
		if !errors.As(err, &failed) || failed.Rollback != "" || failed.RollbackErr != nil || d.calls != 1 || d.plans != 2 {
			t.Fatalf("apply = %v, rollback = %+v, plans = %d, activations = %d", err, failed, d.plans, d.calls)
		}
		if got := stored(t, db, change.Change); got.State != store.ChangeFailed {
			t.Fatalf("change state = %s", got.State)
		}
	})
}

func TestCurrentDockerFileReviewResumesWithoutOwningTheFile(t *testing.T) {
	storetest.Each(t, func(t *testing.T, b store.Backend) {
		_, fleet, db := setup(t, b, "srv_A")
		d := &restoredDaemon{review: true}
		kinds := resource.NewRegistry(&dockerdaemon.Driver{})
		keeper := secretstest.Keeper(db, kinds)
		fleet["srv_A"].agent = resource.Local{Drivers: resource.NewRegistry(d), Server: "srv_A", Values: keeper}
		e := New(db, kinds, fleet, keeper, slog.New(slog.DiscardHandler))
		ref := resource.Ref{Kind: dockerdaemon.Kind, Scope: "srv_A", Name: dockerdaemon.Name}
		p := propose(t, e, "Activate the local Docker file", Edit{Ref: ref, Spec: &spec.DockerDaemon{ReviewCurrentFile: true}})
		if !p.AsGoodAsRoot() || p.Revisions != 0 || len(p.Resources[0].DockerDaemon.GetFileChanges()) != 1 {
			t.Fatalf("activation lost review details or took ownership: %+v", p)
		}
		if err := e.Apply(t.Context(), p.Change); !errors.Is(err, ErrPending) {
			t.Fatalf("first apply = %v, want pending", err)
		}
		if err := e.Resume(t.Context()); err != nil {
			t.Fatal(err)
		}
		// The first apply can already have queued a resume. Wait for that
		// run too, since Resume skips a change it finds running.
		e.Wait()
		if got := stored(t, db, p.Change); got.State != store.ChangeApplied || d.calls != 2 {
			t.Fatalf("resume = %s, calls = %d", got.State, d.calls)
		}
		if err := db.View(t.Context(), func(tx *store.Tx) error {
			_, err := tx.GetResource(t.Context(), string(ref.Kind), ref.Scope, ref.Name)
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("file activation must not create a desired resource: %v", err)
		}
	})
}
