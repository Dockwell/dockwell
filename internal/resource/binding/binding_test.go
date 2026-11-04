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
