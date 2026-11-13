// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"os"
	"path/filepath"
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
	if !strings.Contains(strings.Join(c.HostConfig.Binds, " "), conf+":/etc/nginx/conf.d/default.conf:ro") {
		t.Errorf("binds on Docker: %v", c.HostConfig.Binds)
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
		"a file made by hand": {[]*resourcev1.Mount{{Target: "/a", MakeOwner: "1:1", Source: &resourcev1.Mount_File{File: &resourcev1.MountFile{Content: "x"}}}}, "takes no external, make_owner or relabel"},
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
