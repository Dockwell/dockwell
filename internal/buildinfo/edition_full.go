// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package buildinfo

// edition is which build this is. A plain string, so that the install tests
// can link it empty, to stand in for a release made before there were two
// builds, which reported none.
var edition = string(Full)
