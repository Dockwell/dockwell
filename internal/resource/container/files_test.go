// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
)

const nginxConf = "server { listen 8080; }\n"

func fileMount(target, content string, mode uint32) *resourcev1.Mount {
	return &resourcev1.Mount{Target: target, Source: &resourcev1.Mount_File{File: &resourcev1.MountFile{Content: content, Mode: mode}}}
}

// A file a container mounts is written once on its server, by its mode and content,
// bound read-only where the spec says, and reads back as the same file, so it's no
// drift; a new version is a new file, and the container moves to it (ADR 0085).
func TestAFileMountRunsAndReadsBackAsItsSpec(t *testing.T) {
	e, l, files := filesEngine(t)
	spec := svc(&resourcev1.Container{Mounts: []*resourcev1.Mount{
		fileMount("/etc/nginx/conf.d/default.conf", nginxConf, 0),
		fileMount("/run.sh", "#!/bin/sh\nexec nginx\n", 0o555),
	}})
	if m := spec.GetApp().GetMounts()[0]; !m.GetReadOnly() || m.GetFile().GetMode() != 0o444 {
		t.Errorf("Validate leaves %v, where a file is read-only and 0444 by default", m)
	}
	planAndApply(t, l, webRef(), spec)
	c := e.Container(service.ContainerName(webRef()))
	conf := files.Path(nginxConf, 0o444)
	// M2: bound as a mount of type bind, which fails where its source is missing,
	// never as a Binds entry, for which Docker would make an empty folder instead.
	if !slices.Contains(c.HostConfig.Mounts, docker.Mount{Type: "bind", Source: conf, Target: "/etc/nginx/conf.d/default.conf", ReadOnly: true}) ||
		strings.Contains(strings.Join(c.HostConfig.Binds, " "), conf) {
		t.Errorf("mounts on Docker: %v, binds %v", c.HostConfig.Mounts, c.HostConfig.Binds)
	}
	if b, err := os.ReadFile(conf); err != nil || string(b) != nginxConf {
		t.Errorf("the file kept is %q, %v", b, err)
	}
	if fi, err := os.Stat(files.Path("#!/bin/sh\nexec nginx\n", 0o555)); err != nil || fi.Mode().Perm() != 0o555 {
		t.Errorf("the script kept is %v, %v", fi, err)
	}
	got, err := l.Observe(t.Context(), []resource.Ref{webRef()})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got[0], spec) {
		t.Errorf("reads back as a different spec, which would show as drift: %v", resource.Diff(spec, got[0]))
	}

	changed := svc(&resourcev1.Container{Mounts: []*resourcev1.Mount{fileMount("/etc/nginx/conf.d/default.conf", "server { listen 9090; }\n", 0)}})
	planAndApply(t, l, webRef(), changed)
	got, err = l.Observe(t.Context(), []resource.Ref{webRef()})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got[0], changed) {
		t.Errorf("after a new version it reads back as %v", resource.Diff(changed, got[0]))
	}
}

// A volume's make_owner reads back as its spec says, so it's no drift (ADR 0085).
func TestAVolumesOwnerReadsBackAsItsSpec(t *testing.T) {
	e, l, _ := filesEngine(t)
	for _, v := range []string{"data", "cache"} {
		e.AddVolume(docker.Volume{Name: shapesEnv + "_" + v, Driver: "local"})
	}
	spec := svc(&resourcev1.Container{Mounts: []*resourcev1.Mount{
		{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data", MakeOwner: "1000:1000"},
		{Source: &resourcev1.Mount_Volume{Volume: "cache"}, Target: "/cache"},
	}})
	planAndApply(t, l, webRef(), spec)
	if got := e.Container(service.ContainerName(webRef())).Config.Labels[container.LabelMakeOwners]; got != "/data=1000:1000" {
		t.Errorf("the label says %q", got)
	}
	got, err := l.Observe(t.Context(), []resource.Ref{webRef()})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got[0], spec) {
		t.Errorf("reads back as a different spec, which would show as drift: %v", resource.Diff(spec, got[0]))
	}
}

// A volume's mode and the files it starts with read back as its spec says, from its
// label and the files this server keeps, so neither is drift (ADR 0085).
func TestAVolumesModeAndFilesReadBackAsItsSpec(t *testing.T) {
	e, l, files := filesEngine(t)
	for _, v := range []string{"conf", "logs"} {
		e.AddVolume(docker.Volume{Name: shapesEnv + "_" + v, Driver: "local"})
	}
	spec := svc(&resourcev1.Container{Mounts: []*resourcev1.Mount{
		{Source: &resourcev1.Mount_Volume{Volume: "conf"}, Target: "/etc/app", Seed: []*resourcev1.MountSeed{
			{Path: "conf/app.conf", Content: "listen 80\n"}, {Path: "run.sh", Content: "#!/bin/sh\n", Mode: 0o755},
		}},
		{Source: &resourcev1.Mount_Volume{Volume: "logs"}, Target: "/var/log/app", MakeMode: 0o777},
	}})
	planAndApply(t, l, webRef(), spec)
	if _, err := os.Stat(files.Path("listen 80\n", 0o644)); err != nil {
		t.Errorf("what the volume starts with isn't kept: %v", err)
	}
	got, err := l.Observe(t.Context(), []resource.Ref{webRef()})
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got[0], spec) {
		t.Errorf("reads back as a different spec, which would show as drift: %v", resource.Diff(spec, got[0]))
	}
}

// What a volume starts with is checked: a path under it of plain names, given once,
// with no file where another's folder is, text within a file's bounds, read, write and
// execute bits alone, and on a volume Dockwell makes (ADR 0085).
func TestWhatAVolumeStartsWithTakes(t *testing.T) {
	seeded := func(seeds ...*resourcev1.MountSeed) []*resourcev1.Mount {
		return []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data", Seed: seeds}}
	}
	for name, c := range map[string]struct {
		mounts []*resourcev1.Mount
		want   string
	}{
		"an absolute path": {seeded(&resourcev1.MountSeed{Path: "/etc/passwd", Content: "x"}), "isn't a path under it"},
		"a path above":     {seeded(&resourcev1.MountSeed{Path: "../x", Content: "x"}), "isn't a path under it"},
		"a path not clean": {seeded(&resourcev1.MountSeed{Path: "a//b", Content: "x"}), "isn't a path under it"},
		"too deep":         {seeded(&resourcev1.MountSeed{Path: "a/b/c/d/e/f/g/h/i", Content: "x"}), "folders deep"},
		"twice":            {seeded(&resourcev1.MountSeed{Path: "a", Content: "x"}, &resourcev1.MountSeed{Path: "a", Content: "y"}), "twice"},
		"inside a file":    {seeded(&resourcev1.MountSeed{Path: "a", Content: "x"}, &resourcev1.MountSeed{Path: "a/b", Content: "y"}), "which is a file"},
		"binary":           {seeded(&resourcev1.MountSeed{Path: "a", Content: "\x00\x01"}), "isn't text"},
		"setuid":           {seeded(&resourcev1.MountSeed{Path: "a", Content: "x", Mode: 0o4755}), "read, write and execute bits alone"},
		"on Docker's own": {
			[]*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "theirs"}, Target: "/data", External: true, Seed: []*resourcev1.MountSeed{{Path: "a", Content: "x"}}}},
			"starts with nothing of Dockwell's",
		},
		"on a server path": {
			[]*resourcev1.Mount{{Source: &resourcev1.Mount_HostPath{HostPath: "/srv/data"}, Target: "/data", MakeMode: 0o770}},
			"takes no make_mode or seed",
		},
		"a mode with sticky": {[]*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data", MakeMode: 0o1777}}, "read, write and execute bits alone"},
	} {
		spec := &resourcev1.Container{Image: "nginx:1.27", Mounts: c.mounts}
		if err := container.Validate(spec); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	many := make([]*resourcev1.MountSeed, 65)
	for i := range many {
		many[i] = &resourcev1.MountSeed{Path: fmt.Sprintf("f%d", i), Content: "x"}
	}
	if err := container.Validate(&resourcev1.Container{Image: "nginx:1.27", Mounts: seeded(many...)}); err == nil || !strings.Contains(err.Error(), "files Dockwell writes") {
		t.Errorf("65 files a volume starts with: %v", err)
	}
	spec := &resourcev1.Container{Image: "nginx:1.27", Mounts: seeded(&resourcev1.MountSeed{Path: "b", Content: "x"}, &resourcev1.MountSeed{Path: "a/c", Content: "y"})}
	if err := container.Validate(spec); err != nil || spec.GetMounts()[0].GetSeed()[0].GetPath() != "a/c" || spec.GetMounts()[0].GetSeed()[0].GetMode() != 0o644 {
		t.Errorf("a volume's files in order, 0644 by default: %v, %v", spec.GetMounts()[0].GetSeed(), err)
	}
}

// filesEngine is shapesEngine, with the folder its runtime keeps files in.
func filesEngine(t *testing.T) (*dockertest.Engine, resource.Local, *container.Files) {
	t.Helper()
	e := dockertest.New(t)
	e.Registry["nginx:1.27"] = dockertest.Image{Config: docker.Config{Cmd: []string{"nginx"}}}
	files := &container.Files{Dir: t.TempDir(), Client: e.Client()}
	rt := &container.Docker{Client: e.Client(), Settle: 1, Poll: 1, Soak: -1, Files: files}
	d := &service.Driver{Containers: container.Driver{Runtime: rt}}
	return e, resource.Local{Drivers: resource.NewRegistry(d)}, files
}

// What a file holds, how big it is and what it may do are bounded, and it's read-only.
func TestWhatAFileMountTakes(t *testing.T) {
	big := strings.Repeat("x", 256<<10+1)
	for name, c := range map[string]struct {
		mounts []*resourcev1.Mount
		want   string
	}{
		"a file too big":      {[]*resourcev1.Mount{fileMount("/a", big, 0)}, "writes up to 256 KiB"},
		"a writable file":     {[]*resourcev1.Mount{fileMount("/a", "x", 0o644)}, "is read-only"},
		"a file made by hand": {[]*resourcev1.Mount{{Target: "/a", MakeOwner: "1:1", Source: &resourcev1.Mount_File{File: &resourcev1.MountFile{Content: "x"}}}}, "takes no external, make_owner, make_mode, seed or relabel"},
		"a file in /proc":     {[]*resourcev1.Mount{fileMount("/proc/sys/kernel/x", "x", 0)}, "the kernel's own folders"},
		"a file in /sys":      {[]*resourcev1.Mount{fileMount("/sys/x", "x", 0)}, "the kernel's own folders"},
		"a file in /dev":      {[]*resourcev1.Mount{fileMount("//dev/../dev/x", "x", 0)}, "the kernel's own folders"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := container.Validate(&resourcev1.Container{Image: "nginx:1.27", Mounts: c.mounts}); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Validate = %v, want %q", err, c.want)
			}
		})
	}
	var many []*resourcev1.Mount
	for i := range 65 {
		many = append(many, fileMount("/f/"+strings.Repeat("a", i+1), "x", 0))
	}
	if err := container.Validate(&resourcev1.Container{Image: "nginx:1.27", Mounts: many}); err == nil || !strings.Contains(err.Error(), "takes up to 64") {
		t.Errorf("Validate of 65 files = %v", err)
	}
}

// The store keeps one copy of a file, reads back only what it wrote under the name
// its mode and content give, and prunes what nothing mounts once it's an hour old.
func TestTheFileStore(t *testing.T) {
	e := dockertest.New(t)
	f := &container.Files{Dir: t.TempDir(), Client: e.Client()}
	p, err := f.Write("a", 0o444)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.Write("a", 0o444)
	if err != nil || again != p {
		t.Errorf("writing it again = %s, %v", again, err)
	}
	if got, ok := f.Read(p); !ok || got.GetContent() != "a" || got.GetMode() != 0o444 {
		t.Errorf("Read = %v, %v", got, ok)
	}
	if err := os.WriteFile(p, []byte("b"), 0o444); err == nil {
		if _, ok := f.Read(p); ok {
			t.Error("a file that isn't what its name says reads back")
		}
	}
	if _, ok := f.Read(filepath.Join(t.TempDir(), filepath.Base(p))); ok {
		t.Error("a file elsewhere reads back")
	}
	old, _ := f.Write("old", 0o444)
	young, _ := f.Write("young", 0o444)
	if err := os.Chtimes(old, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := f.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("a file nothing mounts, two hours old, is still there: %v", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Errorf("a file written a moment ago is gone: %v", err)
	}
}

// M2: a prune that can't tell what a container mounts removes nothing, as a file it
// took for unused would leave that container on an empty folder at its next start.
func TestAPruneRemovesNothingItCantTellIsUnused(t *testing.T) {
	for name, fail := range map[string]func(cancel func()) error{
		"an inspect that fails":           func(func()) error { return errors.New("the engine is busy") },
		"a context cancelled in the loop": func(cancel func()) error { cancel(); return nil },
	} {
		t.Run(name, func(t *testing.T) {
			e := dockertest.New(t)
			f := &container.Files{Dir: t.TempDir(), Client: e.Client()}
			e.AddContainer(docker.Container{Name: "/a"})
			e.AddContainer(docker.Container{Name: "/b"})
			old, err := f.Write("old", 0o444)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(old, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			e.Intercept(func(r *http.Request) error {
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json") && strings.Contains(r.URL.Path, "/containers/") && !strings.HasSuffix(r.URL.Path, "/containers/json") {
					return fail(cancel)
				}
				return nil
			})
			if err := f.Prune(ctx); err == nil {
				t.Error("the prune says it read every container")
			}
			if _, err := os.Stat(old); err != nil {
				t.Errorf("a file it couldn't tell was unused is gone: %v", err)
			}
		})
	}
}

// M2: a file written again, as a container is made anew with it, counts its hour
// from then, so a prune never takes it from under a container being made.
func TestAFileWrittenAgainIsKeptAnotherHour(t *testing.T) {
	e := dockertest.New(t)
	f := &container.Files{Dir: t.TempDir(), Client: e.Client()}
	p, err := f.Write("again", 0o444)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write("again", 0o444); err != nil {
		t.Fatal(err)
	}
	if err := f.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("a file written a moment ago, again, is gone: %v", err)
	}
}

// M2: an empty folder on a file's name, as Docker makes for a missing bind source,
// gives way to the file; one that holds something is left, saying so.
func TestAFolderOnAFilesNameGivesWayWhenEmpty(t *testing.T) {
	f := &container.Files{Dir: t.TempDir()}
	p := f.Path("conf", 0o444)
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := f.Write("conf", 0o444); err != nil || got != p {
		t.Fatalf("writing over an empty folder = %s, %v", got, err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "conf" {
		t.Errorf("the file reads %q, %v", b, err)
	}
	full := f.Path("other", 0o444)
	if err := os.MkdirAll(filepath.Join(full, "inside"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write("other", 0o444); err == nil || !strings.Contains(err.Error(), "isn't empty") {
		t.Errorf("writing over a folder that holds something = %v", err)
	}
}
