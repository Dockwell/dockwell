// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"maps"
	"net/netip"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

const dockerInventoryPage = 24

var errDockerInventory = errors.New("the Engine could not answer the Docker inventory read")

func dockerInventoryList(ctx context.Context, d *docker.Client, ask *agentv1.DockerInventoryListCall) (*agentv1.DockerInventoryListResult, error) {
	if d == nil {
		return nil, errDockerInventory
	}
	out := &agentv1.DockerInventoryListResult{}
	summary := &v1.DockerServerSummary{ObservedAt: timestamppb.Now()}
	projects := make(map[string]struct{})
	limit := int(min(ask.GetPageSize(), dockerInventoryPage))
	if limit == 0 {
		limit = dockerInventoryPage
	}
	if ask.GetComposeMembers() {
		limit = 256
	}
	filter := ask.GetFilter()
	search := strings.ToLower(strings.TrimSpace(filter.GetSearch()))
	var snapshot [sha256.Size]byte
	err := d.EachContainer(ctx, nil, func(s *docker.ContainerSummary) error {
		if !docker.IsFullID(s.ID) || inventoryHelper(s.Labels) {
			return nil
		}
		health, exit := inventoryStatus(s.Status)
		attention := health == v1.DockerHealth_DOCKER_HEALTH_UNHEALTHY || s.State == "restarting" || s.State == "exited" && exit != 0
		if ask.GetOverview() {
			summary.Containers++
			if attention {
				summary.NeedsAttention++
				if len(summary.Attention) < 5 {
					summary.Attention = append(summary.Attention, inventorySummary(s))
				}
			}
			if s.State == "running" {
				summary.Running++
			}
			if project := composeIdentity(s.Labels[docker.LabelProject]); project != "" && composeIdentity(s.Labels[docker.LabelService]) != "" && !strings.EqualFold(s.Labels[docker.LabelOneoff], "true") {
				projects[project] = struct{}{}
			}
			return nil
		}
		state := containerState(s.State)
		project := composeIdentity(s.Labels[docker.LabelProject])
		if ask.GetComposeMembers() && (s.Labels[docker.LabelIngress] != "" || strings.EqualFold(s.Labels[docker.LabelOneoff], "true")) {
			return nil
		}
		name := plainName(s.Names)
		if filter.GetState() != 0 && filter.GetState() != state || filter.GetStopped() && state == v1.ContainerState_CONTAINER_STATE_RUNNING {
			return nil
		}
		if filter.GetNeedsAttention() && !attention {
			return nil
		}
		if wanted := filter.GetComposeProject(); wanted != "" && wanted != project {
			return nil
		}
		if search != "" && !strings.Contains(strings.ToLower(name+" "+s.Image+" "+s.ID+" "+project), search) {
			return nil
		}
		// XOR the per-object hashes so the cursor is independent of Engine ordering.
		identity := s.ID + "\x00" + name + "\x00" + s.Image + "\x00" + s.State + "\x00" + project
		if ask.GetComposeMembers() {
			identity = s.ID + "\x00" + project + "\x00" + s.Labels[docker.LabelService] + "\x00" + s.Labels[docker.LabelWorkingDir]
		}
		hash := sha256.Sum256([]byte(identity))
		for i := range snapshot {
			snapshot[i] ^= hash[i]
		}
		if s.ID <= ask.GetAfterId() {
			return nil
		}
		index, _ := slices.BinarySearchFunc(out.Objects, s.ID, func(row *v1.DockerObject, id string) int {
			return strings.Compare(row.GetKey().GetEngineId(), id)
		})
		if index <= limit {
			var row *v1.DockerObject
			if ask.GetComposeMembers() {
				body := &v1.DockerContainer{
					ComposeProject: project, ComposeService: composeIdentity(s.Labels[docker.LabelService]),
					ComposeDirectory: composeDirectory(s.Labels[docker.LabelWorkingDir]), Health: health,
				}
				if s.Created > 0 {
					body.CreatedAt = timestamppb.New(time.Unix(s.Created, 0))
				}
				if s.State == "exited" && exit != 0 {
					body.ExitCode = &exit
				}
				row = &v1.DockerObject{Key: inventoryKey(s.ID), State: state, Detail: &v1.DockerObject_Container{Container: body}}
			} else {
				row = inventorySummary(s)
			}
			out.Objects = slices.Insert(out.Objects, index, row)
			if len(out.Objects) > limit+1 {
				out.Objects = out.Objects[:limit+1]
			}
		}
		return nil
	})
	if err != nil {
		return nil, errDockerInventory
	}
	if ask.GetOverview() {
		info, err := d.InventoryInfo(ctx)
		if err != nil {
			return nil, errDockerInventory
		}
		summary.Images, summary.Networks, summary.Volumes = info.Images, info.Networks, info.Volumes
		summary.RootDirectory, summary.LoggingDriver = bounded(info.DockerRootDir, 1024), composeIdentity(info.LoggingDriver)
		summary.LiveRestore = info.LiveRestoreEnabled
		for _, mirror := range info.RegistryConfig.Mirrors[:min(len(info.RegistryConfig.Mirrors), listEach)] {
			// Show the origin. Credentials, path parameters and queries stay on the server.
			if u, err := url.Parse(mirror); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" {
				summary.RegistryMirrors = append(summary.RegistryMirrors, bounded(u.Scheme+"://"+u.Host, 255))
			}
		}
		_, summary.EngineVersion = d.Version()
		summary.ComposeProjects = uint32(len(projects)) //nolint:gosec // container counts fit in uint32.
		out.Overview = summary
		return out, nil
	}
	out.Snapshot = hex.EncodeToString(snapshot[:])
	if ask.GetSnapshot() != "" && ask.GetSnapshot() != out.Snapshot {
		out.Objects, out.Resync = nil, true
		return out, nil
	}
	if len(out.Objects) > limit {
		out.Objects = out.Objects[:limit]
		out.NextId = out.Objects[limit-1].GetKey().GetEngineId()
	}
	if ask.GetComposeMembers() {
		return out, nil
	}
	// Read the true start time for the page, with a fixed upper bound on concurrent reads.
	for begin := 0; begin < len(out.Objects); begin += 4 {
		var group sync.WaitGroup
		for _, row := range out.Objects[begin:min(begin+4, len(out.Objects))] {
			group.Go(func() {
				if held, err := d.InspectContainer(ctx, row.GetKey().GetEngineId()); err == nil && held.ID == row.GetKey().GetEngineId() {
					row.GetContainer().StartedAt = inventoryTime(held.State.StartedAt)
					row.GetContainer().Health = inventoryHealth(held.State.Health)
					row.GetContainer().RestartCount = countOf(held.RestartCount)
				}
			})
		}
		group.Wait()
	}
	return out, nil
}

func inventoryHelper(labels map[string]string) bool {
	for _, label := range []string{docker.LabelVolumeTool, docker.LabelDrill, docker.LabelCopy, docker.LabelProbe} {
		if _, ok := labels[label]; ok {
			return true
		}
	}
	return false
}

func composeIdentity(value string) string {
	if len(value) > 255 || strings.ContainsFunc(value, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.'
	}) {
		return ""
	}
	return value
}

func composeDirectory(value string) string {
	if len(value) <= 512 && path.IsAbs(value) && !strings.ContainsFunc(value, unicode.IsControl) {
		return path.Clean(value)
	}
	return ""
}

func inventoryTime(value time.Time) *timestamppb.Timestamp {
	if value.IsZero() || value.Year() < 1970 {
		return nil
	}
	return timestamppb.New(value)
}

func inventoryKey(id string) *v1.DockerObjectKey {
	return &v1.DockerObjectKey{Kind: v1.DockerObjectKind_DOCKER_OBJECT_KIND_CONTAINER, EngineId: id}
}

func inventorySummary(s *docker.ContainerSummary) *v1.DockerObject {
	_, system := s.Labels[docker.LabelIngress]
	old := serverContainer(s)
	body := &v1.DockerContainer{
		Image: bounded(s.Image, 512), ImageId: bounded(s.ImageID, 128), CreatedAt: old.GetCreateTime(),
		ComposeProject: composeIdentity(old.GetComposeProject()), ComposeService: composeIdentity(old.GetComposeService()), OneOff: old.GetOneOff(),
		ComposeDirectory: composeDirectory(s.Labels[docker.LabelWorkingDir]),
		Ports:            old.GetPorts(), Endpoints: inventoryEndpoints(s.NetworkSettings.Networks),
		DetailsTruncated: old.GetMorePorts() > 0 || len(s.NetworkSettings.Networks) > listEach,
	}
	for _, port := range body.Ports {
		port.Address = inventoryAddress(port.GetAddress())
	}
	health, exit := inventoryStatus(s.Status)
	body.Health = health
	if s.State == "exited" && exit != 0 {
		body.ExitCode = &exit
	}
	row := &v1.DockerObject{
		Key: inventoryKey(s.ID), Name: bounded(old.GetName(), 255), State: old.GetState(), ObservedAt: timestamppb.Now(),
		Detail: &v1.DockerObject_Container{Container: body},
	}
	if system {
		row.Ownership = v1.DockerOwnership_DOCKER_OWNERSHIP_SYSTEM
	}
	return row
}

func dockerInventoryDetail(ctx context.Context, d *docker.Client, ask *agentv1.DockerInventoryDetailCall) (*agentv1.DockerInventoryDetailResult, error) {
	if d == nil || !docker.IsFullID(ask.GetContainerId()) {
		return nil, errDockerInventory
	}
	out := &agentv1.DockerInventoryDetailResult{}
	id := ask.GetContainerId()
	held, err := d.InspectContainer(ctx, id)
	if docker.IsNotFound(err) || err == nil && held.ID != id {
		if name := composeIdentity(ask.GetLastKnownName()); name != "" {
			next, err := d.ContainerNamed(ctx, name)
			if err == nil && docker.IsFullID(next.ID) && next.ID != id && !inventoryHelper(next.Config.Labels) {
				out.ReplacementKey = inventoryKey(next.ID)
			}
		}
		return out, nil
	}
	if err != nil {
		return nil, errDockerInventory
	}
	if inventoryHelper(held.Config.Labels) {
		return out, nil
	}
	row := inventorySummary(&docker.ContainerSummary{
		ID: id, Names: []string{held.Name}, Image: held.Config.Image, ImageID: held.Image,
		Created: held.Created.Unix(), Labels: held.Config.Labels, State: held.State.Status,
	})
	body := row.GetContainer()
	body.StartedAt, body.FinishedAt = inventoryTime(held.State.StartedAt), inventoryTime(held.State.FinishedAt)
	body.Health = inventoryHealth(held.State.Health)
	body.OomKilled = held.State.OOMKilled
	if !held.State.FinishedAt.IsZero() && !held.State.Running && held.State.ExitCode >= -1 && held.State.ExitCode <= 255 {
		exit := int32(held.State.ExitCode)
		body.ExitCode = &exit
	}
	body.RestartCount = countOf(held.RestartCount)
	body.RestartPolicy = composeIdentity(held.HostConfig.RestartPolicy.Name)
	body.RestartRetries = countOf(held.HostConfig.RestartPolicy.MaximumRetryCount)
	body.MemoryLimitBytes = positive(held.HostConfig.Memory)
	body.CpuLimitNanos = positive(held.HostConfig.NanoCPUs)
	body.Endpoints = inventoryEndpoints(held.NetworkSettings.Networks)
	body.Ports = bindingPorts(held.NetworkSettings.Ports)
	body.ConfiguredPorts = bindingPorts(held.HostConfig.PortBindings)
	body.Mounts = mountsOf(held.Mounts[:min(len(held.Mounts), listEach)])
	body.DetailsTruncated = len(held.Mounts) > listEach || len(held.NetworkSettings.Networks) > listEach ||
		len(body.Ports) == listEach || len(body.ConfiguredPorts) == listEach
	if log := held.HostConfig.LogConfig; log != nil {
		body.LogDriver = composeIdentity(log.Type)
		// Rotation numbers are the only options projected. Reject even a
		// known key's value when it is not a size or a bounded count.
		size := log.Config["max-size"]
		number := strings.TrimRight(size, "kKmMgGbB")
		if _, err := strconv.ParseUint(number, 10, 64); err == nil && len(size) <= 24 {
			body.LogMaxSize = size
		}
		if files, err := strconv.ParseUint(log.Config["max-file"], 10, 32); err == nil {
			body.LogMaxFiles = uint32(files)
		}
	}
	out.Object = row
	return out, nil
}

func countOf(n int) uint32 {
	return uint32(min(max(n, 0), 1<<32-1))
}

// inventoryStatus reads Docker's own fixed health and exit markers. The
// human-readable status string never leaves the agent.
func inventoryStatus(status string) (v1.DockerHealth, int32) {
	for _, word := range []string{"healthy", "unhealthy", "health: starting", "starting"} {
		if strings.HasSuffix(status, "("+word+")") {
			word = strings.TrimPrefix(word, "health: ")
			return inventoryHealth(&docker.Health{Status: word}), 0
		}
	}
	if rest, ok := strings.CutPrefix(status, "Exited ("); ok {
		code, _, _ := strings.Cut(rest, ")")
		n, _ := strconv.ParseInt(code, 10, 32)
		return 0, int32(n)
	}
	return 0, 0
}

func inventoryHealth(health *docker.Health) v1.DockerHealth {
	if health == nil {
		return 0
	}
	switch health.Status {
	case "starting":
		return v1.DockerHealth_DOCKER_HEALTH_STARTING
	case "healthy":
		return v1.DockerHealth_DOCKER_HEALTH_HEALTHY
	case "unhealthy":
		return v1.DockerHealth_DOCKER_HEALTH_UNHEALTHY
	default:
		return 0
	}
}

func inventoryAddress(addr string) string {
	ip, err := netip.ParseAddr(addr)
	if err != nil {
		return ""
	}
	return ip.String()
}

func inventoryEndpoints(networks map[string]*docker.EndpointSettings) []*v1.DockerEndpoint {
	var result []*v1.DockerEndpoint
	names := slices.Sorted(maps.Keys(networks))
	for _, name := range names[:min(len(names), listEach)] {
		if n := networks[name]; n != nil {
			result = append(result, &v1.DockerEndpoint{
				Network: bounded(name, 255), NetworkId: fullNetworkID(n.NetworkID), Ipv4: inventoryAddress(n.IPAddress), Ipv6: inventoryAddress(n.GlobalIPv6Address),
			})
		}
	}
	return result
}

func bindingPorts(ports map[string][]docker.PortBinding) []*v1.ContainerPort {
	var result []*v1.ContainerPort
	for _, key := range slices.Sorted(maps.Keys(ports)) {
		port, protocol, ok := strings.Cut(key, "/")
		n, err := strconv.ParseUint(port, 10, 16)
		if !ok || err != nil || n == 0 || protocol != "tcp" && protocol != "udp" && protocol != "sctp" {
			continue
		}
		for _, b := range ports[key] {
			host, _ := strconv.ParseUint(b.HostPort, 10, 16)
			result = append(result, &v1.ContainerPort{
				ServerPort: uint32(host), ContainerPort: uint32(n), Address: inventoryAddress(b.HostIP), Protocol: protocol,
			})
			if len(result) == listEach {
				return result
			}
		}
	}
	return result
}

func bounded(value string, size int) string {
	if len(value) > size {
		return strings.ToValidUTF8(value[:size], "")
	}
	return value
}

func fullNetworkID(id string) string {
	if docker.IsFullID(id) {
		return id
	}
	return ""
}
