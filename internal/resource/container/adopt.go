// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// adopter is a Runtime that can take on what runs on a server already.
type adopter interface {
	ReadAdoption(ctx context.Context, t Target, id string) (*resourcev1.Container, []string, error)
	Adoption(ctx context.Context, t Target, id string, spec *resourcev1.Container) (resource.Decision, error)
	Adopt(ctx context.Context, t Target, id string, spec *resourcev1.Container) error
}

// ReadAdoptionAt reads a container as what t would run, and says what it runs with
// that no spec holds.
func (d *Driver) ReadAdoptionAt(ctx context.Context, t Target, id string) (*resourcev1.Container, []string, error) {
	a, ok := d.Runtime.(adopter)
	if !ok {
		return nil, nil, errors.New("this runtime can't take on a container")
	}
	return a.ReadAdoption(ctx, t, id)
}

// AdoptsAt says what taking a container on as t takes.
func (d *Driver) AdoptsAt(ctx context.Context, t Target, id string, spec *resourcev1.Container) (resource.Decision, error) {
	a, ok := d.Runtime.(adopter)
	if !ok {
		return resource.Decision{}, errors.New("this runtime can't take on a container")
	}
	return a.Adoption(ctx, t, id, spec)
}

// inspectToAdopt reads a container that is to be taken on as t: as it will read
// once it has been, so what it comes to is what Observe reads from then on.
func (d *Docker) inspectToAdopt(ctx context.Context, t Target, id string) (*docker.Container, *resourcev1.Container, error) {
	c, err := d.Client.InspectContainer(ctx, id)
	if docker.IsNotFound(err) {
		return nil, nil, fmt.Errorf("the container %.12s isn't on this server any more, so there's nothing to take on. Check what's there with docker ps -a", id)
	}
	if err != nil {
		return nil, nil, err
	}
	view := docker.TakenView(c, docker.Taken{Project: t.Project, Service: t.Service})
	spec, err := d.projected(ctx, t, view)
	if err != nil {
		return nil, nil, err
	}
	Normalize(spec)
	return c, spec, nil
}

// ReadAdoption reads a container as what t would run, and says what would stop it
// being taken on. Nothing is touched.
func (d *Docker) ReadAdoption(ctx context.Context, t Target, id string) (*resourcev1.Container, []string, error) {
	c, spec, err := d.inspectToAdopt(ctx, t, id)
	if err != nil {
		return nil, nil, err
	}
	options, err := d.Client.InspectOptions(ctx, c.ID)
	if err != nil {
		return nil, nil, err
	}
	var imageSignal string
	if img, err := d.Client.InspectImage(ctx, c.Image); err == nil && img.Config != nil {
		imageSignal = img.Config.StopSignal
	}
	refused := noSpecHolds(c, options, imageSignal)
	return spec, append(refused, valuesInWords(spec)...), nil
}

// noSpecHolds lists what a container runs with that no spec can say, so a replace
// would drop it without a word. What Dockwell can't run is refused by name, never
// dropped (ADR 0021), and being taken on is the promise that a later change of image
// is safe. Each is named as a Compose file names it.
func noSpecHolds(c *docker.Container, o *docker.Options, imageSignal string) []string {
	h := &o.HostConfig
	const defaultShm = 64 << 20
	set := []struct {
		name string
		on   bool
	}{
		{"hostname", o.Config.Hostname != "" && !strings.HasPrefix(c.ID, o.Config.Hostname)},
		{"domainname", o.Config.Domainname != ""},
		{"tty", o.Config.Tty},
		{"stdin_open", o.Config.OpenStdin},
		{"stop_signal", c.Config.StopSignal != "" && c.Config.StopSignal != imageSignal},
		{"devices", len(h.Devices) > 0},
		{"device_cgroup_rules", len(h.DeviceCgroups) > 0},
		{"sysctls", len(h.Sysctls) > 0},
		{"ulimits", len(h.Ulimits) > 0},
		{"shm_size", h.ShmSize != 0 && h.ShmSize != defaultShm},
		{"init", h.Init != nil && *h.Init},
		{"pid", h.PidMode != ""},
		{"ipc", h.IpcMode != "" && h.IpcMode != "private" && h.IpcMode != "shareable"},
		{"uts", h.UTSMode != ""},
		{"cgroup_parent", h.CgroupParent != ""},
		{"extra_hosts", len(h.ExtraHosts) > 0},
		{"dns", len(h.DNS) > 0},
		{"dns_search", len(h.DNSSearch) > 0},
		{"dns_opt", len(h.DNSOptions) > 0},
		{"links", len(h.Links) > 0},
		{"volumes_from", len(h.VolumesFrom) > 0},
		{"runtime", h.Runtime != "" && h.Runtime != "runc"},
		{"oom_score_adj", h.OomScoreAdj != 0},
		{"cpu_shares", h.CPUShares != 0},
		{"cpuset", h.CPUSetCPUs != ""},
		{"read_only", h.ReadonlyRootfs},
		{"tmpfs", len(h.Tmpfs) > 0},
		{"security_opt", len(h.SecurityOpt) > 0},
		{"pids_limit", h.PidsLimit != nil && *h.PidsLimit > 0},
		{"network_mode: " + h.NetworkMode, h.NetworkMode == "none" || strings.HasPrefix(h.NetworkMode, "container:")},
	}
	var out []string
	for _, s := range set {
		if s.on {
			out = append(out, s.name)
		}
	}
	return out
}

// valuesInWords names the variables a spec's words hold the values of. A plan shows
// a command, and a Compose file that wrote ${PASSWORD} into one leaves its value
// there on the container, so taking that on would show it. It says which by name, as
// a value of a few characters could be anywhere, so those are left out.
func valuesInWords(spec *resourcev1.Container) []string {
	const shortest = 6
	var words []string
	words = append(words, spec.GetCommand()...)
	words = append(words, spec.GetEntrypoint()...)
	words = append(words, spec.GetHealthcheck().GetTest()...)
	var out []string
	for name, value := range spec.GetEnv() {
		if len(value) >= shortest && slices.ContainsFunc(words, func(w string) bool { return strings.Contains(w, value) }) {
			out = append(out, "the value of "+name+" in its command")
		}
	}
	slices.Sort(out)
	return out
}

// Adoption checks a container can be taken on as t with what was read of it, which a
// person is about to approve, and says it costs nothing.
func (d *Docker) Adoption(ctx context.Context, t Target, id string, spec *resourcev1.Container) (resource.Decision, error) {
	if d.Client.Adopted == nil {
		return resource.Decision{}, errors.New("this agent keeps no record of containers it takes on, so it can't take one on")
	}
	c, got, err := d.inspectToAdopt(ctx, t, id)
	if err != nil {
		return resource.Decision{}, err
	}
	name := strings.TrimPrefix(c.Name, "/")
	if c.Config.Labels[docker.LabelAdopted] != "" {
		return resource.Decision{}, fmt.Errorf("%s is already what the service %s runs", name, c.Config.Labels[docker.LabelService])
	}
	all, err := d.instances(ctx, t)
	if err != nil {
		return resource.Decision{}, err
	}
	if len(all) > 0 {
		return resource.Decision{}, fmt.Errorf("the service %s already runs on this server, so it can't take on %s as well. Choose another name for it", t.Service, name)
	}
	if changes := resource.Diff(spec, got); len(changes) > 0 {
		return resource.Decision{}, fmt.Errorf("%s has changed since it was read: its %s differs. Take it on again, to plan it from what it is now", name, changes[0].Path)
	}
	return resource.Decision{Action: resource.ActionCreate, Adopts: c.ID}, nil
}

// Adopt records a container as what t runs, and does nothing else to it. Done
// already, as a step that resumes finds it, it has nothing to do.
func (d *Docker) Adopt(ctx context.Context, t Target, id string, spec *resourcev1.Container) error {
	c, err := d.Client.InspectContainer(ctx, id)
	if err == nil && c.Config.Labels[docker.LabelProject] == t.Project && c.Config.Labels[docker.LabelService] == t.Service && c.Config.Labels[docker.LabelAdopted] != "" {
		return nil
	}
	if _, err := d.Adoption(ctx, t, id, spec); err != nil {
		return err
	}
	return d.Client.Adopted.Take(id, docker.Taken{Project: t.Project, Service: t.Service})
}

// takenOn is the full ID of what t runs, while that is a container that was taken on
// and is all t runs: a rollout partway has replaced it already.
func (d *Docker) takenOn(ctx context.Context, t Target) string {
	if d.Client.Adopted == nil {
		return ""
	}
	all, err := d.instances(ctx, t)
	if err != nil || len(all) != 1 || all[0].Config.Labels[docker.LabelAdopted] == "" {
		return ""
	}
	return all[0].ID
}

// lettingGo says deleting t hands back a container that was taken on, which goes on
// running. Dockwell never made it, so deleting a service is no reason to remove it,
// and an undo of taking it on leaves it as it was.
func (d *Docker) lettingGo(ctx context.Context, t Target) bool { return d.takenOn(ctx, t) != "" }
