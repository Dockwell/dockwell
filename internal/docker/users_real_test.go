// SPDX-License-Identifier: AGPL-3.0-only

package docker_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// What a plan reads as root is what Docker runs as root: each of these writes a
// file in a folder of the server's as root, and a number that isn't 0 doesn't (#247).
func TestWhoWritesAsRootOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	project := docker.Project(dockertest.Scope(t))
	dockertest.Cleanup(t, client, project)
	const image = "nginx:1.27-alpine"
	if err := client.PullImage(t.Context(), image, nil); err != nil {
		t.Fatal(err)
	}
	// Open to every user, so one that isn't root can write in it too.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	// owner says who owns what a container wrote as user, or that Docker wouldn't run
	// it, which is no way past a plan either.
	owner := func(i int, user string) (uint32, bool) {
		t.Helper()
		name := "writes-" + strconv.Itoa(i)
		req := &docker.CreateRequest{
			Config: docker.Config{
				Image: image, Entrypoint: []string{"sh", "-c"}, Cmd: []string{"touch /out/" + name}, User: user,
				Labels: map[string]string{docker.LabelProject: project},
			},
			HostConfig: docker.HostConfig{Binds: []string{dir + ":/out"}},
		}
		id, err := client.CreateContainer(t.Context(), project+"-"+name, req)
		if err == nil {
			err = client.StartContainer(t.Context(), id)
		}
		if err != nil {
			t.Logf("Docker won't run a container as %q: %v", user, err)
			return 0, false
		}
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		for ctx.Err() == nil {
			got, err := client.InspectContainer(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if !got.State.Running && !got.State.FinishedAt.IsZero() {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("as %q it wrote nothing: %v", user, err)
		}
		return info.Sys().(*syscall.Stat_t).Uid, true
	}
	for i, user := range []string{"0", "00", "000", "+0", "0:0", "00:1000", "root", "root:root", "", "1000", "1000:0"} {
		as, _ := docker.UserRunsAs(user)
		uid, ran := owner(i, user)
		switch {
		case !ran && (user == "0" || user == "root" || user == "" || user == "1000"):
			t.Errorf("Docker won't run a container as %q", user)
		case !ran:
		case as == docker.RunsAsRoot && uid != 0:
			t.Errorf("%q reads as root, and Docker wrote as %d", user, uid)
		case as == docker.RunsAsOther && uid == 0:
			t.Errorf("%q reads as a user other than root, and Docker wrote as root", user)
		case as == docker.RunsAsImage && uid != 0:
			// nginx's image runs as root: what a plan can't tell, it reads as may be.
			t.Errorf("nginx's own user wrote as %d", uid)
		}
	}
}
