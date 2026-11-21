// SPDX-License-Identifier: AGPL-3.0-only

// Package dockerlog describes typed Docker logging settings.
package dockerlog

import (
	"errors"
	"strconv"
	"strings"

	"github.com/docker/go-units"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

var errLogging = errors.New("choose json-file or local with 1 MiB to 1 GiB per file and 1 to 100 files")

// ValidateLogging checks the supported drivers and finite limits.
func ValidateLogging(l *spec.DockerDaemonLogging) error {
	if l == nil || (l.Driver != "json-file" && l.Driver != "local") ||
		l.MaxSizeBytes < 1<<20 || l.MaxSizeBytes > 1<<30 || l.MaxFiles < 1 || l.MaxFiles > 100 {
		return errLogging
	}
	return nil
}

// ValidateContainerLogging checks explicit policy while preserving an omitted policy.
func ValidateContainerLogging(l *spec.ContainerLogging) error {
	if l == nil {
		return nil
	}
	switch l.Mode {
	case spec.ContainerLoggingMode_CONTAINER_LOGGING_MODE_INHERIT:
		if l.Limits == nil {
			return nil
		}
	case spec.ContainerLoggingMode_CONTAINER_LOGGING_MODE_LIMITS:
		return ValidateLogging(l.Limits)
	default:
	}
	return errors.New("choose Docker defaults or explicit log limits")
}

// Size parses a Docker log size into bytes. Invalid or unlimited values return zero.
func Size(value string) uint64 {
	// Moby's json-file and local drivers both interpret these sizes with SI units.
	n, err := units.FromHumanSize(strings.TrimSpace(value))
	if err != nil || n <= 0 {
		return 0
	}
	return uint64(n)
}

// Files parses Docker’s maximum number of log files.
func Files(value string) uint32 {
	n, _ := strconv.ParseUint(value, 10, 32)
	return uint32(n)
}

// ContainerLogConfig turns explicit limits into Docker create options.
func ContainerLogConfig(l *spec.ContainerLogging) *docker.LogConfig {
	if l.GetMode() != spec.ContainerLoggingMode_CONTAINER_LOGGING_MODE_LIMITS {
		return nil
	}
	limits := l.GetLimits()
	return &docker.LogConfig{Type: limits.GetDriver(), Config: map[string]string{
		"max-size": strconv.FormatUint(limits.GetMaxSizeBytes(), 10),
		"max-file": strconv.FormatUint(uint64(limits.GetMaxFiles()), 10),
	}}
}

// ObservedLogging restores reviewed policy from the container and its metadata.
func ObservedLogging(mode string, l *docker.LogConfig) *spec.ContainerLogging {
	switch mode {
	case "inherit":
		return &spec.ContainerLogging{Mode: spec.ContainerLoggingMode_CONTAINER_LOGGING_MODE_INHERIT}
	case "limits":
		if l == nil {
			return nil
		}
		return &spec.ContainerLogging{
			Mode:   spec.ContainerLoggingMode_CONTAINER_LOGGING_MODE_LIMITS,
			Limits: &spec.DockerDaemonLogging{Driver: l.Type, MaxSizeBytes: Size(l.Config["max-size"]), MaxFiles: Files(l.Config["max-file"])},
		}
	default:
		return nil
	}
}

// RotationState classifies the effective log limits.
func RotationState(l *docker.LogConfig) v1.DockerLogRotationState {
	if l == nil || l.Type == "" {
		return v1.DockerLogRotationState_DOCKER_LOG_ROTATION_STATE_UNKNOWN
	}
	switch l.Type {
	case docker.LogDriverJSONFile:
		if l.Config["max-size"] == "" || l.Config["max-size"] == "-1" {
			return v1.DockerLogRotationState_DOCKER_LOG_ROTATION_STATE_UNBOUNDED
		}
		if Size(l.Config["max-size"]) == 0 {
			return v1.DockerLogRotationState_DOCKER_LOG_ROTATION_STATE_UNKNOWN
		}
		return v1.DockerLogRotationState_DOCKER_LOG_ROTATION_STATE_BOUNDED
	case docker.LogDriverLocal:
		return v1.DockerLogRotationState_DOCKER_LOG_ROTATION_STATE_BOUNDED
	default:
		return v1.DockerLogRotationState_DOCKER_LOG_ROTATION_STATE_DRIVER_MANAGED
	}
}
