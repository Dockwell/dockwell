// SPDX-License-Identifier: AGPL-3.0-only

package bump

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/dockwell/dockwell/internal/registry"
)

// A bump is proven by upgrading to it (ADR 0100, slice 2): from the release before,
// as the pull request's base has it, and from the first release of the current
// major, as an install that waited months would. Both come from the catalog's own
// history, so nothing keeps a hand-written list of versions.

// Proven is a release of an entry an upgrade is proven from, as a commit of the
// catalog has it.
type Proven struct {
	Commit  string `json:"commit"`
	Version string `json:"version"`
}

// Earlier are the releases an upgrade to an entry is proven from.
type Earlier struct {
	// Before is the release before: the entry as base has it, where base is given
	// and its file differs, else the newest commit before at whose version differs.
	// Nil for an entry that's new.
	Before *Proven `json:"before,omitempty"`
	// First is the first release of the current major, where it's neither the
	// current version nor Before's.
	First *Proven `json:"first,omitempty"`
}

// EarlierReleases reads, from a git checkout of a catalog with its history, the
// releases an upgrade to entries/<id>.yaml at the commit at is proven from. base,
// where given, is the pull request's base.
func EarlierReleases(ctx context.Context, dir, id, at, base string) (*Earlier, error) {
	file := path.Join("entries", id+".yaml")
	now, ok, err := showFile(ctx, dir, at, file)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s has no %s", at, file)
	}
	current := versionOf(now)
	if current == "" {
		return nil, fmt.Errorf("%s at %s says no version", file, at)
	}
	out := &Earlier{}
	if base != "" {
		was, ok, err := showFile(ctx, dir, base, file)
		if err != nil {
			return nil, err
		}
		if ok && !bytes.Equal(was, now) {
			commit, err := revParse(ctx, dir, base)
			if err != nil {
				return nil, err
			}
			out.Before = &Proven{Commit: commit, Version: versionOf(was)}
		}
	}
	// Oldest first: every commit before at that touched the file.
	raw, err := git(ctx, dir, "log", "--format=%H", "--reverse", at, "--", file)
	if err != nil {
		return nil, err
	}
	head, err := revParse(ctx, dir, at)
	if err != nil {
		return nil, err
	}
	var history []Proven
	for c := range strings.FieldsSeq(string(raw)) {
		if c == head {
			continue
		}
		text, ok, err := showFile(ctx, dir, c, file)
		if err != nil {
			return nil, err
		}
		if ok {
			history = append(history, Proven{Commit: c, Version: versionOf(text)})
		}
	}
	if base == "" {
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].Version != "" && history[i].Version != current {
				out.Before = &history[i]
				break
			}
		}
	}
	major, ok := registry.MajorOf(current)
	if !ok {
		return out, nil
	}
	for i := range history {
		r := history[i]
		if m, ok := registry.MajorOf(r.Version); !ok || m != major {
			continue
		}
		if r.Version != current && (out.Before == nil || r.Version != out.Before.Version) {
			out.First = &r
		}
		break
	}
	return out, nil
}

// versionOf reads an entry's version, or "" where it says none.
func versionOf(text []byte) string {
	var e struct {
		Version string `yaml:"version"`
	}
	if yaml.Unmarshal(text, &e) != nil {
		return ""
	}
	return strings.TrimPrefix(e.Version, "v")
}

// showFile reads a file at a commit, and says false where the commit hasn't it.
func showFile(ctx context.Context, dir, commit, file string) ([]byte, bool, error) {
	if _, err := git(ctx, dir, "cat-file", "-e", commit+":"+file); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, false, nil
		}
		return nil, false, err
	}
	out, err := git(ctx, dir, "show", commit+":"+file)
	return out, err == nil, err
}

func revParse(ctx context.Context, dir, ref string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--verify", ref+"^{commit}")
	return strings.TrimSpace(string(out)), err
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // git, with what the person running it names
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
		}
		return nil, err
	}
	return out, nil
}
