// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"io/fs"
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
