// SPDX-License-Identifier: AGPL-3.0-only

package binding

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// A service's container gets its app's environment and the values of
// what it uses; a label says what it used, never a value, and reading
// the container back gives the service's spec.
func TestEnvironmentAndLabelRoundTrip(t *testing.T) {
	svc := &resourcev1.Service{
		Runs:      &resourcev1.Service_App{App: &resourcev1.Container{Image: "acme/api:1", Env: map[string]string{"PORT": "8080"}}},
		Secrets:   map[string]string{"DATABASE_URL": "DB_URL"},
		Variables: map[string]string{"LOG_LEVEL": "LOG_LEVEL"},
		Revisions: map[string]int64{"DATABASE_URL": 4, "LOG_LEVEL": 2},
	}
	if _, err := Environment(context.Background(), svc); err == nil || strings.Contains(err.Error(), "hunter2") ||
		!strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("an environment without the values = %v, want an error that names what is missing", err)
	}
	ctx := resource.WithValues(context.Background(), map[string]string{"DATABASE_URL": "postgres://hunter2@db", "LOG_LEVEL": "info"})
	env, err := Environment(ctx, svc)
	if err != nil || env["PORT"] != "8080" || env["DATABASE_URL"] != "postgres://hunter2@db" || env["LOG_LEVEL"] != "info" {
		t.Fatalf("Environment = %v, %v", env, err)
	}
	if svc.GetApp().GetEnv()["DATABASE_URL"] != "" {
		t.Error("Environment wrote the value into the spec")
	}
	label := Label(svc)
	if label != "DATABASE_URL=secret:DB_URL@4,LOG_LEVEL=variable:LOG_LEVEL@2" {
		t.Errorf("Label = %q", label)
	}
	c := proto.Clone(svc.GetApp()).(*resourcev1.Container)
	c.Env, c.Labels = env, map[string]string{LabelKey: label}
	back, err := Restore(c)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(back, svc) {
		t.Errorf("Restore = %v, want %v", back, svc)
	}
	if _, err := Restore(&resourcev1.Container{Labels: map[string]string{LabelKey: "nonsense"}}); err == nil {
		t.Error("Restore read a label Dockwell didn't write")
	}
}

// A container reads back as using only what its environment has. One made
// by docker compose up from an export, with no value in its shell, carries
// the label without the variable, so it reads back as not using it, and
// shows as drift rather than as if it ran with the value.
func TestRestoreUsesOnlyWhatTheEnvironmentHas(t *testing.T) {
	c := &resourcev1.Container{
		Image:  "acme/api:1",
		Env:    map[string]string{"LOG_LEVEL": "info"},
		Labels: map[string]string{LabelKey: "DATABASE_URL=secret:DB_URL@4,LOG_LEVEL=variable:LOG_LEVEL@2"},
	}
	back, err := Restore(c)
	if err != nil {
		t.Fatal(err)
	}
	if back.GetSecrets() != nil || back.GetVariables()["LOG_LEVEL"] != "LOG_LEVEL" || back.GetRevisions()["LOG_LEVEL"] != 2 || len(back.GetRevisions()) != 1 {
		t.Errorf("Restore = %v, want the variable alone", back)
	}
	if c.GetEnv() != nil || c.GetLabels() != nil {
		t.Errorf("Restore left %v and %v on the app", c.GetEnv(), c.GetLabels())
	}
}

// App gives the container what the service runs with, and leaves the
// service's own spec without it, so a value goes no further than the call.
func TestAppLeavesTheSpecAsItIs(t *testing.T) {
	svc := &resourcev1.Service{
		Runs:      &resourcev1.Service_App{App: &resourcev1.Container{Image: "acme/api:1", Labels: map[string]string{"team": "shop"}}},
		Secrets:   map[string]string{"TOKEN": "API_TOKEN"},
		Revisions: map[string]int64{"TOKEN": 3},
	}
	before := proto.Clone(svc)
	app := App(svc, map[string]string{"TOKEN": "hunter2"})
	if app.GetEnv()["TOKEN"] != "hunter2" || app.GetLabels()[LabelKey] != "TOKEN=secret:API_TOKEN@3" || app.GetLabels()["team"] != "shop" {
		t.Errorf("App = %v", app)
	}
	if !proto.Equal(svc, before) {
		t.Errorf("App changed the service: %v", svc)
	}
	if plain := App(&resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "a"}}}, nil); plain.GetLabels() != nil {
		t.Errorf("a service that uses nothing has the labels %v", plain.GetLabels())
	}
}

// A label is what Dockwell wrote, or it is refused: another kind, or a
// part that doesn't read, isn't a use.
func TestParseLabelRefusesWhatDockwellDidntWrite(t *testing.T) {
	if uses, err := ParseLabel(""); err != nil || uses != nil {
		t.Errorf("an empty label = %v, %v", uses, err)
	}
	uses, err := ParseLabel("A=secret:B@4,C=variable:D@0")
	if err != nil || len(uses) != 2 || uses[0] != (resource.Use{As: "A", Kind: "secret", Name: "B", Revision: 4}) {
		t.Errorf("ParseLabel = %+v, %v", uses, err)
	}
	for _, label := range []string{"nonsense", "A=secret", "A=secret:B", "A=volume:B@1", "=secret:B@1", "A=secret:@1", "A=secret:B@x"} {
		if _, err := ParseLabel(label); err == nil {
			t.Errorf("ParseLabel(%q) took it", label)
		}
	}
}
