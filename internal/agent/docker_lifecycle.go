// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	dwid "github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/host"
)

const (
	lifecycleStart   = v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_START
	lifecycleStop    = v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_STOP
	lifecycleRestart = v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_RESTART
	lifecyclePause   = v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_PAUSE
	lifecycleUnpause = v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_UNPAUSE
	lifecycleKill    = v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_KILL
	lifecycleRemove  = v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_REMOVE
	lifecycleRename  = v1.DockerContainerAction_DOCKER_CONTAINER_ACTION_RENAME

	maxLifecycleTargets = 64
	receiptApplied      = v1.DockerReceiptState_DOCKER_RECEIPT_STATE_APPLIED
)

var errProtectedContainer = errors.New("this container belongs to Dockwell or Docker infrastructure. Use its own controls")

func (c *calls) lifecycleTarget(ctx context.Context, id string, includeVolumes bool) (*v1.DockerContainerTarget, error) {
	item, err := c.cfg.Docker.InspectExternalContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	if item.ID != id {
		return nil, errProtectedContainer
	}
	for label := range item.Config.Labels {
		if strings.HasPrefix(label, docker.OwnPrefix) || strings.HasPrefix(label, "com.docker.swarm.") {
			return nil, errProtectedContainer
		}
	}
	name := strings.TrimPrefix(item.Name, "/")
	hostname, _ := os.Hostname()
	if reservedLifecycleName(name) ||
		dockwellRepository(item.Config.Image) ||
		(len(hostname) >= 12 && strings.HasPrefix(id, hostname)) {
		return nil, errProtectedContainer
	}
	if !validLifecycleName(name) {
		return nil, errors.New("the Engine did not report a usable container name")
	}
	out := &v1.DockerContainerTarget{
		EngineId: id, Name: name, State: item.State.Status,
		ComposeProject: composeIdentity(item.Config.Labels[docker.LabelProject]),
		ComposeService: composeIdentity(item.Config.Labels[docker.LabelService]),
		HostAccess:     container.ExternalHostAccess(ctx, c.cfg.Docker, item),
	}
	var mounts []string
	for _, m := range item.Mounts {
		if !includeVolumes || m.Type != "volume" || explicitVolumeSource(item, m) {
			continue
		}
		if len(mounts) == 64 {
			return nil, errors.New("this container has too many mounts to review volume removal")
		}
		volume, err := c.cfg.Docker.InspectVolume(ctx, m.Name)
		if err != nil || volume.Name != m.Name {
			return nil, errors.New("the Engine could not verify a mounted volume. Review removal without volumes")
		}
		if volume.Driver != "local" || volume.Scope != "local" || !isAnonymousVolume(volume) || c.volumeKept(m.Name) {
			return nil, errors.New("a mounted volume is protected. Review removal without volumes")
		}
		mounts = append(mounts, m.Name+"@"+volume.CreatedAt)
		out.AnonymousVolumes = append(out.AnonymousVolumes, m.Name)
	}
	slices.Sort(mounts)
	slices.Sort(out.AnonymousVolumes)
	out.AnonymousVolumes = slices.Compact(out.AnonymousVolumes)
	out.Evidence = cleanupEvidence(id, name, out.ComposeProject, out.ComposeService, out.State, item.Image,
		item.State.StartedAt.String(), item.State.FinishedAt.String(), strconv.Itoa(item.RestartCount),
		strconv.FormatBool(out.HostAccess), strings.Join(mounts, "\x00"))
	return out, nil
}

func explicitVolumeSource(item *docker.Container, mount docker.MountPoint) bool {
	for _, bound := range item.HostConfig.Binds {
		source, rest, ok := strings.Cut(bound, ":")
		target, _, _ := strings.Cut(rest, ":")
		if ok && source == mount.Name && target == mount.Destination {
			return true
		}
	}
	return slices.ContainsFunc(item.HostConfig.Mounts, func(m docker.Mount) bool {
		return m.Type == "volume" && m.Source == mount.Name && m.Target == mount.Destination
	})
}

func reservedLifecycleName(name string) bool {
	name = strings.ToLower(name)
	return name == "dockwell" || strings.HasPrefix(name, "dockwell-") || strings.HasPrefix(name, "dockwell_")
}

func validLifecycleName(name string) bool {
	return name != "" && composeIdentity(name) == name && name[0] != '-' && name[0] != '_' && name[0] != '.'
}

func lifecycleEligible(action v1.DockerContainerAction, state string) bool {
	switch action {
	case lifecycleStart:
		return state == "created" || state == "exited"
	case lifecycleStop, lifecycleKill:
		return state == "running" || state == "restarting" || state == "paused"
	case lifecycleRestart:
		return state == "running" || state == "restarting"
	case lifecyclePause:
		return state == "running"
	case lifecycleUnpause:
		return state == "paused"
	case lifecycleRemove:
		return state == "created" || state == "exited" || state == "dead"
	case lifecycleRename:
		return state == "created" || state == "running" || state == "paused" || state == "restarting" || state == "exited" || state == "dead"
	default:
		return false
	}
}

func (c *calls) dockerLifecyclePreview(ctx context.Context, ask *agentv1.DockerLifecyclePreviewCall) (*agentv1.DockerLifecyclePreviewResult, error) {
	if c.cfg.Docker == nil || c.cfg.Identity == nil || len(ask.GetKeys()) == 0 || len(ask.GetKeys()) > maxLifecycleTargets {
		return nil, errors.New("select up to 64 Docker containers or Compose projects per server")
	}
	if (ask.GetAction() == lifecycleRename) != (ask.GetNewName() != "") || ask.GetNewName() != "" && !validLifecycleName(ask.GetNewName()) {
		return nil, errors.New("enter a valid Docker container name")
	}
	ids := make(map[string]string)
	projects := make(map[string]bool)
	for _, key := range ask.GetKeys() {
		if key.GetServerId() != c.cfg.Identity.ServerID {
			return nil, errors.New("this target belongs to a different server")
		}
		switch key.GetKind() {
		case v1.DockerObjectKind_DOCKER_OBJECT_KIND_CONTAINER:
			if !docker.IsFullID(key.GetEngineId()) {
				return nil, errors.New("select a container by its full Docker ID")
			}
			ids[key.GetEngineId()] = ""
		case v1.DockerObjectKind_DOCKER_OBJECT_KIND_COMPOSE:
			project, ok := strings.CutPrefix(key.GetEngineId(), "outside:")
			if !ok || composeIdentity(project) != project || project == "" {
				return nil, errors.New("select an external Compose project")
			}
			projects[project] = true
		default:
			return nil, errors.New("select a container or an external Compose project")
		}
	}
	if len(projects) > 0 {
		found := make(map[string]bool, len(projects))
		err := c.cfg.Docker.EachContainer(ctx, nil, func(item *docker.ContainerSummary) error {
			project := item.Labels[docker.LabelProject]
			if !projects[project] || item.Labels[docker.LabelService] == "" || strings.EqualFold(item.Labels[docker.LabelOneoff], "true") {
				return nil
			}
			found[project], ids[item.ID] = true, project
			if len(ids) > maxLifecycleTargets {
				return errors.New("select up to 64 Compose members per server")
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(found) != len(projects) {
			return nil, errors.New("a selected Compose project is no longer on this server. Refresh and review again")
		}
	}
	out := &agentv1.DockerLifecyclePreviewResult{}
	names := make(map[string]bool)
	for id, project := range ids {
		var target *v1.DockerContainerTarget
		if !docker.IsFullID(id) {
			return nil, errors.New("the Engine returned an incomplete container identity")
		}
		err := c.cfg.Docker.Adopted.WithExternal(id, func() error {
			var err error
			target, err = c.lifecycleTarget(ctx, id, ask.GetRemoveAnonymousVolumes())
			return err
		})
		if err != nil {
			return nil, err
		}
		if ask.GetRemoveAnonymousVolumes() && len(target.GetAnonymousVolumes()) != 0 && c.swarmBlocksVolumeCleanup(ctx) {
			return nil, errors.New("the Engine could not verify local volume removal. Review removal without volumes")
		}
		if project != "" && target.GetComposeProject() != project {
			return nil, errors.New("the Compose project changed. Refresh the list and review again")
		}
		if !lifecycleEligible(ask.GetAction(), target.GetState()) {
			if project != "" && ask.GetAction() == lifecycleRemove {
				return nil, errors.New("stop all containers in the Compose project before removing it")
			}
			continue
		}
		if ask.GetAction() == lifecycleRename {
			name := ask.GetNewName()
			if len(ids) > 1 || project != "" {
				name += "-" + target.GetName()
			}
			if !validLifecycleName(name) || reservedLifecycleName(name) || names[name] || target.GetName() == name {
				return nil, errors.New("choose a new, unique name for each container")
			}
			if _, err := c.cfg.Docker.ContainerNamed(ctx, name); !docker.IsNotFound(err) {
				return nil, errors.New("the Engine could not confirm the new container name is free. Choose a different name")
			}
			names[name] = true
			target.NewName = name
		}
		out.Targets = append(out.Targets, target)
	}
	slices.SortFunc(out.Targets, func(a, b *v1.DockerContainerTarget) int { return strings.Compare(a.GetEngineId(), b.GetEngineId()) })
	return out, nil
}

func (c *calls) checkDockerLifecycle(ref resource.Ref, plan *v1.DockerMaintenance) error {
	life := plan.GetLifecycle()
	created, err := dwid.Parse(dwid.Change, plan.GetChangeId())
	if c.cfg.Docker == nil || c.cfg.disks == nil || c.cfg.Identity == nil || c.cfg.Dir == "" ||
		ref.Kind != host.Kind || ref.Name != host.Name || ref.Scope != c.cfg.Identity.ServerID ||
		err != nil || c.cleanupNow().Sub(created) >= cleanupReceiptRetention ||
		len(life.GetTargets()) == 0 || len(life.GetTargets()) > maxLifecycleTargets ||
		len(plan.GetTargets()) > 0 || plan.GetImageAction() != nil || plan.GetResumeCleanupToken() != "" {
		return errors.New("this agent could not verify the reviewed Docker lifecycle plan")
	}
	if life.GetAction() == lifecycleKill {
		if life.GetTimeoutSeconds() > 60 || life.GetSignal() != "" && (life.GetTimeoutSeconds() != 0 || !docker.ValidLifecycleSignal(life.GetSignal())) {
			return errors.New("enter a signal name or an escalation timeout between 1 and 60 seconds")
		}
	} else if life.GetSignal() != "" || life.GetTimeoutSeconds() != 0 {
		return errors.New("signals and escalation timeouts apply to Kill")
	}
	if life.GetRemoveAnonymousVolumes() && life.GetAction() != lifecycleRemove {
		return errors.New("anonymous volumes can only be removed with their container")
	}
	seen := make(map[string]bool)
	for _, target := range life.GetTargets() {
		id := target.GetEngineId()
		if !docker.IsFullID(id) || !validLifecycleName(target.GetName()) || !docker.IsFullID(target.GetEvidence()) || seen[id] ||
			!lifecycleEligible(life.GetAction(), target.GetState()) ||
			(life.GetAction() == lifecycleRename) != validLifecycleName(target.GetNewName()) || reservedLifecycleName(target.GetNewName()) ||
			!life.GetRemoveAnonymousVolumes() && len(target.GetAnonymousVolumes()) != 0 {
			return errors.New("the reviewed Docker lifecycle plan has an invalid target")
		}
		if life.GetAction() == lifecycleKill && target.GetState() == "paused" && !docker.AllowedPausedKillSignal(life.GetSignal()) {
			return errors.New("paused containers accept only the default kill or SIGKILL")
		}
		seen[id] = true
	}
	return nil
}

func (c *calls) runDockerLifecycle(ctx context.Context, ref resource.Ref, plan *v1.DockerMaintenance, held *agentv1.DockerCleanupProtection) error {
	if err := c.checkDockerLifecycle(ref, plan); err != nil {
		return err
	}
	life := plan.GetLifecycle()
	if life.GetRemoveAnonymousVolumes() && held == nil {
		return errors.New("the agent did not receive current Docker volume protections")
	}
	if life.GetRemoveAnonymousVolumes() && c.swarmBlocksVolumeCleanup(ctx) {
		return errors.New("the Engine could not verify local volume removal. Review removal without volumes")
	}
	c.cfg.disks.lifecycleMu.Lock()
	defer c.cfg.disks.lifecycleMu.Unlock()
	dir := filepath.Join(c.cfg.Dir, "docker-cleanup")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file := filepath.Join(dir, cleanupEvidence("lifecycle", plan.GetChangeId())+".json")
	var receipts []*v1.DockerReceipt
	raw, err := os.ReadFile(file) //nolint:gosec // File name is a hash under the agent data directory.
	if err == nil {
		err = json.Unmarshal(raw, &receipts)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("the agent could not read its Docker lifecycle receipts")
	}
	if len(receipts) > len(life.GetTargets()) {
		return errors.New("the agent could not verify its Docker lifecycle receipts")
	}
	save := func() error {
		data, err := json.Marshal(receipts)
		if err == nil {
			err = writeDockerCleanupFile(file, data)
		}
		return err
	}
	defer func() { resource.Report(ctx, resource.Outcome{DockerReceipts: receipts}) }()
	for i, receipt := range receipts {
		if !proto.Equal(receipt.GetContainerTarget(), life.GetTargets()[i]) {
			return errors.New("the agent could not verify its Docker lifecycle receipts")
		}
		if receipt.GetState() == receiptUnknown {
			return errors.New("an earlier Docker action could not be confirmed. Inspect the container before reviewing another action")
		}
	}
	for _, target := range life.GetTargets()[len(receipts):] {
		if err := ctx.Err(); err != nil {
			return err
		}
		receipt := &v1.DockerReceipt{ContainerTarget: target, State: receiptChanged}
		receipts = append(receipts, receipt)
		started := false
		err := c.cfg.Docker.Adopted.WithExternal(target.GetEngineId(), func() error {
			current, err := c.lifecycleTarget(ctx, target.GetEngineId(), life.GetRemoveAnonymousVolumes())
			if err != nil || current.GetEvidence() != target.GetEvidence() || current.GetHostAccess() != target.GetHostAccess() || !slices.Equal(current.GetAnonymousVolumes(), target.GetAnonymousVolumes()) {
				receipt.Reason = receiptReasonReferenceMoved
				switch {
				case docker.IsNotFound(err):
					receipt.Reason = receiptReasonGone
				case errors.Is(err, errProtectedContainer):
					receipt.Reason = receiptReasonProtected
				case err != nil:
					receipt.State = receiptFailed
				}
				return nil
			}
			if len(current.GetAnonymousVolumes()) > 0 && c.swarmBlocksVolumeCleanup(ctx) {
				receipt.Reason = receiptReasonProtected
				return nil
			}
			if slices.ContainsFunc(current.GetAnonymousVolumes(), func(name string) bool {
				return slices.Contains(held.GetVolumes(), name) || slices.Contains(held.GetBackupVolumes(), name) ||
					slices.Contains(held.GetRollbackVolumes(), name)
			}) {
				receipt.Reason = receiptReasonProtected
				return nil
			}
			receipt.State = receiptUnknown
			if err := save(); err != nil {
				receipt.State = receiptFailed
				return fmt.Errorf("the agent could not save its Docker lifecycle receipt before dispatch: %w", err)
			}
			started = true
			return c.applyContainerAction(ctx, life, target)
		})
		if errors.Is(err, docker.ErrContainerAdopted) {
			receipt.Reason = receiptReasonProtected
		} else if !started && err != nil {
			return err
		}
		if started && err == nil {
			receipt.State = receiptApplied
		} else if started {
			var refused *docker.Error
			if errors.As(err, &refused) && refused.Status >= 400 && refused.Status < 500 {
				receipt.State = receiptFailed
			}
		}
		if err := save(); err != nil {
			return errors.New("the agent could not save its Docker lifecycle receipt")
		}
		if receipt.GetState() == receiptUnknown {
			return errors.New("the Engine did not confirm the action. Inspect the target before reviewing a new action")
		}
	}
	if slices.ContainsFunc(receipts, func(r *v1.DockerReceipt) bool { return r.GetState() != receiptApplied }) {
		return errors.New("some containers changed or Docker refused the action. Review their receipts")
	}
	return nil
}

func (c *calls) applyContainerAction(ctx context.Context, life *v1.DockerContainerLifecycle, target *v1.DockerContainerTarget) error {
	d, id := c.cfg.Docker, target.GetEngineId()
	const defaultGrace = 10 * time.Second
	switch life.GetAction() {
	case lifecycleStart:
		return d.StartContainer(ctx, id)
	case lifecycleStop:
		return d.StopContainer(ctx, id, defaultGrace)
	case lifecycleRestart:
		return d.RestartContainer(ctx, id, defaultGrace)
	case lifecyclePause:
		return d.PauseContainer(ctx, id)
	case lifecycleUnpause:
		return d.UnpauseContainer(ctx, id)
	case lifecycleKill:
		if life.GetSignal() != "" {
			if target.GetState() == "paused" && !docker.AllowedPausedKillSignal(life.GetSignal()) {
				return errors.New("paused containers accept only the default kill or SIGKILL")
			}
			return d.SignalContainer(ctx, id, life.GetSignal())
		}
		grace := time.Duration(life.GetTimeoutSeconds()) * time.Second
		if grace == 0 {
			grace = defaultGrace
		}
		return d.TerminateContainer(ctx, id, grace)
	case lifecycleRemove:
		return d.RemoveExternalContainer(ctx, id, len(target.GetAnonymousVolumes()) > 0)
	case lifecycleRename:
		return d.RenameContainer(ctx, id, target.GetNewName())
	default:
		return fmt.Errorf("unknown Docker lifecycle action %d", life.GetAction())
	}
}
