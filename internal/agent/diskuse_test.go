// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"errors"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dockwell/dockwell/internal/agent/collect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// Logs are what json-file and local keep for each container, rotated files too,
// and nothing else in a container's directory.
func TestLogsSize(t *testing.T) {
	fsys := fstest.MapFS{
		"containers/a1/a1-json.log":                {Data: make([]byte, 300)},
		"containers/a1/a1-json.log.1":              {Data: make([]byte, 200)},
		"containers/a1/config.v2.json":             {Data: make([]byte, 5000)},
		"containers/a1/hostname":                   {Data: []byte("a1\n")},
		"containers/b2/local-logs/container.log":   {Data: make([]byte, 40)},
		"containers/b2/local-logs/container.log.1": {Data: make([]byte, 60)},
		"containers/b2/hosts":                      {Data: make([]byte, 999)},
		"overlay2/x/diff/var/log/a-json.log":       {Data: make([]byte, 7000)},
	}
	if got := logsSize(fsys); got != 600 {
		t.Errorf("logs = %d bytes, want 600", got)
	}
	if got := logsSize(fstest.MapFS{}); got != 0 {
		t.Errorf("with no containers directory, logs = %d", got)
	}
}

// Docker is asked once for what callers ask at once, and once in 30 s, and adds
// up what's in use apart from what isn't.
func TestDiskUseAsksDockerOnce(t *testing.T) {
	e := dockertest.New(t)
	e.Registry["web:1"] = dockertest.Image{Size: 100 << 20, Config: docker.Config{Cmd: []string{"web"}}}
	e.AddLocalImage("old:1", dockertest.Image{Size: 40 << 20})
	e.AddContainer(docker.Container{ID: "c1", Name: "/web", Config: docker.Config{Image: "old:1"}})
	e.ContainerSizes = map[string]int64{"web": 3 << 20}
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	d := &diskUse{
		docker: e.Client(),
		collector: &collect.Collector{
			Root: fstest.MapFS{"proc/self/mounts": {Data: []byte("/dev/sda1 / ext4 rw 0 0\n")}},
			Statfs: func(string) (uint64, uint64, error) {
				return 80 << 30, 30 << 30, nil
			},
		},
		logs: func(string) fs.FS { return fstest.MapFS{"containers/c1/c1-json.log": {Data: make([]byte, 1000)}} },
		now:  clock,
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := d.read(t.Context()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	use, err := d.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	asked := func() int {
		n := 0
		for _, r := range e.Seen() {
			if strings.HasSuffix(r, "/system/df") {
				n++
			}
		}
		return n
	}
	if n := asked(); n != 1 {
		t.Errorf("five asks within 30 s asked Docker %d times", n)
	}
	if use.GetMountPoint() != "/" || use.GetSizeBytes() != 80<<30 || use.GetUsedBytes() != 30<<30 {
		t.Errorf("disk = %q, %d of %d", use.GetMountPoint(), use.GetUsedBytes(), use.GetSizeBytes())
	}
	if use.GetImagesBytes() != 40<<20 || use.GetImagesUnusedBytes() != 0 || use.GetContainersBytes() != 3<<20 || use.GetLogsBytes() != 1000 {
		t.Errorf("images %d (%d unused), containers %d, logs %d", use.GetImagesBytes(), use.GetImagesUnusedBytes(), use.GetContainersBytes(), use.GetLogsBytes())
	}

	// Once it's old, the next ask counts again, and sees an image nothing uses.
	mu.Lock()
	now = now.Add(diskUseFor)
	mu.Unlock()
	e.Remove("web")
	use, err = d.read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n := asked(); n != 2 {
		t.Errorf("an ask after 30 s asked Docker %d times in all, want 2", n)
	}
	if use.GetImagesUnusedBytes() != 40<<20 || use.GetContainers() != 0 {
		t.Errorf("with no container, unused images = %d, containers %d", use.GetImagesUnusedBytes(), use.GetContainers())
	}
}

// A count that failed isn't tried again for 30 s, so each page that opens doesn't
// ask a Docker that's struggling once more; then it is.
func TestDiskUseRemembersAFailure(t *testing.T) {
	e := dockertest.New(t)
	var mu sync.Mutex
	asked := 0
	e.Intercept(func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/system/df") {
			mu.Lock()
			defer mu.Unlock()
			asked++
			return errors.New("the disk is busy")
		}
		return nil
	})
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	d := &diskUse{docker: e.Client(), collector: &collect.Collector{}, now: func() time.Time { return now }}
	for range 3 {
		if _, err := d.read(t.Context()); err == nil || !strings.Contains(err.Error(), "the disk is busy") {
			t.Fatalf("read = %v, want what Docker said", err)
		}
	}
	if asked != 1 {
		t.Errorf("three asks within 30 s of a failure asked Docker %d times, want once", asked)
	}
	now = now.Add(diskUseFor)
	_, _ = d.read(t.Context())
	if asked != 2 {
		t.Errorf("an ask 30 s on asked Docker %d times in all, want twice", asked)
	}
}

// A Docker that doesn't answer where it keeps its data is given a few seconds,
// not the minute a count takes, and nobody waits on it for longer.
func TestDiskUseDoesntWaitLongForAHungDocker(t *testing.T) {
	was := infoWait
	infoWait = 100 * time.Millisecond
	t.Cleanup(func() { infoWait = was })
	e := dockertest.New(t)
	e.Intercept(func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/info") {
			<-r.Context().Done()
		}
		return nil
	})
	d := &diskUse{docker: e.Client(), collector: &collect.Collector{}, now: time.Now}
	start := time.Now()
	_, err := d.read(t.Context())
	if err == nil || !strings.Contains(err.Error(), "didn't say where it keeps its data") {
		t.Errorf("read = %v, want that Docker didn't answer", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("a hung Docker held the answer up for %s", took)
	}
}

// A count that takes too long says so in words the control plane can put after
// "web-1 can't say what takes its disk now:", without saying that twice.
func TestDiskUseSaysACountTookTooLong(t *testing.T) {
	was := diskUseWait
	diskUseWait = 100 * time.Millisecond
	t.Cleanup(func() { diskUseWait = was })
	e := dockertest.New(t)
	e.Intercept(func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/system/df") {
			<-r.Context().Done()
		}
		return nil
	})
	d := &diskUse{docker: e.Client(), collector: &collect.Collector{}, now: time.Now}
	_, err := d.read(t.Context())
	if err == nil || !strings.Contains(err.Error(), "took more than") || strings.Contains(err.Error(), "can't say") {
		t.Errorf("a count that took too long: %v", err)
	}
}
