// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package volumetool

import (
	"archive/tar"
	"errors"
	"time"
)

// The tool runs in a Linux container. These let the module build elsewhere.

func readXattrs(string) (map[string]string, error) { return nil, nil }

func writeXattrs(string, map[string]string) error { return nil }

func makeNode(string, *tar.Header) error {
	return errors.New("devices and pipes unpack only on Linux")
}

func setTime(string, time.Time) error { return nil }
