// SPDX-License-Identifier: AGPL-3.0-only

package compose_test

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// sameStacks fails unless two stacks hold the same specs, once each is
// checked as the engine checks it.
func sameStacks(t *testing.T, want, got *compose.Stack) {
	t.Helper()
	if len(want.Services) != len(got.Services) || len(want.Volumes) != len(got.Volumes) || len(want.Networks) != len(got.Networks) {
		t.Fatalf("the stacks differ in size: %v and %v", names(want.Services), names(got.Services))
	}
	for i := range want.Services {
		w, g := want.Services[i].Spec.GetApp(), got.Services[i].Spec.GetApp()
		if err := container.Validate(w); err != nil {
			t.Fatal(err)
		}
		if err := container.Validate(g); err != nil {
			t.Fatal(err)
		}
		if !proto.Equal(w, g) {
			t.Errorf("the service %s reads back as\n%v\nnot\n%v", want.Services[i].Name, g, w)
		}
	}
	for i := range want.Volumes {
		w, g := want.Volumes[i].Spec, got.Volumes[i].Spec
		_ = (&volume.Driver{}).Validate(w)
		_ = (&volume.Driver{}).Validate(g)
		if !proto.Equal(w, g) {
			t.Errorf("the volume %s reads back as %v, not %v", want.Volumes[i].Name, g, w)
		}
	}
	for i := range want.Networks {
		w, g := want.Networks[i].Spec, got.Networks[i].Spec
		_ = (&network.Driver{}).Validate(w)
		_ = (&network.Driver{}).Validate(g)
		if !proto.Equal(w, g) {
			t.Errorf("the network %s reads back as %v, not %v", want.Networks[i].Name, g, w)
		}
	}
}

// A $ of a file's own, written $$ as a shell command or a health check
// has it, stays a $ in every string an export writes, so the export
// imports again as the same specs, and docker compose reads no variable.
func TestAnExportImportsAgainAsTheSameSpecs(t *testing.T) {
	file := `
services:
  db:
    image: postgres:17
    command: ["sh", "-c", "exec postgres -p $$PORT"]
    entrypoint: ["/bin/sh", "-c", "echo $$HOME; exec \"$$@\"", "--"]
    user: "$$USER"
    working_dir: /srv/$$app
    labels: {cost: "$$5", "dollar$$key": plain}
    volumes: ["/srv/$$data:/data", "db-data:/var/lib/$$db"]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U $${POSTGRES_USER}"]
  web:
    image: nginx:1.27
    entrypoint: []
    command: ["sleep", "inf"]
volumes:
  db-data:
    labels: {note: "$$1 a month"}
networks:
  default:
    labels: {note: "$$2 a month"}
`
	first, err := compose.Parse(t.Context(), []byte(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	db := first.Services[0].Spec.GetApp()
	if got := db.GetCommand()[2]; got != "exec postgres -p $PORT" {
		t.Fatalf("the command reads as %q, want its $$ as a $", got)
	}
	out, err := compose.Export(project, first)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"exec postgres -p $$PORT", "pg_isready -U $${POSTGRES_USER}", "/srv/$$data:/data", "note: $$1 a month", "entrypoint: []"} {
		if !strings.Contains(string(out.File), want) {
			t.Errorf("the export doesn't have %s:\n%s", want, out.File)
		}
	}
	again, err := compose.Parse(t.Context(), out.File, nil)
	if err != nil {
		t.Fatalf("the export doesn't import again: %v\n%s", err, out.File)
	}
	sameStacks(t, first, again)
}

// entrypoint: [], entrypoint: "" and entrypoint: [""] each run no
// entrypoint, not even the image's, as docker compose runs them; an
// entrypoint left out takes the image's. The spec tells them apart.
func TestAnEntrypointClearedIsntLeftOut(t *testing.T) {
	for v, cleared := range map[string]bool{"[]": true, `""`: true, `[""]`: true, "": false} {
		file := "services:\n  web:\n    image: nginx:1.27\n    command: [\"sleep\", \"inf\"]\n"
		if v != "" {
			file += "    entrypoint: " + v + "\n"
		}
		stack, err := compose.Parse(t.Context(), []byte(file), nil)
		if err != nil {
			t.Fatalf("entrypoint %s: %v", v, err)
		}
		app := stack.Services[0].Spec.GetApp()
		if err := container.Validate(app); err != nil {
			t.Fatalf("entrypoint %s: %v", v, err)
		}
		if app.GetNoEntrypoint() != cleared || len(app.GetEntrypoint()) > 0 {
			t.Errorf("entrypoint %q reads as no_entrypoint %v, entrypoint %q; want no_entrypoint %v", v, app.GetNoEntrypoint(), app.GetEntrypoint(), cleared)
		}
	}
}

// A file larger than MaxFile is refused before anything reads it, as
// compose-go's canonical forms take far more time and memory than the
// file's size; aliases that expand a small file are refused too.
func TestCostlyFilesAreRefused(t *testing.T) {
	var b strings.Builder
	b.WriteString("services:\n  web:\n    image: nginx\n    labels:\n")
	for i := 0; b.Len() <= compose.MaxFile; i++ {
		fmt.Fprintf(&b, "      - k%d=v\n", i)
	}
	file := []byte(b.String())
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	_, err := compose.Parse(t.Context(), file, nil)
	took := time.Since(start)
	runtime.ReadMemStats(&after)
	if err == nil || !strings.Contains(err.Error(), "files of up to 1 MiB") {
		t.Errorf("a file of %d bytes = %v, want it refused for its size", len(file), err)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 1<<20 || took > time.Second {
		t.Errorf("refusing a large file took %s and allocated %d KB", took, alloc>>10)
	}

	var bomb strings.Builder
	bomb.WriteString(`x-a0: &a0 ["aaaaaaaaaaaaaaaa","aaaaaaaaaaaaaaaa","aaaaaaaaaaaaaaaa","aaaaaaaaaaaaaaaa","aaaaaaaaaaaaaaaa"]` + "\n")
	for i := 1; i <= 6; i++ {
		fmt.Fprintf(&bomb, "x-a%d: &a%d [*a%d,*a%d,*a%d,*a%d,*a%d,*a%d,*a%d,*a%d,*a%d,*a%d]\n", i, i, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1, i-1)
	}
	bomb.WriteString("services:\n  web:\n    image: nginx\n")
	if _, err := compose.Parse(t.Context(), []byte(bomb.String()), nil); err == nil || !strings.Contains(err.Error(), "aliasing") {
		t.Errorf("a file of %d bytes whose aliases expand = %v, want it refused", bomb.Len(), err)
	}
}
