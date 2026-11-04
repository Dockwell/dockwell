// SPDX-License-Identifier: AGPL-3.0-only

// Command protoc-gen-dockwell-cli is the protoc plugin that writes the
// dockwell CLI's API commands from the schema: one command for each method
// of dockwell.v1, and create, update and delete for each kind of resource.
// make generate runs it through buf twice: for the commands, into
// internal/cli/apicmd, and with output=names for only their names, into
// internal/cli/apinames, which the agent build links. See
// internal/cli/cligen, docs/adr/0019-a-cli-generated-from-the-schema.md and
// docs/adr/0023-a-full-build-and-an-agent-build.md.
package main

import (
	"flag"
	"fmt"

	"google.golang.org/protobuf/compiler/protogen"

	"github.com/dockwell/dockwell/internal/cli/cligen"
)

func main() {
	var flags flag.FlagSet
	output := flags.String("output", "commands", "what to write: commands, or names for only the commands' names")
	protogen.Options{ParamFunc: flags.Set}.Run(func(p *protogen.Plugin) error {
		switch *output {
		case "commands":
			return cligen.Generate(p)
		case "names":
			return cligen.GenerateNames(p)
		}
		return fmt.Errorf("protoc-gen-dockwell-cli writes commands or names, not %q. Set output to one of them in buf.gen.yaml", *output)
	})
}
