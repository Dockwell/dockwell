// SPDX-License-Identifier: AGPL-3.0-only

// Package scripts embeds the install script, for the control plane to upgrade
// agents with (ADR 0094). The release ships the same file unchanged, so the bytes
// a control plane carries are its release's own. Only the full build links it.
package scripts

import _ "embed"

// Install is install.sh, as the release ships it.
//
//go:embed install.sh
var Install []byte
