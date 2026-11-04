// SPDX-License-Identifier: AGPL-3.0-only

package binding

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

var api = resource.Ref{Kind: "service", Scope: "env_1", Name: "api"}

// Values sealed for one stream, server and resource open only there.
func TestValuesOpenOnlyWhereTheyWereSealedFor(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"DATABASE_URL": "postgres://hunter2@db"}
	sealed, err := Seal(k.Public(), "srv_A", api, values)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("hunter2")) {
		t.Fatal("the sealed values hold the plaintext")
	}
	got, err := k.Open("srv_A", api, sealed)
	if err != nil || got["DATABASE_URL"] != values["DATABASE_URL"] {
		t.Fatalf("Open = %v, %v", got, err)
	}
	other, _ := NewKey()
	for what, try := range map[string]func() (map[string]string, error){
		"on another server": func() (map[string]string, error) { return k.Open("srv_B", api, sealed) },
		"for another service": func() (map[string]string, error) {
			return k.Open("srv_A", resource.Ref{Kind: "service", Scope: "env_1", Name: "web"}, sealed)
		},
		"on another stream": func() (map[string]string, error) { return other.Open("srv_A", api, sealed) },
	} {
		if v, err := try(); err == nil || v != nil {
			t.Errorf("opening values %s = %v, %v; want it refused", what, v, err)
		} else if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the error says the value: %v", err)
		}
	}
	if _, err := Seal(nil, "srv_A", api, values); err == nil {
		t.Error("sealed values for an agent with no delivery key")
	}
}

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
