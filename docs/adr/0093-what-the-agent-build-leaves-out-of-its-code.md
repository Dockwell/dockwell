# 0093. What the agent build leaves out of its code

- Status: Accepted. Amends [ADR 0023](0023-a-full-build-and-an-agent-build.md), where it says only `internal/cli` and `internal/buildinfo` have build tags.
- Date: 2026-11-13

## Context

[ADR 0060](0060-what-an-idle-agent-keeps-in-memory.md) brought an idle agent to 27.4 MB, under the 30 MB warning line of `scripts/budgets.sh`. Three days later main's agent read 31.2 MB on a CI runner, and 32.2 MB on the machine with 16 CPUs that measured this (#622). Nothing large had landed: between 9b20de7 and fb6813d the heap at idle grew about 80 KB, and the binary's symbols 77 KB. Each feature since ADR 0060 added a little code, about 1.8 MB in all, and nearly all of an agent's binary is resident ([ADR 0018](0018-keep-the-budgets-in-rss.md)), so its pages grew as its code did.

Three things in the agent build's code were there for no agent's sake.

- **Every exported method of every type.** `text/template` finds a method by a name it reads as it runs, so a binary that links it keeps every exported method it has, in case one is called. Cobra 1.10 writes its help without a template, but `SetUsageTemplate`, which gave Dockwell's help its own headings, brought one in. That was 3.0 MB of the agent build's 21.4 MB of code and tables, and most of what [ADR 0023](0023-a-full-build-and-an-agent-build.md) measured the command line at.
- **The public API's schema.** The agent's protocol reaches 21 of the 34 files of `proto/dockwell/v1`, and Go links a generated package whole: each file's descriptors, types and coders, which register as the program starts. ADR 0060 left the rest for a change to how the two builds are told apart.
- **Expressions compiled as the program starts.** 62 package-level regular expressions in packages the agent build links compiled before `main` ran, though most serve a check that runs only when a change does.

## Decision

- **Nothing the agent build links finds a method by name.** The usage layout is a function, `writeUsage` in `internal/cli`, that writes what the template wrote, and a test holds every command's help in both builds to the template it replaced.
  - `cmd/dockwell/build_test.go` builds the agent build with the linker's account of what it kept, `-ldflags=-dumpdep`, and fails on a function that finds a method by a name it learns as it runs, with what reached it.
  - `text/template` and `html/template` are the full build's. A page the agent serves, such as a sign-in's end state ([ADR 0087](0087-private-routes.md)), is written with `html.EscapeString`.
  - reflect's `MethodByName` with a constant name is fine: the linker keeps only that method.
- **The public API's files the agent's protocol doesn't reach are the full build's.** `scripts/agent-api.sh`, which `make generate` runs, follows the imports, public ones too, of `proto/dockwell/agent/v1` and `proto/dockwell/resource/v1alpha1`, the schemas the agent build links, into `proto/dockwell/v1`. Each generated file outside them gets `//go:build !agent || fullapi`. A new file of the public API is the full build's until the agent's protocol imports it, and then the next `make generate` drops the line.
- **The agent build's tests build with `fullapi` as well.** The tests of two packages the agent build links start a control plane to join, and the control plane needs every file. So `make test`, the vet and golangci-lint steps of `make lint`, and CI build the agent build's tests with `-tags agent,fullapi`. `fullapi` puts the files back and changes nothing else. `go build` or `go vet` with `-tags agent` alone fails outside `cmd/dockwell`, as the control plane's packages need the files, so agent-tagged work there takes `-tags agent,fullapi`. The binary is built with `-tags agent` alone, and `build_test.go` checks what it links.
  - This amends ADR 0023: outside `internal/cli` and `internal/buildinfo`, the public API's generated files carry a build tag too, and only `scripts/agent-api.sh` writes it.
- **A package-level regular expression in a package the agent build links compiles on first use.** `lazyre.Regexp(expr)` returns a function that compiles it the first time it's called, with `sync.OnceValue`. A broken expression would panic on its first use in a running agent, not as it starts, so a test of `internal/lazyre` reads every call in the module's source and compiles its expression: a string literal, or literals and the package's string constants joined with `+`.

## Consequences

Measured as `scripts/budgets.sh` measures: joined to a control plane, sampled after 180 s, each from its own binary, on a machine with 16 CPUs. Each figure is the median of 5 runs, side by side with the build before it. Where two builds' places were swapped, the second figure is that run.

| Change | Code and tables of the agent build | An idle agent |
| --- | ---: | ---: |
| Expressions on first use | 22.3 MB, the same | 32.0 against 32.3 MB, within the noise. What the agent's packages allocate as it starts fell from 1,958 KB to 1,516 KB. |
| The public API as far as the protocol reaches | 22.3 to 21.4 MB | 30.9 against 32.6 MB, and 31.2 against 32.2 MB swapped |
| Help without a template | 21.4 to 18.4 MB | 28.3 against 30.5 MB, in runs of 28.0 to 28.8 against 30.2 to 31.8 MB |
| All three, against main at 9b1faf251 | 22.4 to 18.4 MB | 29.0 against 32.2 MB, and 28.5 against 32.2 MB swapped |
| All three, against main at 3609e1c9d | 22.6 to 18.6 MB | 28.6 against 32.4 MB, and 28.5 against 32.1 MB swapped |

- **An idle agent reads 3.2 to 3.8 MB lower,** over four runs against two of main's commits. Main's read 31.2 MB on a CI runner, so this build should read about 28 MB there, 2 MB under the warning line. The line stays at 30 MB.
- **A control plane with its own agent reads 4 to 7 MB lower:** 73.2 against 79.9 MB and 75.6 against 79.6 MB swapped at 9b1faf251, and 76.2 against 81.2 MB and 75.3 against 80.5 MB swapped at 3609e1c9d. The full build drops the template's methods too: its code and tables are 10.3 MB smaller, 71.1 to 60.8 MB at 9b1faf251.
- **What's left of the command line is small.** Cobra and pflag are 0.08 MB of the agent build's code, and `text/template` 0.02 MB that cobra still names. A command line without cobra is no longer worth its change.
- **A new message the agent sends from the public API** comes into the agent build when its file is imported by the agent's protocol, which is the schema saying so. A file the agent build needs but doesn't reach fails to compile under `-tags agent`, not at run time.

### Not done

- **`GOGC=25` for an agent on its own.** It reads 1.2 MB lower again, 29.8 against 31.0 MB. It also doubles how often an idle agent collects, to about every 7 s, and costs CPU under a transfer or a flood of logs. That is a choice of CPU against memory, as ADR 0060's `GOGC=50` was, and the agent is under the line without it.
- **A cap on the CPUs the agent runs on,** for the reasons ADR 0060 gives.
