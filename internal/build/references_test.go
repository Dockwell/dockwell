// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"errors"
	"slices"
	"testing"
)

// A build with logins reads what its Dockerfile pulls as BuildKit would: every
// FROM, COPY and ADD --from and RUN --mount from=, past its own stages, with build
// arguments put in, and nothing a heredoc or a comment holds. (Security review of
// #591, H1.)
func TestReadingWhatADockerfilePulls(t *testing.T) {
	for what, c := range map[string]struct {
		dockerfile string
		args       map[string]string
		want       []string
		syntax     string
	}{
		"one base": {dockerfile: "FROM ghcr.io/acme/base:1.0\nRUN make\n", want: []string{"ghcr.io/acme/base:1.0"}},
		"stages and scratch": {
			dockerfile: "FROM --platform=$BUILDPLATFORM golang:1.24 AS build\nRUN go build\nFROM build AS test\nFROM scratch\nCOPY --from=build /app /app\nCOPY --from=0 /x /x\n",
			args:       map[string]string{"BUILDPLATFORM": "linux/amd64"},
			want:       []string{"golang:1.24"},
		},
		"copy, add and mount from images": {
			dockerfile: "FROM alpine:3\nCOPY --chown=1:1 --from=ghcr.io/acme/billing:2 / /loot\nadd --from=quay.io/acme/x:1 /a /b\nRUN --mount=type=bind,from=registry.example.com/tools:1,target=/t --mount=type=cache,target=/c true\n",
			want:       []string{"alpine:3", "ghcr.io/acme/billing:2", "quay.io/acme/x:1", "registry.example.com/tools:1"},
		},
		"lines continued, and a comment in one": {
			dockerfile: "FROM alpine:3\nCOPY \\\n# a comment\n  --from=ghcr.io/acme/billing:2 \\\n  / /loot\n",
			want:       []string{"alpine:3", "ghcr.io/acme/billing:2"},
		},
		"the escape directive": {
			dockerfile: "# escape=`\nFROM mcr.microsoft.com/windows/servercore:ltsc2022\nCOPY `\n  --from=ghcr.io/acme/billing:2 / /loot\n",
			want:       []string{"mcr.microsoft.com/windows/servercore:ltsc2022", "ghcr.io/acme/billing:2"},
		},
		"a heredoc holds no instructions": {
			dockerfile: "FROM alpine:3\nRUN <<EOF\nFROM ghcr.io/acme/not-pulled:1\nEOF\nCOPY <<-'END' /x\n\tFROM ghcr.io/acme/not-pulled:2\n\tEND\nCOPY --from=ghcr.io/acme/billing:2 / /loot\n",
			want:       []string{"alpine:3", "ghcr.io/acme/billing:2"},
		},
		"build arguments and their defaults": {
			dockerfile: "ARG REG=ghcr.io\nARG TAG=1\nFROM ${REG}/acme/base:$TAG\nARG TOOLS=quay.io/acme/tools:1\nCOPY --from=${TOOLS} / /t\n",
			args:       map[string]string{"TAG": "2"},
			want:       []string{"ghcr.io/acme/base:2", "quay.io/acme/tools:1"},
		},
		"a syntax directive": {
			dockerfile: "# syntax=docker/dockerfile:1.7\n# check=skip=all\nFROM alpine:3\n# syntax=frontend.example.com/late:1\n",
			want:       []string{"alpine:3"},
			syntax:     "docker/dockerfile:1.7",
		},
	} {
		got, err := readDockerfileImages(c.dockerfile, c.args)
		if err != nil {
			t.Errorf("%s: %v", what, err)
			continue
		}
		if !slices.Equal(got.images, c.want) || got.syntax != c.syntax {
			t.Errorf("%s: pulls %q with %q, want %q with %q", what, got.images, got.syntax, c.want, c.syntax)
		}
	}
}

// An image named by a build argument the build doesn't give says nothing of what it
// pulls, so it's refused, not passed over.
func TestADockerfileImageNoArgumentNames(t *testing.T) {
	for _, dockerfile := range []string{
		"ARG BASE\nFROM $BASE\n",
		"FROM alpine:3\nCOPY --from=${FROM} / /x\n",
		"FROM alpine:3\nRUN --mount=from=$TOOLS,target=/t true\n",
	} {
		if _, err := readDockerfileImages(dockerfile, nil); !errors.Is(err, errUnresolvedImage) {
			t.Errorf("%q = %v", dockerfile, err)
		}
	}
}

// Docker's own Dockerfile frontend, on Docker Hub, is the one a build with logins
// takes; any other, wherever it is, could ask for any image.
func TestDockersOwnFrontend(t *testing.T) {
	for syntax, want := range map[string]bool{
		"":                                       true,
		"docker/dockerfile:1":                    true,
		"docker.io/docker/dockerfile:1.7-labs":   true,
		"docker/dockerfile@sha256:" + sixtyFour0: true,
		"acme/dockerfile:1":                      false,
		"ghcr.io/docker/dockerfile:1":            false,
		"frontend.example.com/docker/dockerfile": false,
		"localhost:5000/docker/dockerfile:1":     false,
	} {
		if got := officialFrontend(syntax); got != want {
			t.Errorf("officialFrontend(%q) = %v", syntax, got)
		}
	}
}

const sixtyFour0 = "0000000000000000000000000000000000000000000000000000000000000000"

// The frontend a Dockerfile names is read in every form BuildKit's Dockerfile
// frontend takes one (parser.DetectSyntax in v0.34.1), so none slips past a build
// with logins. (Security review of #591, M1.)
func TestTheFrontendADockerfileNames(t *testing.T) {
	for dockerfile, want := range map[string]string{
		"# syntax=frontend.example.com/fe:1\nFROM alpine\n":                  "frontend.example.com/fe:1",
		"#SYNTAX = frontend.example.com/fe:1 --some option\nFROM alpine\n":   "frontend.example.com/fe:1",
		"\uFEFF# syntax=frontend.example.com/fe:1\nFROM alpine\n":            "frontend.example.com/fe:1",
		"#!/usr/bin/env buildctl\n# syntax=frontend.example.com/fe:1\n":      "frontend.example.com/fe:1",
		"# check=skip=all\n# escape=`\n# syntax=frontend.example.com/fe:1\n": "frontend.example.com/fe:1",
		"// syntax=frontend.example.com/fe:1\nFROM alpine\n":                 "frontend.example.com/fe:1",
		`{"syntax": "frontend.example.com/fe:1", "from": "alpine"}`:          "frontend.example.com/fe:1",
		"# syntax=docker/dockerfile:1\r\nFROM alpine\r\n":                    "docker/dockerfile:1",
		// BuildKit stops at a directive it doesn't know, at a line that isn't one,
		// and at the second of one.
		"# hello=there\n# syntax=frontend.example.com/fe:1\n":                "",
		"FROM alpine\n# syntax=frontend.example.com/fe:1\n":                  "",
		"\n# syntax=frontend.example.com/fe:1\n":                             "",
		"  # syntax=frontend.example.com/fe:1\n":                             "",
		"# syntax=docker/dockerfile:1\n# syntax=frontend.example.com/fe:1\n": "docker/dockerfile:1",
	} {
		got, err := readDockerfileImages(dockerfile, nil)
		if err != nil || got.syntax != want {
			t.Errorf("%q names %q, want %q (%v)", dockerfile, got.syntax, want, err)
		}
	}
}
