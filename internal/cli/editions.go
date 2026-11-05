// SPDX-License-Identifier: AGPL-3.0-only

package cli

// controlServerOnly are the commands that run beside the control plane, on
// the control server. Only the full build has them; the agent build has a
// stand-in for each that says where it is. A test of the full build checks
// that this names every command the agent build lacks, other than the API
// commands, which apiAccessCommands and internal/cli/apinames name.
var controlServerOnly = []string{"claim", "recover", "rotate-key", "token", "__read-compose"}

// apiAccessCommands sign the CLI in to a control plane's API. Only the full
// build has them, with the API commands, whose names internal/cli/apinames
// has, generated from the schema. The agent build says each of them is in
// the full build, and takes any other command it lacks for a mistake. A
// test of the full build checks that the two name its API commands.
var apiAccessCommands = []string{"login", "logout"}
