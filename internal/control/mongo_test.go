// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

// A MongoDB moves one major release at a time: a plan that skips one is refused and
// names the release to go through, and one that moves on says its features go up
// first (ADR 0038).
func TestAMongoDBMovesOneReleaseAtATime(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	for _, image := range []string{"mongo:6.0", "mongo:7.0", "mongo:8.0"} {
		e.AddLocalImage(image, dockertest.Image{Config: docker.Config{Cmd: []string{"mongod"}}, Volumes: []string{"/data/db", "/data/configdb"}})
	}
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	compose := func(image string) string {
		return "services:\n  docs:\n    image: " + image + "\n    volumes:\n      - data:/data/db\nvolumes:\n  data: {}\n"
	}
	if got := apply(t, c, importCompose(t, cc, compose("mongo:6.0"), nil, false)); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("MongoDB 6.0: %v", got.GetError())
	}
	_, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: scope.DefaultProject, Compose: compose("mongo:8.0")}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "Change its image to mongo:7.0 first, then to mongo:8.0 once that's healthy") {
		t.Errorf("skipping 7.0: %v", err)
	}
	next := importCompose(t, cc, compose("mongo:7.0"), nil, false)
	i := slices.IndexFunc(next.GetPlan().GetNotes(), func(n *dockwellv1.PlanNote) bool { return n.GetReason() == "raises_features" })
	if i < 0 {
		t.Fatalf("moving to 7.0 says nothing of its features: %v", next.GetPlan().GetNotes())
	}
	if n := next.GetPlan().GetNotes()[i]; !slices.Equal(n.GetNames(), []string{"6.0", "7.0"}) || n.GetResource().GetName() != "docs" ||
		!strings.Contains(n.GetMessage(), "its feature compatibility version goes up to the release it runs, 6.0, where") {
		t.Errorf("the note is %v", n)
	}

	// It backs up the database's volume before it moves, as a step of the plan,
	// unless it's asked not to.
	steps := next.GetPlan().GetServers()[0].GetSteps()
	first := slices.IndexFunc(steps, func(s *dockwellv1.PlanStep) bool {
		return s.GetBackupFirst() != nil && s.GetResource().GetName() == "data"
	})
	replace := slices.IndexFunc(steps, func(s *dockwellv1.PlanStep) bool {
		return s.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_REPLACE && s.GetResource().GetName() == "docs"
	})
	if first < 0 || replace < 0 || first > replace {
		t.Errorf("the backup isn't before the move: %v", steps)
	}
	if !slices.ContainsFunc(next.GetPlan().GetNotes(), func(n *dockwellv1.PlanNote) bool { return n.GetReason() == "backup_first" }) {
		t.Errorf("the plan doesn't say it backs up first: %v", next.GetPlan().GetNotes())
	}
	res, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: scope.DefaultProject, Compose: compose("mongo:7.0"), SkipBackup: true}))
	if err != nil {
		t.Fatal(err)
	}
	skipped := res.Msg.GetChange().GetPlan()
	if slices.ContainsFunc(skipped.GetServers()[0].GetSteps(), func(s *dockwellv1.PlanStep) bool { return s.GetBackupFirst() != nil }) {
		t.Error("it backs up what it was told not to")
	}
	if !slices.ContainsFunc(skipped.GetNotes(), func(n *dockwellv1.PlanNote) bool { return n.GetReason() == "no_backup" }) {
		t.Errorf("skipping says nothing: %v", skipped.GetNotes())
	}
}

// MongoDB's users are read, and Dockwell makes none yet: asking for one is refused
// in words that say what to do instead, before anything is planned (ADR 0038).
func TestAMongoDBUserIsRefusedInWords(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.AddLocalImage("mongo:8.0", dockertest.Image{Config: docker.Config{Cmd: []string{"mongod"}}, Volumes: []string{"/data/db", "/data/configdb"}})
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	file := "services:\n  docs:\n    image: mongo:8.0\n    volumes:\n      - data:/data/db\nvolumes:\n  data: {}\n"
	if got := apply(t, c, importCompose(t, cc, file, nil, false)); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("MongoDB 8.0: %v", got.GetError())
	}
	dc := dockwellv1connect.NewDatabaseServiceClient(b, b.origin, connect.WithProtoJSON())
	_, err := dc.CreateDatabaseUser(t.Context(), connect.NewRequest(&dockwellv1.CreateDatabaseUserRequest{Id: "docs", User: "shop"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument ||
		!strings.Contains(err.Error(), "docs runs MongoDB, and Dockwell can't make users in MongoDB yet. Make one with db.createUser in mongosh") {
		t.Errorf("a user in MongoDB: %v", err)
	}
}
