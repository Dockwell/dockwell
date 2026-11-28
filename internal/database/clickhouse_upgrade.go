// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

const (
	clickHouseVersionLabel = "com.clickhouse.build.version"
	clickHouseDataDir      = "/var/lib/clickhouse"
)

func clickHouseMountSpec(app *resourcev1.Container) (Found, bool) {
	for _, m := range app.GetMounts() {
		if path.Clean(m.GetTarget()) == clickHouseDataDir {
			return Found{Engine: Of(v1.DatabaseEngine_DATABASE_ENGINE_CLICKHOUSE), Volume: m.GetVolume(), External: m.GetExternal()}, true
		}
	}
	return Found{}, false
}

// IsClickHouseContainer also recognizes a registry mirror by its label or data mount.
func IsClickHouseContainer(c *docker.Container) bool {
	e := Recognise(c.Config.Image)
	return c.Config.Labels[clickHouseVersionLabel] != "" || e != nil && e.ClickHouse() || hasClickHouseMount(c)
}

func hasClickHouseMount(c *docker.Container) bool {
	return slices.ContainsFunc(c.Mounts, func(m docker.MountPoint) bool { return path.Clean(m.Destination) == clickHouseDataDir })
}

func clickHouseMove(name string, was Found, fromImage, toImage string) (*Move, error) {
	if fromImage == toImage {
		return nil, nil
	}
	from, to := Version(fromImage), Version(toImage)
	if err := checkClickHouseVersions(name, from, to, fromImage, toImage); err != nil {
		return nil, err
	}
	_, digest, _ := strings.Cut(toImage, "@sha256:")
	if digest == "" {
		return nil, fmt.Errorf("the new ClickHouse image for %s isn't pinned, so its release could change. Choose an image with a version and digest", name)
	}
	if from == to {
		return nil, nil
	}
	volume := was.Volume
	if was.External {
		volume = ""
	}
	return &Move{Engine: was.Engine, From: from, To: to, Volume: volume, RequiresBackup: Major(fromImage) != Major(toImage)}, nil
}

// ClickHouse recommends upgrading to a release less than a year newer.
func checkClickHouseVersions(name, from, to, fromImage, toImage string) error {
	oldDate, oldOK := clickHouseReleaseMonth(from)
	newDate, newOK := clickHouseReleaseMonth(to)
	switch {
	case !oldOK:
		return unknownClickHouseVersion(name, fromImage)
	case !newOK:
		return unknownClickHouseVersion(name, toImage)
	case newer(from, to):
		return fmt.Errorf("ClickHouse on %s runs %s and can't downgrade to %s. Keep its current release, or restore a backup into a separate instance", name, from, to)
	case newDate-oldDate >= 12:
		return fmt.Errorf("ClickHouse %s is at least a year newer than %s, so %s can't move there in one step. Upgrade to an intermediate release first", to, from, name)
	}
	return nil
}

func unknownClickHouseVersion(name, image string) error {
	return fmt.Errorf("the image %s has no verifiable full version, so Dockwell can't check the ClickHouse upgrade on %s. Choose an image with its full version tag, such as 26.3.62.3, and its digest", image, name)
}

func clickHouseReleaseMonth(version string) (int, bool) {
	parts := strings.Split(version, ".")
	if len(parts) < 3 {
		return 0, false
	}
	for _, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return 0, false
		}
	}
	year, yearErr := strconv.Atoi(parts[0])
	month, monthErr := strconv.Atoi(parts[1])
	return year*12 + month, yearErr == nil && monthErr == nil && year > 0 && month >= 1 && month <= 12
}

// Check the real source and target against the releases used to plan the backup.
func (r Reader) checkClickHouseImage(ctx context.Context, old *docker.Container, spec *resourcev1.Container) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	image := spec.GetImage()
	next, err := r.Client.InspectImage(ctx, docker.WithTag(image))
	if err != nil {
		return "", fmt.Errorf("can't read the pulled ClickHouse image %s: %w", image, err)
	}
	if !docker.IsImageID(next.ID) {
		return "", fmt.Errorf("the Docker image %s has no reported ID, so Dockwell can't verify the ClickHouse replacement. Pull the pinned image again", image)
	}
	if next.ID == old.Image {
		return next.ID, nil
	}
	fromTag, toTag := Version(old.Config.Image), Version(image)
	if e := Recognise(old.Config.Image); (e == nil || !e.ClickHouse()) && !hasClickHouseMount(old) {
		return "", fmt.Errorf("%s runs ClickHouse without its standard data mount, so Dockwell can't plan an upgrade backup for this image. Use the standard /var/lib/clickhouse mount or restore a backup into a new instance", old.Name)
	}
	if err := checkClickHouseVersions(old.Name, fromTag, toTag, old.Config.Image, image); err != nil {
		return "", err
	}
	e := Of(v1.DatabaseEngine_DATABASE_ENGINE_CLICKHOUSE)
	from := Version("clickhouse:" + old.Config.Labels[clickHouseVersionLabel])
	if old.State.Running {
		cmd := e.WithPasswordFile(clickHouseClient(PortOf(e, old.Config.Cmd), "", "--format=TabSeparated"), old.Config.Env)
		if said, err := r.ask(ctx, old.ID, e, cmd, "SELECT version()\n"); err == nil {
			from = strings.TrimSpace(string(said))
		}
	}
	if _, ok := clickHouseReleaseMonth(from); !ok {
		if built, err := r.Client.InspectImage(ctx, old.Image); err == nil {
			from = Version("clickhouse:" + built.Config.Labels[clickHouseVersionLabel])
			if _, valid := clickHouseReleaseMonth(from); !valid {
				from = r.clickHouseBinaryVersion(ctx, built.ID)
			}
		}
	}
	to := Version("clickhouse:" + next.Config.Labels[clickHouseVersionLabel])
	if _, ok := clickHouseReleaseMonth(to); !ok {
		to = r.clickHouseBinaryVersion(ctx, next.ID)
	}
	if err := checkClickHouseVersions(old.Name, from, to, old.Config.Image, image); err != nil {
		return "", err
	}
	if from != fromTag || to != toTag {
		return "", fmt.Errorf("ClickHouse on %s runs %s and the pulled image contains %s, so the planned move from %s to %s stopped. Pin the images with their actual versions and plan again", old.Name, from, to, fromTag, toTag)
	}
	fromYear, _, _ := strings.Cut(from, ".")
	toYear, _, _ := strings.Cut(to, ".")
	if fromYear != toYear && !r.clickHouseManagedData(ctx, old, spec) {
		return "", fmt.Errorf("ClickHouse on %s keeps its data outside the managed volume for this upgrade, so Dockwell can't back it up before %s starts. Move its data to a volume Dockwell manages before planning the upgrade, or restore a backup into a new instance", old.Name, to)
	}
	return next.ID, nil
}

// Both containers must use the volume the plan can back up. Docker gives an
// anonymous volume a name too, but it doesn't have the managed volume's labels.
func (r Reader) clickHouseManagedData(ctx context.Context, old *docker.Container, spec *resourcev1.Container) bool {
	mount, ok := clickHouseMountSpec(spec)
	project := old.Config.Labels[docker.LabelProject]
	if !ok || mount.Volume == "" || mount.External || project == "" {
		return false
	}
	name := docker.ScopedName(project, mount.Volume)
	if !slices.ContainsFunc(old.Mounts, func(m docker.MountPoint) bool {
		return path.Clean(m.Destination) == clickHouseDataDir && m.Type == "volume" && m.Name == name
	}) {
		return false
	}
	v, err := r.Client.InspectVolume(ctx, name)
	return err == nil && v.Labels[docker.LabelProject] == project && v.Labels[docker.LabelVolume] == mount.Volume
}

func (r Reader) clickHouseBinaryVersion(ctx context.Context, imageID string) string {
	if imageID == "" {
		return ""
	}
	req := &docker.CreateRequest{
		Config:     docker.Config{Image: imageID, Entrypoint: []string{"/usr/bin/clickhouse"}, Cmd: []string{"server", "--version"}},
		HostConfig: docker.HostConfig{NetworkMode: "none", Memory: 128 << 20, NanoCPUs: 1e9, LogConfig: docker.Rotation()},
	}
	id, err := r.Client.CreateContainer(ctx, "", req)
	if err != nil {
		return ""
	}
	defer func() {
		clean, cancel := resource.Finishing(ctx, 10*time.Second)
		defer cancel()
		_ = r.Client.RemoveOneOff(clean, id)
	}()
	if err := r.Client.StartContainer(ctx, id); err != nil {
		return ""
	}
	if code, err := r.Client.WaitContainer(ctx, id); err != nil || code != 0 {
		return ""
	}
	logs, err := r.Client.ContainerLogs(ctx, id, false, docker.LogOptions{})
	if err != nil {
		return ""
	}
	defer logs.Close()
	for {
		line, err := logs.Next()
		if err != nil || line == nil {
			return ""
		}
		if rest, ok := strings.CutPrefix(string(line.Text), "ClickHouse server version "); ok {
			version, _, _ := strings.Cut(rest, " ")
			version = strings.TrimSuffix(version, ".")
			if _, valid := clickHouseReleaseMonth(version); valid {
				return version
			}
		}
	}
}
