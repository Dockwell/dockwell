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

// Moving PostgreSQL, MySQL or MariaDB across a release series by its image is
// refused, in a line that says why and what to pick; within one, or where a tag
// can't say, it isn't (ADR 0080).
func TestADatabasesSeriesIsNoPlainSwap(t *testing.T) {
	for _, c := range []struct {
		from, to string
		refused  string
	}{
		{"postgres:16.4", "postgres:17.2", "PostgreSQL 17 can't start on the data of db, which runs PostgreSQL 16, so its image stays as it is. Dockwell can't move a database across major releases yet: keep it on 16"},
		{"postgres:17", "postgres:16", "PostgreSQL 16 can't start on the data of db, which runs PostgreSQL 17"},
		{"postgres:9.6", "postgres:10", "PostgreSQL 10 can't start on the data of db, which runs PostgreSQL 9.6"},
		{"mysql:8.0.36", "mysql:8.4.3", "MySQL 8.4 would move the data of db, which runs 8.0, on for good, and 8.0 can't read it again, so neither undo nor a rollback could put it back. Dockwell can't move a database across release series yet: keep it on 8.0"},
		{"mysql:8.4", "mysql:8.0", "MySQL 8.0 can't read the data of db, which runs MySQL 8.4, so its image stays as it is. Keep it on 8.4"},
		{"mariadb:10.11", "mariadb:11.4", "MariaDB 11.4 would move the data of db, which runs 10.11, on for good"},
		{"postgres:16.4", "postgres:16.5", ""},
		{"postgres:16", "postgres:16.5-alpine", ""},
		{"postgres:16.4", "postgres:latest", ""},
		{"mysql:8.0.35", "mysql:8.0.36", ""},
		{"mysql:8", "mysql:8.4", ""},
		{"redis:7.2", "redis:7.4", ""},
		{"postgres:16", "mysql:8.4", ""},
	} {
		_, err := MoveOf("db", service(c.from), service(c.to))
		switch {
		case c.refused == "" && err != nil:
			t.Errorf("%s to %s is refused: %v", c.from, c.to, err)
		case c.refused != "" && (err == nil || !strings.Contains(err.Error(), c.refused)):
			t.Errorf("%s to %s: %v, want it refused with %q", c.from, c.to, err, c.refused)
		}
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
	if want := "postgres:latest names PostgreSQL 17 now. PostgreSQL 17 can't start on the data of db, which runs PostgreSQL 16, so its image stays as it is. Dockwell can't move a database across major releases yet: keep it on 16, such as with postgres:16. Nothing was stopped."; plain != want {
		t.Errorf("it says %q, want %q", plain, want)
	}
}
