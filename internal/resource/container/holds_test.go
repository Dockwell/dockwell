// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/dockwell/dockwell/internal/docker"
)

// hostDefaults is what Docker 28 says of a container nobody asked anything of, key by
// key, on cgroup v2. None of it may be a reason to refuse one.
const hostDefaults = `{
	"Binds": null, "ContainerIDFile": "", "LogConfig": {"Type": "json-file", "Config": {}}, "NetworkMode": "bridge",
	"PortBindings": {}, "RestartPolicy": {"Name": "no", "MaximumRetryCount": 0}, "AutoRemove": false, "VolumeDriver": "",
	"VolumesFrom": null, "ConsoleSize": [0, 0], "CapAdd": null, "CapDrop": null, "CgroupnsMode": "private", "Dns": [],
	"DnsOptions": [], "DnsSearch": [], "ExtraHosts": null, "GroupAdd": null, "IpcMode": "private", "Cgroup": "", "Links": null,
	"OomScoreAdj": 0, "PidMode": "", "Privileged": false, "PublishAllPorts": false, "ReadonlyRootfs": false, "SecurityOpt": null,
	"UTSMode": "", "UsernsMode": "", "ShmSize": 67108864, "Runtime": "runc", "Isolation": "", "CpuShares": 0, "Memory": 0,
	"NanoCpus": 0, "CgroupParent": "", "BlkioWeight": 0, "BlkioWeightDevice": [], "BlkioDeviceReadBps": [],
	"BlkioDeviceWriteBps": [], "BlkioDeviceReadIOps": [], "BlkioDeviceWriteIOps": [], "CpuPeriod": 0, "CpuQuota": 0,
	"CpuRealtimePeriod": 0, "CpuRealtimeRuntime": 0, "CpusetCpus": "", "CpusetMems": "", "Devices": [], "DeviceCgroupRules": null,
	"DeviceRequests": null, "MemoryReservation": 0, "MemorySwap": 0, "MemorySwappiness": null, "OomKillDisable": false,
	"PidsLimit": null, "Ulimits": [], "CpuCount": 0, "CpuPercent": 0, "IOMaximumIOps": 0, "IOMaximumBandwidth": 0,
	"MaskedPaths": ["/proc/asound", "/proc/acpi", "/proc/kcore", "/sys/firmware"], "ReadonlyPaths": ["/proc/bus", "/proc/fs"]
}`

const configDefaults = `{
	"Hostname": "0123456789ab", "Domainname": "", "User": "", "AttachStdin": false, "AttachStdout": true, "AttachStderr": true,
	"ExposedPorts": {"80/tcp": {}}, "Tty": false, "OpenStdin": false, "StdinOnce": false, "Env": ["PATH=/bin"], "Cmd": ["nginx"],
	"Image": "nginx:1.27-alpine", "Volumes": null, "WorkingDir": "", "Entrypoint": ["/docker-entrypoint.sh"], "OnBuild": null,
	"Labels": {}, "StopSignal": "SIGQUIT"
}`

// options reads what an engine says, with each of edits over its HostConfig.
func options(t *testing.T, edits map[string]string) *docker.Options {
	t.Helper()
	o := &docker.Options{}
	if err := json.Unmarshal([]byte(configDefaults), &o.Config); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(hostDefaults), &o.HostConfig); err != nil {
		t.Fatal(err)
	}
	for k, v := range edits {
		o.HostConfig[k] = json.RawMessage(v)
	}
	return o
}

func TestWhatEveryContainerHasIsNoReason(t *testing.T) {
	c := &docker.Container{ID: "0123456789abcdef"}
	info := &docker.Info{Name: "hz-1", LoggingDriver: "json-file", DefaultRuntime: "runc", CgroupVersion: "2"}
	for _, s := range []struct {
		name  string
		edits map[string]string
		info  *docker.Info
		host  bool
	}{
		{name: "a plain docker run"},
		{name: "a memory limit, with the swap Docker gives it", edits: map[string]string{"Memory": "1073741824", "MemorySwap": "2147483648"}},
		{name: "what docker compose asks for", edits: map[string]string{
			"NetworkMode": `"shop_default"`, "Dns": "null", "DnsOptions": "null", "DnsSearch": "null", "PidsLimit": "0",
			"Mounts": `[{"Type": "volume", "Source": "shop_data", "Target": "/data", "VolumeOptions": {}}]`,
			"Binds":  `["/srv/app:/app:rw", "/srv/conf:/conf:ro,z"]`,
		}},
		{name: "a bind docker compose makes the path of", edits: map[string]string{
			"Mounts": `[{"Type": "bind", "Source": "/srv/app", "Target": "/app", "BindOptions": {"CreateMountpoint": true}}]`,
		}},
		{name: "rotation in the file", edits: map[string]string{"LogConfig": `{"Type": "json-file", "Config": {"max-size": "10m", "max-file": "3"}}`}},
		{name: "the server's own network", edits: map[string]string{"NetworkMode": `"host"`}, host: true},
		{
			name:  "another engine's logging",
			edits: map[string]string{"LogConfig": `{"Type": "journald", "Config": {}}`},
			info:  &docker.Info{LoggingDriver: "journald"},
		},
		{
			name:  "cgroup v1",
			edits: map[string]string{"CgroupnsMode": `"host"`},
			info:  &docker.Info{LoggingDriver: "json-file", CgroupVersion: "1"},
		},
	} {
		t.Run(s.name, func(t *testing.T) {
			using := info
			if s.info != nil {
				using = s.info
			}
			ctr := *c
			o := options(t, s.edits)
			if err := json.Unmarshal(o.HostConfig["Memory"], &ctr.HostConfig.Memory); err != nil {
				t.Fatal(err)
			}
			if s.host {
				ctr.HostConfig.NetworkMode = "host"
				o.Config["Hostname"] = json.RawMessage(`"hz-1"`)
			}
			if got := noSpecHolds(&ctr, o, "SIGQUIT", using); len(got) != 0 {
				t.Errorf("noSpecHolds = %v, want nothing", got)
			}
		})
	}
}

func TestEachThingASpecCantHoldIsNamed(t *testing.T) {
	c := &docker.Container{ID: "0123456789abcdef"}
	c.HostConfig.Memory = 1 << 30
	info := &docker.Info{LoggingDriver: "json-file", DefaultRuntime: "runc", CgroupVersion: "2"}
	for key, s := range map[string]struct {
		value string
		says  string
	}{
		"DeviceRequests":       {`[{"Driver": "nvidia"}]`, "gpus"},
		"Devices":              {`[{"PathOnHost": "/dev/fuse"}]`, "devices"},
		"LogConfig":            {`{"Type": "fluentd", "Config": {}}`, "logging: fluentd"},
		"PublishAllPorts":      {"true", "publish_all_ports (-P)"},
		"Mounts":               {`[{"Type": "tmpfs", "Target": "/cache"}]`, "a tmpfs mount at /cache"},
		"Binds":                {`["/srv/a:/a:rslave"]`, "rslave on /a"},
		"CpuQuota":             {"50000", "cpu_quota"},
		"MemorySwap":           {"-1", "memswap_limit"},
		"OomKillDisable":       {"true", "oom_kill_disable"},
		"UsernsMode":           {`"host"`, "userns_mode"},
		"ShmSize":              {"1073741824", "shm_size"},
		"IpcMode":              {`"host"`, "ipc"},
		"CgroupnsMode":         {`"host"`, "cgroupns_mode: host"},
		"NetworkMode":          {`"none"`, "network_mode: none"},
		"PidsLimit":            {"100", "pids_limit"},
		"Runtime":              {`"runsc"`, "runtime: runsc"},
		"SomethingDockerAdded": {`{"on": true}`, "HostConfig.SomethingDockerAdded"},
	} {
		t.Run(key, func(t *testing.T) {
			got := noSpecHolds(c, options(t, map[string]string{key: s.value}), "SIGQUIT", info)
			if !slices.Contains(got, s.says) {
				t.Errorf("noSpecHolds = %v, want %q", got, s.says)
			}
		})
	}

	// Inside a mount, what isn't known counts too.
	got := noSpecHolds(c, options(t, map[string]string{
		"Mounts": `[{"Type": "volume", "Source": "a", "Target": "/a", "VolumeOptions": {"NoCopy": true}},
			{"Type": "bind", "Source": "/b", "Target": "/b", "BindOptions": {"Propagation": "rshared"}}]`,
	}), "SIGQUIT", info)
	if !slices.Equal(got, []string{"VolumeOptions on /a", "rshared on /b"}) {
		t.Errorf("noSpecHolds of mount options = %v", got)
	}

	// A hostname of its own is one someone chose, on any network.
	o := options(t, nil)
	o.Config["Hostname"] = json.RawMessage(`"db-primary"`)
	o.Config["StopSignal"] = json.RawMessage(`"SIGTERM"`)
	o.Config["Healthcheck"] = json.RawMessage(`{"Test": ["CMD", "true"], "StartInterval": 1000000000}`)
	if got := noSpecHolds(c, o, "SIGQUIT", info); !slices.Equal(got, []string{"healthcheck start_interval", "hostname", "stop_signal"}) {
		t.Errorf("noSpecHolds of its Config = %v", got)
	}
}
