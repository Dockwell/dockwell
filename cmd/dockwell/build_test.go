// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// The agent build, built with -tags agent, is what every server that joins
// a control plane runs. Each package it links costs every one of them idle
// memory, whether or not the agent calls it: package initialization maps
// most of a binary's code (docs/adr/0018-keep-the-budgets-in-rss.md). So it
// links only what an agent runs, and these lists say what that is
// (docs/adr/0023-a-full-build-and-an-agent-build.md).
//
// A new package on either list is a decision about every server's memory:
// measure it with make budgets, and say what it cost in the pull request.

// agentPackages are the packages of this module the agent build may link.
var agentPackages = []string{
	"cmd/dockwell",
	"gen/dockwell/agent/v1",
	"gen/dockwell/agent/v1/agentv1connect",
	"gen/dockwell/catalog/v1alpha1", // the catalog's format, which gen/dockwell/v1 imports
	"gen/dockwell/resource/v1alpha1",
	"gen/dockwell/v1",
	"internal/agent",
	"internal/agent/access", // who reaches the server, under the firewall's timer (ADR 0052)
	"internal/agent/collect",
	"internal/agent/desired", // what the server should run, kept sealed (ADR 0054)
	"internal/agent/enroll",
	"internal/agent/files",      // a server's files, for a person (ADR 0032)
	"internal/agent/hosttool",   // running the server's own tools, such as ufw and nft (ADR 0043, ADR 0052)
	"internal/agent/logs",       // reading logs, and keeping those of removed containers (ADR 0025)
	"internal/agent/nftables",   // Dockwell's own nftables table, written with nft (ADR 0052)
	"internal/agent/onepanel",   // reading what 1Panel keeps, for an import (ADR 0056)
	"internal/agent/sqlitefile", // reading 1Panel's SQLite records, read-only, without SQLite (ADR 0056)
	"internal/agent/terminal",   // a server's terminals, for a person (ADR 0037)
	"internal/agent/updates",    // a server's package updates, read with its own package manager (ADR 0043)
	"internal/backup",           // taking and restoring backups on this server (ADR 0033)
	"internal/backup/s3",        // sending them to S3-compatible storage, with no SDK
	"internal/backup/seal",      // sealing them before they leave
	"internal/buildinfo",
	"internal/cli",
	"internal/cli/apinames", // the API commands' names, to say they're in the full build
	"internal/cron",         // the schedule each scheduled task runs on (ADR 0053)
	"internal/database",     // reading a database in its own container (ADR 0038)
	"internal/datadir",
	"internal/delivery", // opens the values of a step's variables and secrets, sealed to the stream
	"internal/docker",   // the Docker Engine's API, for the Docker driver (ADR 0021)
	"internal/fault",    // what stops a rollout partway in a test, and kinds of error (ADR 0027)
	"internal/health",   // checking containers, and watching them after a deploy (ADR 0027)
	"internal/human",
	"internal/id",
	"internal/ingress", // what runs a server's routes on a proxy (ADR 0024)
	"internal/ingress/caddy",
	"internal/logging",
	"internal/mounts", // what's mounted, to keep away from a mount that stopped answering
	"internal/netif",  // which network interfaces are physical, as sysfs says
	"internal/pki",
	"internal/resource",
	"internal/resource/binding",   // a service's environment and label, from what it uses
	"internal/resource/container", // the Docker driver's kinds
	"internal/resource/firewall",  // firewall rules and the policy, on every server (ADR 0052)
	"internal/resource/network",
	"internal/resource/resourcepb",
	"internal/resource/route", // the ingress's kind (ADR 0024)
	"internal/resource/service",
	"internal/resource/variable", // the names and kinds a service's Validate checks what it uses by
	"internal/resource/volume",
	"internal/task", // scheduled tasks, which each agent runs on its own clock (ADR 0053)
	"internal/ticket",
	"internal/volumetool", // reading and writing a volume's bytes, in a throwaway container (ADR 0033)
}

// agentModules are the other modules the agent build may link packages of.
var agentModules = []string{
	"connectrpc.com/connect",                    // the agent's stream to the control plane
	"github.com/spf13/cobra",                    // the command line
	"github.com/spf13/pflag",                    // its flags
	"golang.org/x/sys",                          // system calls, for x/term
	"golang.org/x/term",                         // whether a person is at a terminal
	"google.golang.org/genproto/googleapis/api", // google.api annotations, which the schema's files import
	"google.golang.org/protobuf",                // the agent's messages
}

// controlOnly are packages the agent build must never link, and why, for a
// clearer message than "not on the list". The standard library has no list
// of its own, so its heaviest packages that only the control plane uses are
// named here.
var controlOnly = map[string]string{
	"github.com/dockwell/dockwell/internal/control":    "the control plane",
	"github.com/dockwell/dockwell/internal/store":      "the control plane's state, on SQLite or Postgres",
	"github.com/dockwell/dockwell/internal/auth":       "sign-in and passkeys",
	"github.com/dockwell/dockwell/internal/revision":   "the revision engine",
	"github.com/dockwell/dockwell/internal/panel":      "the panel's assets",
	"github.com/dockwell/dockwell/internal/cli/apicli": "the API commands",
	"github.com/dockwell/dockwell/internal/cli/apicmd": "the API commands",
	"github.com/dockwell/dockwell/internal/https":      "the panel's HTTPS and ACME",
	"github.com/dockwell/dockwell/internal/compose":    "Compose import and export",
	"modernc.org/sqlite":                               "SQLite",
	"github.com/jackc/pgx":                             "Postgres",
	"github.com/go-webauthn/webauthn":                  "passkeys",
	"connectrpc.com/vanguard":                          "the REST paths",
	"connectrpc.com/grpcreflect":                       "the API's reflection",
	"github.com/compose-spec/compose-go":               "Compose import and export",
	"database/sql":                                     "the control plane's state",
	"encoding/xml":                                     "passkeys' attestation",
}

type listed struct {
	ImportPath string
	Standard   bool
	Imports    []string
	Module     *struct{ Path string }
}

// goList lists the packages ./cmd/dockwell links for linux/arch, with tags.
func goList(t *testing.T, arch, tags string) []listed {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "-json", "-tags", tags, ".")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -tags %q for linux/%s: %v\n%s", tags, arch, err, stderr.String())
	}
	var pkgs []listed
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listed
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

// importers maps each package to the one that brought it into the build
// first, breadth first from the main package.
func importers(pkgs []listed) map[string]string {
	byPath := map[string]listed{}
	for _, p := range pkgs {
		byPath[p.ImportPath] = p
	}
	from := map[string]string{mainPackage: ""}
	queue := []string{mainPackage}
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		for _, next := range byPath[at].Imports {
			if _, seen := from[next]; !seen {
				from[next] = at
				queue = append(queue, next)
			}
		}
	}
	return from
}

// chain is how the main package comes to import path, such as
// "cmd/dockwell → internal/cli → internal/store".
func chain(from map[string]string, path string) []string {
	var steps []string
	for p := path; p != ""; p = from[p] {
		steps = append(steps, p)
	}
	slices.Reverse(steps)
	return steps
}

const (
	module      = "github.com/dockwell/dockwell"
	mainPackage = module + "/cmd/dockwell"
)

func within(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// refuse says why the agent build may not link p, or nothing when it may.
func refuse(p listed) string {
	for prefix, what := range controlOnly {
		if within(p.ImportPath, prefix) {
			return fmt.Sprintf("which is %s, and the agent build never needs it. Import it only from files with //go:build !agent", what)
		}
	}
	const add = ", and measure it with make budgets; if only the control plane needs it, import it only from files with //go:build !agent"
	switch {
	case p.Standard, p.Module == nil:
	case p.Module.Path == module:
		if !slices.Contains(agentPackages, strings.TrimPrefix(p.ImportPath, module+"/")) {
			return "which isn't on agentPackages. If the agent runs it, add it there, in cmd/dockwell/build_test.go" + add
		}
	default:
		if !slices.ContainsFunc(agentModules, func(m string) bool { return within(p.ImportPath, m) }) {
			return "of module " + p.Module.Path + ", which isn't on agentModules. If the agent needs it, add it there, in cmd/dockwell/build_test.go" + add
		}
	}
	return ""
}

func TestTheAgentBuildLinksOnlyTheAgent(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			pkgs := goList(t, arch, "agent")
			refused := map[string]string{}
			for _, p := range pkgs {
				if why := refuse(p); why != "" {
					refused[p.ImportPath] = why
				}
			}
			// Each refused package is reported where it enters the build,
			// not again for each package it brings with it.
			from := importers(pkgs)
			for _, p := range pkgs {
				why, ok := refused[p.ImportPath]
				if !ok {
					continue
				}
				steps := chain(from, p.ImportPath)
				if slices.ContainsFunc(steps[:len(steps)-1], func(s string) bool { _, ok := refused[s]; return ok }) {
					continue
				}
				brings := 0
				for _, q := range pkgs {
					if q.ImportPath != p.ImportPath && slices.Contains(chain(from, q.ImportPath), p.ImportPath) {
						brings++
					}
				}
				for i := range steps {
					steps[i] = strings.TrimPrefix(steps[i], module+"/")
				}
				t.Errorf("the agent build links %s, %s. It comes in through %s, and %d more packages come in through it. "+
					"Every server that joins a control plane runs the agent build, so each package it links costs all of them memory at idle "+
					"(docs/adr/0023-a-full-build-and-an-agent-build.md)",
					p.ImportPath, why, strings.Join(steps, " → "), brings)
			}
		})
	}
}

// The lists above would pass a build that linked nothing. The full build,
// built as it is, has what the agent build leaves out, so the build
// constraints can't have been turned around.
func TestTheFullBuildLinksTheControlPlane(t *testing.T) {
	var paths []string
	for _, p := range goList(t, "amd64", "") {
		paths = append(paths, p.ImportPath)
	}
	for _, want := range []string{module + "/internal/control", module + "/internal/store", module + "/internal/panel", module + "/internal/cli/apicmd"} {
		if !slices.Contains(paths, want) {
			t.Errorf("the full build doesn't link %s, so it has no control plane, panel or API commands. "+
				"Files that reach them carry //go:build !agent, and the agent build's stand-ins //go:build agent", want)
		}
	}
}

// Neither build reads another catalog's format: the converter runs where the catalog
// is built, as its own program, never in the control plane (ADR 0029).
func TestNeitherBuildLinksTheCatalogConverter(t *testing.T) {
	for _, tags := range []string{"", "agent"} {
		for _, p := range goList(t, "amd64", tags) {
			if strings.HasPrefix(p.ImportPath, module+"/internal/catalog/convert") {
				t.Errorf("the build with tags %q links %s, which reads other catalogs' formats. Only cmd/dockwell-catalog may", tags, p.ImportPath)
			}
		}
	}
}

// What a package allocates as the program starts costs every server more than its
// size: it lands among what lives as long as the agent does, and the heap never
// closes the gaps it leaves. Regular expressions with a counted repetition did
// that, each compiling to as many copies of what it repeats, and took about
// 0.85 MB of an idle agent (docs/adr/0060-what-an-idle-agent-keeps-in-memory.md).
// So a package of the agent's own may allocate only so much before main runs.
const (
	// initBudget is the most one package of this module may allocate as the agent
	// build starts, and initBudgetAll the most they may together, in bytes. The
	// largest was 163 KB and the sum 351 KB when this was set; before, 816 KB and
	// 1,334 KB.
	initBudget    = 256 << 10
	initBudgetAll = 512 << 10
)

func TestTheAgentBuildAllocatesLittleAsItStarts(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the agent build")
	}
	bin := t.TempDir() + "/dockwell-agent"
	build := exec.CommandContext(t.Context(), "go", "build", "-tags", "agent", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build -tags agent: %v\n%s", err, out)
	}
	run := exec.CommandContext(t.Context(), bin, "version")
	run.Env = append(os.Environ(), "GODEBUG=inittrace=1")
	var trace bytes.Buffer
	run.Stderr = &trace
	if err := run.Run(); err != nil {
		t.Fatalf("dockwell version: %v\n%s", err, trace.String())
	}
	// A line is "init <package> @<when> ms, <how long> ms clock, <bytes> bytes, <allocs> allocs".
	sum, seen := 0, 0
	for line := range strings.Lines(trace.String()) {
		f := strings.Fields(line)
		if len(f) < 9 || f[0] != "init" || f[8] != "bytes," {
			continue
		}
		seen++
		// Generated code registers its messages, which is the schema's size, not a
		// package's choice.
		if !within(f[1], module+"/internal") && !within(f[1], module+"/cmd") {
			continue
		}
		var n int
		if _, err := fmt.Sscan(f[7], &n); err != nil {
			t.Fatalf("can't read the bytes in %q: %v", line, err)
		}
		sum += n
		if n > initBudget {
			t.Errorf("%s allocates %d KB as the agent build starts, over the %d KB a package may. "+
				"Look at its package-level variables: a regular expression with a counted repetition, such as {0,254}, "+
				"compiles to that many copies, so check the length apart from the form, as resource.IsLabel does; "+
				"build a large table on first use",
				strings.TrimPrefix(f[1], module+"/"), n>>10, initBudget>>10)
		}
	}
	if seen == 0 {
		t.Fatalf("GODEBUG=inittrace=1 printed no line this test reads, so it checked nothing:\n%s", trace.String())
	}
	if sum > initBudgetAll {
		t.Errorf("the agent build's own packages allocate %d KB between them as it starts, over %d KB. "+
			"Run GODEBUG=inittrace=1 bin/dockwell-agent version to see which, and build what's large on first use", sum>>10, initBudgetAll>>10)
	}
	t.Logf("the agent build's own packages allocate %d KB as it starts", sum>>10)
}
