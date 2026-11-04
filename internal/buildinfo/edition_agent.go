// SPDX-License-Identifier: AGPL-3.0-only

//go:build agent

package buildinfo

// edition is which build this is. A plain string, as in the full build.
var edition = string(Agent)
