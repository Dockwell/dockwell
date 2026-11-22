// SPDX-License-Identifier: AGPL-3.0-only

package dockerdaemon

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource/dockerlog"
)

// ReadLoggingSummary is a separate, bounded read so the settings view can
// show the Engine and its effective defaults without waiting for inspections.
func (d *Driver) ReadLoggingSummary(ctx context.Context, settings *v1.DockerSettings) {
	if settings.GetSource() == "" || (settings.GetLogging().GetDriver() != "json-file" && settings.GetLogging().GetDriver() != "local") {
		return
	}
	check, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	rows, err := d.workloads(check)
	if err != nil {
		return
	}
	var mismatch uint32
	for i := range rows {
		if rows[i].State != "running" {
			continue
		}
		c, err := d.Client.InspectContainer(check, rows[i].ID)
		if err != nil || c.HostConfig.LogConfig == nil {
			return
		}
		if !sameLogging(c.HostConfig.LogConfig, settings.GetLogging()) {
			mismatch++
		}
	}
	settings.RunningLoggingMismatch = &mismatch
}

func sameLogging(logs *docker.LogConfig, defaults *spec.DockerDaemonLogging) bool {
	if logs.Type != defaults.GetDriver() {
		return false
	}
	got, want := effectiveLogging(logs.Type, dockerlog.Size(logs.Config["max-size"]), dockerlog.Files(logs.Config["max-file"]))
	size, files := effectiveLogging(defaults.GetDriver(), defaults.GetMaxSizeBytes(), defaults.GetMaxFiles())
	return got == size && want == files
}

func effectiveLogging(driver string, size uint64, files uint32) (uint64, uint32) {
	switch {
	case driver == "local":
		if size == 0 {
			size = 20 << 20
		}
		if files == 0 {
			files = 5
		}
	case size == 0:
		files = 0
	case files == 0:
		files = 1
	}
	return size, files
}

func checkMirrors(ctx context.Context, mirrors []string) []v1.DockerMirrorReachability {
	if len(mirrors) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	client := netguard.Client(netguard.Options{Timeout: 3 * time.Second, MaxBody: 1})
	states := make([]v1.DockerMirrorReachability, len(mirrors))
	var wg sync.WaitGroup
	for i, mirror := range mirrors {
		wg.Go(func() { states[i] = mirrorStatus(ctx, client, mirror) })
	}
	wg.Wait()
	return states
}

func mirrorStatus(ctx context.Context, client *netguard.HTTP, mirror string) v1.DockerMirrorReachability {
	const (
		skipped = v1.DockerMirrorReachability_DOCKER_MIRROR_REACHABILITY_NOT_CHECKED
		down    = v1.DockerMirrorReachability_DOCKER_MIRROR_REACHABILITY_UNREACHABLE
		up      = v1.DockerMirrorReachability_DOCKER_MIRROR_REACHABILITY_REACHABLE
	)
	if !validMirror(mirror) {
		return skipped
	}
	u, _ := url.Parse(mirror)
	if !strings.Contains(u.Hostname(), ".") && !strings.Contains(u.Hostname(), ":") {
		return skipped
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
			return skipped
		}
	}
	u.Path, u.RawPath = strings.TrimRight(u.Path, "/")+"/v2/", ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return skipped
	}
	resp, err := client.Do(req)
	if errors.Is(err, netguard.ErrForbidden) {
		return skipped
	}
	if err != nil {
		return down
	}
	defer resp.Body.Close()
	// Authenticated registries usually respond to the v2 ping with a challenge.
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized {
		return up
	}
	return down
}
