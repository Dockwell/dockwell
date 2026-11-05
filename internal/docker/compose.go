// SPDX-License-Identifier: AGPL-3.0-only

package docker

import "strings"

// The labels Docker Compose puts on what it makes, which Dockwell puts on
// what it makes too, so docker compose sees and manages it.
const (
	ComposePrefix = "com.docker.compose."
	LabelProject  = ComposePrefix + "project"
	LabelService  = ComposePrefix + "service"
	LabelNumber   = ComposePrefix + "container-number"
	LabelOneoff   = ComposePrefix + "oneoff"
	LabelVolume   = ComposePrefix + "volume"
	LabelNetwork  = ComposePrefix + "network"
	// LabelConfigHash is the hash of a container's configuration. Compose
	// lists only the containers that carry one as its project's, and
	// recreates one whose hash isn't what it computes. A volume or a
	// network carries none: Compose would recreate one whose hash differs,
	// which empties a volume.
	LabelConfigHash = ComposePrefix + "config-hash"
)

// OwnPrefix starts the labels that are Dockwell's own.
const OwnPrefix = "dev.dockwell."

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
func ContainerName(project, service string) string { return project + "-" + service + "-1" }

// ScopedName is what Compose names a project's volume or network, such as
// "env_01kat…_data".
func ScopedName(project, name string) string { return project + "_" + name }
