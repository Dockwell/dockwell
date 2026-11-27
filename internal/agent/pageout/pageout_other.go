// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package pageout

// Executable is a no-op on systems without Linux MADV_PAGEOUT.
func Executable() {}
