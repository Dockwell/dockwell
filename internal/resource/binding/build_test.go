// SPDX-License-Identifier: AGPL-3.0-only

package binding

import (
	"context"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// A build's secret is used as any secret is, so its value reaches the server that
// builds, and never what runs; the container's labels say how it was built, so it
// reads back as its spec.
func TestABuildsSecretsAndSettingsRoundTrip(t *testing.T) {
	svc := &resourcev1.Service{
		Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "dockwell.invalid/svc_x:abc-def", Env: map[string]string{"PORT": "8080"}}},
		Build: &resourcev1.ImageBuild{
			Source:     &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: "https://github.com/acme/web", Ref: "main", Commit: strings.Repeat("a", 40)}},
			Dockerfile: "deploy/Dockerfile", Args: map[string]string{"NODE_ENV": "production"}, Secrets: map[string]string{"NPM_TOKEN": "NPM"},
		},
		Secrets:   map[string]string{"DATABASE_URL": "DATABASE_URL"},
		Revisions: map[string]int64{"DATABASE_URL": 2, "build:NPM_TOKEN": 3},
	}
	uses := Uses(svc)
	if len(uses) != 2 || uses[0].As != "DATABASE_URL" || uses[1].As != "build:NPM_TOKEN" || uses[1].Name != "NPM" || uses[1].Revision != 3 {
		t.Fatalf("Uses = %+v", uses)
	}
	ctx := resource.WithValues(context.Background(), map[string]string{"DATABASE_URL": "postgres://db", "build:NPM_TOKEN": "npm_s3cr3t"})
	env, err := Environment(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range env {
		if strings.Contains(k, "NPM") || v == "npm_s3cr3t" {
			t.Errorf("the build's secret reached what runs, as %s", k)
		}
	}
	secrets, err := BuildSecrets(ctx, svc)
	if err != nil || secrets["NPM_TOKEN"] != "npm_s3cr3t" || len(secrets) != 1 {
		t.Errorf("BuildSecrets = %v, %v", secrets, err)
	}
	if _, err := BuildSecrets(context.Background(), svc); err == nil || strings.Contains(err.Error(), "npm_s3cr3t") || !strings.Contains(err.Error(), "NPM_TOKEN") {
		t.Errorf("a build's secret that wasn't sent = %v, want an error that names it", err)
	}
	app := App(svc, env)
	if strings.Contains(app.GetLabels()[BuildLabelKey], "NPM") {
		t.Errorf("the build's label names its secrets, which the uses label does: %s", app.GetLabels()[BuildLabelKey])
	}
	if again := App(svc, env); again.GetLabels()[BuildLabelKey] != app.GetLabels()[BuildLabelKey] {
		t.Error("one build's label reads differently each time")
	}
	back, err := Restore(app)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(back, svc) {
		t.Errorf("Restore = %v, want %v", back, svc)
	}
	if _, err := Restore(&resourcev1.Container{Labels: map[string]string{BuildLabelKey: "{}"}}); err == nil {
		t.Error("Restore read a build label Dockwell didn't write")
	}
}

func TestASecretIsMaskedInEachFormALogPrintsItIn(t *testing.T) {
	forms := Lines([]string{"tok3n-value\n", "-----BEGIN KEY-----\nMIIEvQIBADAN\nab\n-----END KEY-----", ""})
	for _, want := range []string{"tok3n-value\n", "tok3n-value", "MIIEvQIBADAN", "-----BEGIN KEY-----"} {
		if !slices.Contains(forms, want) {
			t.Errorf("Lines doesn't have %q: %q", want, forms)
		}
	}
	if slices.Contains(forms, "ab") {
		t.Errorf("Lines masks a line too short to mask wherever it is: %q", forms)
	}
	if got := Hide("token tok3n-value, then MIIEvQIBADAN", forms); strings.Contains(got, "tok3n") || strings.Contains(got, "MIIEvQ") {
		t.Errorf("Hide with Lines left %q", got)
	}
}
