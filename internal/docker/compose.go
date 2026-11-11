// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
)

// The labels Docker Compose puts on what it makes, which Dockwell puts on
// what it makes too, so docker compose sees and manages it.
const (
	ComposePrefix = "com.docker.compose."
	LabelProject  = ComposePrefix + "project"
	LabelService  = ComposePrefix + "service"
	LabelNumber   = ComposePrefix + "container-number"
	LabelOneoff   = ComposePrefix + "oneoff"
	// LabelWorkingDir is where Compose ran, which often says what ran it.
	LabelWorkingDir = ComposePrefix + "project.working_dir"
	LabelVolume     = ComposePrefix + "volume"
	LabelNetwork    = ComposePrefix + "network"
	// LabelConfigHash is the hash of a container's configuration. Compose
	// lists only the containers that carry one as its project's, and
	// recreates one whose hash isn't what it computes. A volume or a
	// network carries none: Compose would recreate one whose hash differs,
	// which empties a volume.
	LabelConfigHash = ComposePrefix + "config-hash"
)

// OwnPrefix starts the labels that are Dockwell's own.
const OwnPrefix = "dev.dockwell."

// LabelIngress marks what runs a server's ingress: the proxy, and what keeps its
// network. They are Dockwell's own, and belong to no project.
const LabelIngress = OwnPrefix + "ingress"

// portainerStack is where Portainer runs a stack, in its own data directory.
var portainerStack = regexp.MustCompile(`^/data/compose/\d+(/|$)`)

// ManagedBy names the tool that runs a container's Compose project, as its labels
// show it, or nothing when they don't.
func ManagedBy(labels map[string]string) string {
	dir := labels[LabelWorkingDir]
	switch {
	case strings.Contains(dir, "/1panel/"), labels["createdBy"] == "Apps":
		return "1Panel"
	case labels["coolify.managed"] == "true":
		return "Coolify"
	case portainerStack.MatchString(dir):
		return "Portainer"
	case strings.HasPrefix(dir, "/var/lib/casaos/"):
		return "CasaOS"
	}
	return ""
}

// LabelVolumeTool marks a throwaway container that reads or writes a volume for
// a backup or a restore (ADR 0033).
const LabelVolumeTool = OwnPrefix + "volume-tool"

// LabelDrill marks a restore drill's container and volume, with the drill's ID, which
// it throws away whatever comes of it (ADR 0068).
const LabelDrill = OwnPrefix + "drill"

// IsOwnLabel reports a label Dockwell or Compose sets, which a spec can't.
func IsOwnLabel(key string) bool {
	return strings.HasPrefix(key, ComposePrefix) || strings.HasPrefix(key, OwnPrefix)
}

// Project is the Compose project of what lives in a scope: an environment,
// or a server for what runs on its own there. It is the scope's ID in
// lowercase, such as "env_01kat4zr6e3v0y5xbkzb5d9q6m", which Compose takes
// as a project name and never changes, as a name could.
func Project(scope string) string { return strings.ToLower(scope) }

// ContainerName is what Compose names the first container of a service,
// such as "env_01kat…-web-1".
func ContainerName(project, service string) string { return InstanceName(project, service, 1) }

// InstanceName is what Compose names a service's container of a number, such
// as "env_01kat…-web-3".
func InstanceName(project, service string, number int) string {
	return project + "-" + service + "-" + strconv.Itoa(number)
}

// ReadableName is what a service's container of a number is named after what
// people call its environment, scope, such as "default.production.web-1",
// rather than its Compose project, which is the environment's ID (ADR 0035).
func ReadableName(scope, service string, number int) string {
	return scope + "." + service + "-" + strconv.Itoa(number)
}

// ScopedName is what Compose names a project's volume or network, such as
// "env_01kat…_data".
func ScopedName(project, name string) string { return project + "_" + name }

// IngressNetwork is the network a server's ingress reaches services on: a
// bridge of its own, with no way out and no way between its containers, on
// which each has a static address (ADR 0024). No spec may name it.
const IngressNetwork = "dockwell_ingress"

// AnonymousVolume reports a volume Docker made for a path an image declares,
// which it names by 64 hex digits.
func AnonymousVolume(name string) bool {
	if len(name) != 64 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}
