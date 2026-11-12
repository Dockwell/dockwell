// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// A tag says a release series where it names one: PostgreSQL's major, and MySQL's
// and MariaDB's two numbers. One that names less says nothing.
func TestASeriesIsWhatATagNames(t *testing.T) {
	pg, my, maria := Recognise("postgres"), Recognise("mysql"), Recognise("mariadb")
	for _, c := range []struct {
		e     *Engine
		image string
		want  string
	}{
		{pg, "postgres:16", "16"},
		{pg, "postgres:16.4", "16"},
		{pg, "postgres:17.2-alpine", "17"},
		{pg, "postgres:9.6.24", "9.6"},
		{pg, "postgres:9", ""},
		{pg, "postgres:latest", ""},
		{pg, "postgres:alpine", ""},
		{pg, "docker.io/library/postgres:16.4@sha256:abc", "16"},
		{my, "mysql:8.0.36", "8.0"},
		{my, "mysql:8.4", "8.4"},
		{my, "mysql:8", ""},
		{my, "mysql:lts", ""},
		{maria, "mariadb:11.4.4-noble", "11.4"},
		{maria, "mariadb:10.11", "10.11"},
		{maria, "mariadb:11", ""},
	} {
		if got := Series(c.e, c.image); got != c.want {
			t.Errorf("Series(%s) = %q, want %q", c.image, got, c.want)
		}
	}
}

// onVolume is a database's service with its data in a volume of its environment, where
// the image's release keeps it.
func onVolume(image, volume string) *resourcev1.Service {
	svc := service(image)
	e := Recognise(image)
	svc.GetApp().Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: volume}, Target: e.Volume(Major(image))}}
	return svc
}

// Moving PostgreSQL, MySQL or MariaDB up a release series by its image moves its
// data by a dump into a new volume, where the new release keeps it; down one, or
// with its data where Dockwell can't back it up, it's refused, in a line that says
// why and what to pick. Within a series, or where a tag can't say, it's no move
// (ADR 0080).
func TestADatabasesSeriesIsNoPlainSwap(t *testing.T) {
	for _, c := range []struct {
		from, to string
		target   string // where the new release has the new volume, for a move
		refused  string
	}{
		{"postgres:16.4", "postgres:17.2", "/var/lib/postgresql/data", ""},
		{"postgres:9.6", "postgres:10", "/var/lib/postgresql/data", ""},
		// PostgreSQL 18 keeps its data a level down, in the volume it declares one up.
		{"postgres:17", "postgres:18.1", "/var/lib/postgresql", ""},
		{"mysql:8.0.36", "mysql:8.4.3", "/var/lib/mysql", ""},
		{"mariadb:10.11", "mariadb:11.4", "/var/lib/mysql", ""},
		{"postgres:17", "postgres:16", "", "PostgreSQL 16 can't read the data of db, which runs PostgreSQL 17, so its image stays as it is. Keep it on 17"},
		{"mysql:8.4", "mysql:8.0", "", "MySQL 8.0 can't read the data of db, which runs MySQL 8.4, so its image stays as it is. Keep it on 8.4"},
		{"postgres:16.4", "postgres:16.5", "", ""},
		{"postgres:16", "postgres:16.5-alpine", "", ""},
		{"postgres:16.4", "postgres:latest", "", ""},
		{"mysql:8.0.35", "mysql:8.0.36", "", ""},
		{"mysql:8", "mysql:8.4", "", ""},
		{"redis:7.2", "redis:7.4", "", ""},
		{"postgres:16", "mysql:8.4", "", ""},
	} {
		move, err := MoveOf("db", onVolume(c.from, "data"), onVolume(c.to, "data"))
		switch {
		case c.refused == "" && err != nil:
			t.Errorf("%s to %s is refused: %v", c.from, c.to, err)
		case c.refused != "" && (err == nil || !strings.Contains(err.Error(), c.refused)):
			t.Errorf("%s to %s: %v, want it refused with %q", c.from, c.to, err, c.refused)
		case c.target == "" && move != nil && move.Dumps:
			t.Errorf("%s to %s is a move by a dump", c.from, c.to)
		case c.target != "" && (move == nil || !move.Dumps || move.Volume != "data" || move.Target != c.target):
			t.Errorf("%s to %s = %+v, want a dump of the volume data into one at %s", c.from, c.to, move, c.target)
		}
	}
}

// A database whose data isn't in a volume of its environment can't move by a dump
// into a new one. A change that mounts another volume in place of the one its data
// is on starts the new release on that, as undoing a move does: that's no move, and
// nothing refuses it.
func TestADatabaseMovesOnlyOutOfAVolumeOfItsOwn(t *testing.T) {
	inPath := service("postgres:16")
	inPath.GetApp().Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/pg"}, Target: "/var/lib/postgresql/data"}}
	if _, err := MoveOf("db", inPath, onVolume("postgres:17", "data")); err == nil || !strings.Contains(err.Error(), "its data isn't in a volume of its environment") {
		t.Errorf("data on a server path = %v", err)
	}
	if _, err := MoveOf("db", service("postgres:16"), service("postgres:17")); err == nil || !strings.Contains(err.Error(), "its data isn't in a volume of its environment") {
		t.Errorf("data in the image's own volume = %v", err)
	}
	external := onVolume("postgres:16", "pgdata")
	external.GetApp().GetMounts()[0].External = true
	ext17 := onVolume("postgres:17", "pgdata")
	ext17.GetApp().GetMounts()[0].External = true
	if _, err := MoveOf("db", external, ext17); err == nil || !strings.Contains(err.Error(), "a volume Dockwell didn't make") {
		t.Errorf("data in a volume Dockwell didn't make = %v", err)
	}
	// Undoing a move: back to 16, on the volume it had.
	if move, err := MoveOf("db", onVolume("postgres:17", "data-17"), onVolume("postgres:16", "data")); err != nil || move != nil {
		t.Errorf("back to the volume it had = %+v, %v, want no move", move, err)
	}
	// A database's own data dir, by its variable, stays where it is.
	moved := onVolume("postgres:17", "data")
	moved.GetApp().Env = map[string]string{"PGDATA": "/srv/pg/data"}
	moved.GetApp().GetMounts()[0].Target = "/srv/pg"
	to := onVolume("postgres:18", "data")
	to.GetApp().Env = map[string]string{"PGDATA": "/srv/pg/data"}
	to.GetApp().GetMounts()[0].Target = "/srv/pg"
	if move, err := MoveOf("db", moved, to); err != nil || move == nil || move.Target != "/srv/pg" {
		t.Errorf("data where PGDATA says = %+v, %v, want a move that keeps /srv/pg", move, err)
	}
}

// What an official image says of its release, in its environment, is its series.
func TestAnImageSaysItsSeriesInItsEnvironment(t *testing.T) {
	pg, my, maria := Recognise("postgres"), Recognise("mysql"), Recognise("mariadb")
	for _, c := range []struct {
		e    *Engine
		env  []string
		want string
	}{
		{pg, []string{"PATH=/usr/bin", "PG_MAJOR=16", "PG_VERSION=16.4-1.pgdg120+2"}, "16"},
		{pg, []string{"PG_MAJOR=9.6"}, "9.6"},
		{my, []string{"MYSQL_MAJOR=innovation", "MYSQL_VERSION=9.1.0-1.el9"}, "9.1"},
		{my, []string{"MYSQL_MAJOR=8.4", "MYSQL_VERSION=8.4.3-1.el9"}, "8.4"},
		{maria, []string{"MARIADB_VERSION=1:11.4.4+maria~ubu2404"}, "11.4"},
		{maria, []string{"MARIADB_MAJOR=11.4"}, ""},
		{pg, nil, ""},
	} {
		if got := seriesFromEnv(c.e, c.env); got != c.want {
			t.Errorf("seriesFromEnv(%v) = %q, want %q", c.env, got, c.want)
		}
	}
}

// A moving tag that crossed a major release is refused after the pull and before
// anything stops, from what the images say; one that didn't, or images that say
// nothing, go ahead.
func TestAMovingTagThatCrossesAMajorIsRefusedBeforeAnythingStops(t *testing.T) {
	e := dockertest.New(t)
	e.AddLocalImage("postgres:latest", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}, Env: []string{"PG_MAJOR=16"}}})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), "env_1-db-1", &docker.CreateRequest{Config: docker.Config{
		Image: "postgres:latest", Cmd: []string{"postgres"}, Labels: map[string]string{docker.LabelService: "db"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	old, err := client.InspectContainer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	r := Reader{Client: client}
	e.AddLocalImage("postgres:16.5", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}, Env: []string{"PG_MAJOR=16"}}})
	e.AddLocalImage("postgres:custom", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}}})
	for _, image := range []string{"postgres:16.5", "postgres:custom"} {
		if err := r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: image}); err != nil {
			t.Errorf("to %s: %v", image, err)
		}
	}
	// The tag moves on to 17, as a pull of it would find.
	e.AddLocalImage("postgres:latest", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}, Env: []string{"PG_MAJOR=17"}}})
	err = r.BeforeReplace(t.Context(), old, &resourcev1.Container{Image: "postgres:latest"})
	if err == nil || resource.ReasonOf(err) != resource.ReasonDatabaseRelease {
		t.Fatalf("a tag that moved from 16 to 17 = %v", err)
	}
	plain, _ := resource.Explain(err)
	if want := "postgres:latest names PostgreSQL 17 now. PostgreSQL 17 can't start on the data of db, which runs PostgreSQL 16, so its image stays as it is. " +
		"Dockwell moves a database to a new release by a dump into a new volume where its tag names both: set its image to postgres:16, then to postgres:17. Nothing was stopped."; plain != want {
		t.Errorf("it says %q, want %q", plain, want)
	}
}

// A replace that mounts another volume where the data was starts on that volume's
// data, as the undo of a move and its rollback do, so what the images say of their
// series doesn't hold it up. One that keeps the volume, or a volume Docker made for
// the image, which a replace carries over, is held to its series.
func TestAReplaceOntoAnotherVolumeIsNoMove(t *testing.T) {
	e := dockertest.New(t)
	e.AddLocalImage("postgres:17", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}, Env: []string{"PG_MAJOR=17"}}})
	e.AddLocalImage("postgres:16", dockertest.Image{Config: docker.Config{Cmd: []string{"postgres"}, Env: []string{"PG_MAJOR=16"}}})
	client := e.Client()
	ctx := t.Context()
	if _, err := client.CreateVolume(ctx, &docker.VolumeCreate{Name: "env_1_data-17"}); err != nil {
		t.Fatal(err)
	}
	id, err := client.CreateContainer(ctx, "env_1-db-1", &docker.CreateRequest{
		Config:     docker.Config{Image: "postgres:17", Cmd: []string{"postgres"}, Labels: map[string]string{docker.LabelService: "db", docker.LabelProject: "env_1"}},
		HostConfig: docker.HostConfig{Mounts: []docker.Mount{{Type: "volume", Source: "env_1_data-17", Target: "/var/lib/postgresql/data"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	old, err := client.InspectContainer(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	r := Reader{Client: client}
	back := &resourcev1.Container{Image: "postgres:16", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/var/lib/postgresql/data"}}}
	if err := r.BeforeReplace(ctx, old, back); err != nil {
		t.Errorf("back to 16 on the volume it had: %v", err)
	}
	same := &resourcev1.Container{Image: "postgres:16", Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data-17"}, Target: "/var/lib/postgresql/data"}}}
	if err := r.BeforeReplace(ctx, old, same); resource.ReasonOf(err) != resource.ReasonDatabaseRelease {
		t.Errorf("16 on 17's volume = %v, want it refused", err)
	}
}
