// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package footprint

// ReleaseStartupPages releases nothing here: dockwell runs on Linux, and
// builds elsewhere only for development.
func ReleaseStartupPages() (before, after int64, err error) { return 0, 0, ErrUnsupported }
