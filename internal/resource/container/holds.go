// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/dockwell/dockwell/internal/docker"
)

// reading is what a container's settings are read against.
type reading struct {
	c *docker.Container
	// imageSignal is the stop signal its image gives it.
	imageSignal string
	info        *docker.Info
}

// A rule reads the value of a key of a container's settings, and names what it runs
// with that a replace would drop, in words for people. Most need nothing but a name:
// see only.
type rule func(raw json.RawMessage, r *reading) []string

// kept is the rule of a key that a replace can't lose: a spec holds it, or it makes no
// difference to what runs.
func kept(json.RawMessage, *reading) []string { return nil }

// only is the rule of a key no spec holds: Docker's zero is fine, and so is what the
// engine gives everyone, as a replace gets that too. Anything else is name.
func only(name string, defaults ...string) rule {
	return func(raw json.RawMessage, _ *reading) []string {
		if isZero(raw) || slices.Contains(defaults, string(bytes.TrimSpace(raw))) {
			return nil
		}
		return []string{name}
	}
}

// isZero reports what Docker says of a setting no one set: null, false, 0, "", and
// what holds only those. What can't be read isn't zero.
func isZero(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var v any
	return json.Unmarshal(raw, &v) == nil && zeroValue(v)
}

func zeroValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case float64:
		return x == 0
	case string:
		return x == ""
	case []any:
		return !slices.ContainsFunc(x, func(e any) bool { return !zeroValue(e) })
	case map[string]any:
		for _, e := range x {
			if !zeroValue(e) {
				return false
			}
		}
		return true
	}
	return false
}

// hostConfigRules are the keys of a container's HostConfig that Dockwell knows. It's
// an allow list: a key that isn't here and isn't zero is refused by its own name, so
// a setting Docker adds later can't be dropped without a word. Each is named as a
// Compose file names it, where one does.
var hostConfigRules = map[string]rule{
	// A spec holds these, as Project reads them.
	"CapAdd": kept, "CapDrop": kept, "GroupAdd": kept, "ExtraHosts": kept, "Memory": kept, "MemoryReservation": kept,
	"NanoCpus": kept, "Privileged": kept, "RestartPolicy": kept, "ShmSize": kept,
	// These say how it was started, or follow from what's refused below by name: the
	// paths Docker masks depend on its version and on security_opt and privileged.
	"ConsoleSize": kept, "ContainerIDFile": kept, "MaskedPaths": kept, "ReadonlyPaths": kept,

	"Binds": readBinds, "CgroupnsMode": readCgroupns, "LogConfig": readLogging, "MemorySwap": readSwap,
	"Mounts": readMountList, "NetworkMode": readNetworkMode, "PidsLimit": readPidsLimit, "Runtime": readRuntime,
	"PortBindings": readPortBindings, "Tmpfs": readTmpfs,

	"Annotations": only("annotations"), "AutoRemove": only("auto remove (--rm)"),
	"BlkioDeviceReadBps": only("blkio_config"), "BlkioDeviceReadIOps": only("blkio_config"),
	"BlkioDeviceWriteBps": only("blkio_config"), "BlkioDeviceWriteIOps": only("blkio_config"),
	"BlkioWeight": only("blkio_config"), "BlkioWeightDevice": only("blkio_config"),
	"Cgroup": only("cgroup"), "CgroupParent": only("cgroup_parent"), "CpuCount": only("cpu_count"),
	"CpuPercent": only("cpu_percent"), "CpuPeriod": only("cpu_period"), "CpuQuota": only("cpu_quota"),
	"CpuRealtimePeriod": only("cpu_rt_period"), "CpuRealtimeRuntime": only("cpu_rt_runtime"),
	"CpuShares": only("cpu_shares"), "CpusetCpus": only("cpuset"), "CpusetMems": only("cpuset memory nodes"),
	"DeviceCgroupRules": only("device_cgroup_rules"), "DeviceRequests": only("gpus"), "Devices": only("devices"),
	"Dns": only("dns"), "DnsOptions": only("dns_opt"), "DnsSearch": only("dns_search"),
	"IOMaximumBandwidth": only("an I/O limit"), "IOMaximumIOps": only("an I/O limit"),
	"Init": only("init"), "IpcMode": only("ipc", `"private"`, `"shareable"`), "Isolation": only("isolation", `"default"`),
	"Links": only("links"), "MemorySwappiness": only("mem_swappiness"), "OomKillDisable": only("oom_kill_disable"),
	"OomScoreAdj": only("oom_score_adj"), "PidMode": only("pid"), "PublishAllPorts": only("publish_all_ports (-P)"),
	"ReadonlyRootfs": only("read_only"), "SecurityOpt": only("security_opt"),
	"StorageOpt": only("storage_opt"), "Sysctls": only("sysctls"), "UTSMode": only("uts"),
	"Ulimits": only("ulimits"), "Umask": only("umask"), "UsernsMode": only("userns_mode"),
	"VolumeDriver": only("volume_driver"), "VolumesFrom": only("volumes_from"),
}

// configRules are the same for a container's Config.
var configRules = map[string]rule{
	// A spec holds these.
	"Cmd": kept, "Entrypoint": kept, "Env": kept, "ExposedPorts": kept, "Image": kept, "Labels": kept, "StopTimeout": kept,
	"User": kept, "WorkingDir": kept,
	// How it was started, and what its image says, which it says again. A replace
	// carries each anonymous volume over, whoever asked for it: see createRequest.
	"ArgsEscaped": kept, "AttachStderr": kept, "AttachStdin": kept, "AttachStdout": kept,
	"OnBuild": kept, "Shell": kept, "StdinOnce": kept, "Volumes": kept,

	"Healthcheck": readHealthcheck, "Hostname": readHostname, "StopSignal": readStopSignal,

	"Domainname": only("domainname"), "MacAddress": only("mac_address"), "NetworkDisabled": only("network_mode: none"),
	"OpenStdin": only("stdin_open"), "Tty": only("tty"),
}

// noSpecHolds lists what a container runs with that no spec can say, so a replace
// would drop it without a word. What Dockwell can't run is refused by name, never
// dropped (ADR 0021), and being taken on is the promise that a later change of image
// is safe.
func noSpecHolds(c *docker.Container, o *docker.Options, imageSignal string, info *docker.Info) []string {
	r := &reading{c: c, imageSignal: imageSignal, info: info}
	out := readKeys("Config", o.Config, configRules, r)
	out = append(out, readKeys("HostConfig", o.HostConfig, hostConfigRules, r)...)
	for _, name := range slices.Sorted(maps.Keys(o.NetworkSettings.Networks)) {
		out = append(out, readEndpoint(name, o.NetworkSettings.Networks[name])...)
	}
	// Two keys can come to one name, as blkio_config does.
	slices.Sort(out)
	return slices.Compact(out)
}

// leaningOn names what depends on c in a way a replace would cut off, each with how:
// a container that shares its network has only loopback once it has gone. It reads
// each container on the server, stopped ones too, as one starts on what it was made
// with, so it's for a person's plan and not for a list.
func (d *Docker) leaningOn(ctx context.Context, c *docker.Container) ([]string, error) {
	list, err := d.Client.ListContainers(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("can't list what shares this container: %w", err)
	}
	var out []string
	for i := range list {
		if list[i].ID == c.ID {
			continue
		}
		o, err := d.Client.InspectOptions(ctx, list[i].ID)
		switch {
		case docker.IsNotFound(err):
			continue
		case err != nil:
			return nil, fmt.Errorf("can't read what shares this container: %w", err)
		}
		out = append(out, leans(o, c)...)
	}
	slices.Sort(out)
	return out, nil
}

// leans says how o depends on c, by its name.
func leans(o *docker.Options, c *docker.Container) []string {
	name, self := strings.TrimPrefix(o.Name, "/"), strings.TrimPrefix(c.Name, "/")
	is := func(ref string) bool { return ref == self || len(ref) >= 12 && strings.HasPrefix(c.ID, ref) }
	var out []string
	for _, m := range []struct{ key, how string }{
		{"NetworkMode", "shares its network"}, {"PidMode", "shares its processes"}, {"IpcMode", "shares its shared memory"},
	} {
		var mode string
		_ = json.Unmarshal(o.HostConfig[m.key], &mode)
		if ref, ok := strings.CutPrefix(mode, "container:"); ok && is(ref) {
			out = append(out, name+" "+m.how)
		}
	}
	// Each is a name or an ID, then what it's called here or how it's mounted.
	sources := func(key string) bool {
		var list []string
		_ = json.Unmarshal(o.HostConfig[key], &list)
		return slices.ContainsFunc(list, func(s string) bool {
			source, _, _ := strings.Cut(s, ":")
			return is(strings.TrimPrefix(source, "/"))
		})
	}
	if sources("Links") {
		out = append(out, name+" links to it")
	}
	if sources("VolumesFrom") {
		out = append(out, name+" mounts its volumes")
	}
	return out
}

func readKeys(section string, keys map[string]json.RawMessage, rules map[string]rule, r *reading) []string {
	var out []string
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		if read, known := rules[key]; known {
			out = append(out, read(keys[key], r)...)
		} else if !isZero(keys[key]) {
			out = append(out, section+"."+key)
		}
	}
	return out
}

// readEndpoint names what a container asks of a network that a spec can't. Most of
// what Docker says of one is how it's connected now, so this one lists what matters.
func readEndpoint(network string, ep map[string]json.RawMessage) []string {
	var out []string
	for _, s := range []struct{ key, name string }{
		{"IPAMConfig", "ipv4_address"}, {"DriverOpts", "driver_opts"}, {"Links", "links"}, {"GwPriority", "gw_priority"},
	} {
		if !isZero(ep[s.key]) {
			out = append(out, s.name+" on "+network)
		}
	}
	return out
}

// readPortBindings reads what's published. A spec holds one server port for each, over
// TCP or UDP. Docker keeps "" for a port it picks itself, and a range as it was written.
func readPortBindings(raw json.RawMessage, _ *reading) []string {
	var bindings map[string][]struct {
		HostPort string `json:"HostPort"`
	}
	if json.Unmarshal(raw, &bindings) != nil {
		return []string{"HostConfig.PortBindings"}
	}
	var out []string
	for _, port := range slices.Sorted(maps.Keys(bindings)) {
		if strings.HasSuffix(port, "/sctp") {
			out = append(out, "the sctp port "+port)
		}
		for _, b := range bindings[port] {
			if n, err := strconv.ParseUint(b.HostPort, 10, 16); err != nil || n == 0 {
				out = append(out, "a server port Docker picks for "+port)
				break
			}
		}
	}
	return out
}

func readBinds(raw json.RawMessage, _ *reading) []string {
	var binds []string
	if json.Unmarshal(raw, &binds) != nil {
		return []string{"HostConfig.Binds"}
	}
	var out []string
	for _, b := range binds {
		parts := strings.Split(b, ":")
		if len(parts) < 3 {
			continue
		}
		for o := range strings.SplitSeq(parts[2], ",") {
			switch o {
			case "", "ro", "rw", "cached", "delegated", "consistent", "rprivate":
				// Consistency means nothing on Linux, and rprivate is the default.
			case "z", "Z":
				if !strings.HasPrefix(parts[0], "/") {
					out = append(out, o+" on "+parts[1]) // Dockwell relabels a server path alone
				}
			default:
				out = append(out, o+" on "+parts[1])
			}
		}
	}
	return out
}

func readMountList(raw json.RawMessage, _ *reading) []string {
	var mounts []map[string]json.RawMessage
	if json.Unmarshal(raw, &mounts) != nil {
		return []string{"HostConfig.Mounts"}
	}
	var out []string
	for _, m := range mounts {
		var kind, target string
		_ = json.Unmarshal(m["Type"], &kind)
		_ = json.Unmarshal(m["Target"], &target)
		if kind == "tmpfs" {
			out = append(out, readTmpfsMount(m, target)...)
			continue
		}
		if kind != "volume" && kind != "bind" {
			out = append(out, fmt.Sprintf("a %s mount at %s", kind, target))
			continue
		}
		for _, key := range slices.Sorted(maps.Keys(m)) {
			switch key {
			case "Type", "Source", "Target", "ReadOnly", "Consistency":
			case "BindOptions":
				out = append(out, readBindOptions(m[key], target)...)
			default:
				if !isZero(m[key]) {
					out = append(out, key+" on "+target)
				}
			}
		}
	}
	return out
}

// readTmpfs reads what a container keeps in memory, by the path of each. A spec holds
// that and its size. Docker gives every tmpfs rw, noexec, nosuid and nodev.
func readTmpfs(raw json.RawMessage, _ *reading) []string {
	var tmpfs map[string]string
	if json.Unmarshal(raw, &tmpfs) != nil {
		return []string{"HostConfig.Tmpfs"}
	}
	var out []string
	for _, target := range slices.Sorted(maps.Keys(tmpfs)) {
		for o := range strings.SplitSeq(tmpfs[target], ",") {
			name, _, _ := strings.Cut(o, "=")
			switch name {
			case "", "size", "rw", "noexec", "nosuid", "nodev":
			default:
				out = append(out, "tmpfs option "+name+" on "+target)
			}
		}
	}
	return out
}

// readTmpfsMount reads what docker compose sends for a volume of type tmpfs, which a
// spec holds as it holds any other.
func readTmpfsMount(m map[string]json.RawMessage, target string) []string {
	var out []string
	if !isZero(m["ReadOnly"]) {
		out = append(out, "read_only on "+target)
	}
	var options map[string]json.RawMessage
	if len(m["TmpfsOptions"]) > 0 && json.Unmarshal(m["TmpfsOptions"], &options) != nil {
		return append(out, "TmpfsOptions on "+target)
	}
	for _, key := range slices.Sorted(maps.Keys(options)) {
		if key != "SizeBytes" && !isZero(options[key]) {
			out = append(out, key+" on "+target)
		}
	}
	return out
}

func readBindOptions(raw json.RawMessage, target string) []string {
	var options map[string]json.RawMessage
	if json.Unmarshal(raw, &options) != nil {
		return []string{"BindOptions on " + target}
	}
	var out []string
	for _, key := range slices.Sorted(maps.Keys(options)) {
		var propagation string
		switch {
		case key == "CreateMountpoint": // a replace's bind makes the path too
		case key == "Propagation" && json.Unmarshal(options[key], &propagation) == nil:
			if propagation != "" && propagation != "rprivate" {
				out = append(out, propagation+" on "+target)
			}
		case !isZero(options[key]):
			out = append(out, key+" on "+target)
		}
	}
	return out
}

// readLogging reads how Docker keeps a container's log. A replace takes the engine's
// default, with Dockwell's rotation on json-file (logConfig), so a limit on its size
// is one a replace sets anyway, and anything else would be lost.
func readLogging(raw json.RawMessage, r *reading) []string {
	var l struct {
		Type   string            `json:"Type"`
		Config map[string]string `json:"Config"`
	}
	if json.Unmarshal(raw, &l) != nil {
		return []string{"HostConfig.LogConfig"}
	}
	if l.Type != "" && l.Type != r.info.LoggingDriver {
		return []string{"logging: " + l.Type}
	}
	var out []string
	for _, key := range slices.Sorted(maps.Keys(l.Config)) {
		if key != "max-size" && key != "max-file" {
			out = append(out, "logging option "+key)
		}
	}
	return out
}

// readSwap reads a limit on memory and swap together. Docker gives a container with a
// memory limit twice that of it, which a replace gets too.
func readSwap(raw json.RawMessage, r *reading) []string {
	var swap int64
	if json.Unmarshal(raw, &swap) != nil {
		return []string{"HostConfig.MemorySwap"}
	}
	if swap != 0 && swap != 2*r.c.HostConfig.Memory {
		return []string{"memswap_limit"}
	}
	return nil
}

func readPidsLimit(raw json.RawMessage, _ *reading) []string {
	var limit int64
	if !isZero(raw) && (json.Unmarshal(raw, &limit) != nil || limit > 0) {
		return []string{"pids_limit"}
	}
	return nil
}

func readNetworkMode(raw json.RawMessage, _ *reading) []string {
	var mode string
	if json.Unmarshal(raw, &mode) != nil {
		return []string{"HostConfig.NetworkMode"}
	}
	if mode == "none" || strings.HasPrefix(mode, "container:") {
		return []string{"network_mode: " + mode}
	}
	return nil
}

// readCgroupns reads a container's cgroup namespace. Docker gives one of its own on
// cgroup v2, and the server's on v1.
func readCgroupns(raw json.RawMessage, r *reading) []string {
	var mode string
	if json.Unmarshal(raw, &mode) != nil {
		return []string{"HostConfig.CgroupnsMode"}
	}
	if mode == "" || mode == "private" || mode == "host" && r.info.CgroupVersion == "1" {
		return nil
	}
	return []string{"cgroupns_mode: " + mode}
}

func readRuntime(raw json.RawMessage, r *reading) []string {
	var runtime string
	if json.Unmarshal(raw, &runtime) != nil {
		return []string{"HostConfig.Runtime"}
	}
	if runtime == "" || runtime == "runc" || runtime == r.info.DefaultRuntime {
		return nil
	}
	return []string{"runtime: " + runtime}
}

// readHostname reads a container's hostname: its own ID unless someone said, and the
// server's on the server's own network.
func readHostname(raw json.RawMessage, r *reading) []string {
	var name string
	if json.Unmarshal(raw, &name) != nil {
		return []string{"Config.Hostname"}
	}
	onHost := r.c.HostConfig.NetworkMode == "host" && name == r.info.Name
	if name == "" || strings.HasPrefix(r.c.ID, name) || onHost {
		return nil
	}
	return []string{"hostname"}
}

func readStopSignal(raw json.RawMessage, r *reading) []string {
	var signal string
	if json.Unmarshal(raw, &signal) != nil {
		return []string{"Config.StopSignal"}
	}
	if signal != "" && signal != r.imageSignal {
		return []string{"stop_signal"}
	}
	return nil
}

// readHealthcheck reads what a spec's health check can't hold.
func readHealthcheck(raw json.RawMessage, _ *reading) []string {
	var check map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &check) != nil {
		return nil
	}
	if !isZero(check["StartInterval"]) {
		return []string{"healthcheck start_interval"}
	}
	return nil
}
