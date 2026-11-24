// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

func (s *sshSettings) validateStagedSSH(ctx context.Context, body string) ([]byte, error) {
	r := s.reader
	main, err := readSSHFile(r.rootPath(sshdConfig), maxSSHKeysFile)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(r.rootPath("/etc/ssh"), ".dockwell-validate-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	dropIn := filepath.Join(dir, dockwellDropIn)
	if body != "" {
		if err := os.WriteFile(dropIn, []byte(body), 0o600); err != nil {
			return nil, err
		}
	}
	main, err = r.stagedSSHMain(main, dropIn, body != "")
	if err != nil {
		return nil, err
	}
	config := filepath.Join(dir, "sshd_config")
	if err := os.WriteFile(config, main, 0o600); err != nil {
		return nil, err
	}
	if _, err := r.sshd(ctx, "-t", "-f", config); err != nil {
		return nil, fmt.Errorf("sshd rejected the staged drop-in: %w", err)
	}
	return r.sshd(ctx, "-T", "-f", config)
}

// Rewrite the main file's Include of our drop-in in place, preserving the
// original order and leaving all the other files at their usual paths.
func (r *sshReader) stagedSSHMain(main []byte, dropIn string, includeCandidate bool) ([]byte, error) {
	found := false
	var out strings.Builder
	for line := range strings.SplitSeq(string(main), "\n") {
		key, value := sshdLine(line)
		if key != "include" {
			out.WriteString(line + "\n")
			continue
		}
		for _, arg := range strings.Fields(strings.SplitN(value, "#", 2)[0]) {
			pattern := strings.Trim(arg, `"`)
			if !path.IsAbs(pattern) {
				pattern = path.Join("/etc/ssh", pattern)
			}
			if !appliesToDockwell(pattern) {
				out.WriteString("Include " + arg + "\n")
				continue
			}
			found = true
			files, err := fs.Glob(r.root, strings.TrimPrefix(pattern, "/"))
			if err != nil {
				return nil, err
			}
			name := strings.TrimPrefix(sshDropInPath, "/")
			if !slices.Contains(files, name) {
				files = append(files, name)
				slices.Sort(files)
			}
			for _, file := range files {
				resolved := r.rootPath("/" + file)
				if file == name {
					if !includeCandidate {
						continue
					}
					resolved = dropIn
				}
				if strings.ContainsAny(resolved, "\"\\\r\n") {
					return nil, errors.New("the SSH include path can't be staged. Use an ordinary path before changing SSH settings")
				}
				fmt.Fprintf(&out, "Include \"%s\"\n", resolved)
			}
		}
	}
	if !found {
		return nil, errors.New("sshd_config has no include for the Dockwell SSH file. Include /etc/ssh/sshd_config.d/*.conf")
	}
	return []byte(out.String()), nil
}
