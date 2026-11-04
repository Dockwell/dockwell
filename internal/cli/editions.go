// SPDX-License-Identifier: AGPL-3.0-only

package cli

// controlServerOnly are the commands that run beside the control plane, on
// the control server. Only the full build has them; the agent build has a
// stand-in for each that says where it is. A test of the full build checks
// that this names every command the agent build lacks, other than the API
// commands, which the agent build's unknownCommand covers.
var controlServerOnly = []string{"claim", "recover", "token"}
