// SPDX-License-Identifier: AGPL-3.0-only

// Command protoc-gen-dockwell-cli is the protoc plugin that writes the
// dockwell CLI's API commands from the schema: one command for each method
// of dockwell.v1, and create, update and delete for each kind of resource.
// make generate runs it through buf. See internal/cli/cligen and
// docs/adr/0019-a-cli-generated-from-the-schema.md.
package main

import (
	"google.golang.org/protobuf/compiler/protogen"

	"github.com/dockwell/dockwell/internal/cli/cligen"
)

func main() {
	protogen.Options{}.Run(cligen.Generate)
}
