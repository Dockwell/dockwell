// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// StandaloneReferences enumerates every image the built-in frontend may use.
// The runtime policy denies any source this projection did not approve.
func StandaloneReferences(text []byte) ([]registry.Ref, error) {
	images, err := dockerfileSources(string(text), nil, true)
	if errors.Is(err, errUnresolvedImage) {
		return nil, errors.New("use explicit base image references in the Dockerfile")
	}
	if err != nil {
		return nil, err
	}
	if images.syntax != "" {
		return nil, errors.New("remove the syntax directive to use Docker's built-in Dockerfile frontend")
	}
	var refs []registry.Ref
	for _, image := range images.images {
		ref, err := registry.Parse(image)
		if err != nil {
			return nil, errors.New("the Dockerfile has an invalid base image reference")
		}
		if !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
		if len(refs) > 32 {
			return nil, errors.New("a build can review up to 32 base images")
		}
	}
	return refs, nil
}

// standaloneInstruction refuses unsupported operations at review. The builder
// independently denies remote sources and receives no entitlements or mounts.
func standaloneInstruction(ins string, fields []string) error {
	switch strings.ToUpper(fields[0]) {
	case "ADD":
		args := withoutFlags(fields[1:])
		if len(args) > 0 && strings.HasPrefix(args[0], "[") {
			// JSON arrays need no spaces between their arguments.
			var sources []string
			if start := strings.IndexByte(ins, '['); json.Unmarshal([]byte(ins[start:]), &sources) == nil {
				args = sources
			}
		}
		if len(args) < 2 {
			return nil // BuildKit reports malformed ADD instructions.
		}
		// Both shell and JSON ADD accept remote sources. The destination is last.
		for _, arg := range args[:len(args)-1] {
			arg = strings.TrimLeft(arg, `["' `)
			if strings.Contains(arg, "://") || strings.HasPrefix(arg, "git@") {
				return errors.New("use files from the build context in ADD")
			}
		}
	case "RUN":
		for _, flag := range fields[1:] {
			if !strings.HasPrefix(flag, "--") {
				break
			}
			key, value, _ := strings.Cut(flag, "=")
			value = strings.ReplaceAll(strings.ReplaceAll(value, `"`, ""), "'", "")
			switch key {
			case "--device", "--security":
				return errors.New("remove device and security entitlements from RUN")
			case "--network":
				if value == "host" {
					return errors.New("remove host networking from RUN")
				}
			case "--mount":
				for opt := range strings.SplitSeq(value, ",") {
					if kind, ok := strings.CutPrefix(opt, "type="); ok && (kind == "secret" || kind == "ssh") {
						return errors.New("remove secret and SSH mounts from RUN")
					}
				}
			}
		}
	}
	return nil
}

// StandalonePolicy converts each reviewed image to its native manifest. BuildKit
// applies the policy both at metadata resolution and when loading sources, even
// for inherited ONBUILD instructions. The frontend receives no entitlement.
func StandalonePolicy(refs []registry.Ref, bases []*v1.DockerRegistryDescriptor, platform string) (string, error) {
	type selector struct {
		Identifier string `json:"identifier"`
		Match      int    `json:"match_type,omitempty"`
	}
	type rule struct {
		Action   int       `json:"action"`
		Selector selector  `json:"selector"`
		Updates  *selector `json:"updates,omitempty"`
	}
	rules := []rule{
		{Action: 1, Selector: selector{Identifier: "docker-image://*"}},
		{Action: 1, Selector: selector{Identifier: "http*://*"}},
		{Action: 1, Selector: selector{Identifier: "git://*"}},
	}
	if len(refs) != len(bases) {
		return "", errors.New("the Dockerfile's reviewed images changed")
	}
	for i, ref := range refs {
		pin := bases[i]
		if ref.Name() != pin.GetRepository() || !registry.IsDigest(pin.GetManifestDigest()) || pin.GetPlatform() != platform {
			return "", errors.New("the Dockerfile's reviewed images changed")
		}
		if ref.Digest != "" && ref.Digest != pin.GetManifestDigest() && ref.Digest != pin.GetIndexDigest() {
			return "", errors.New("the Dockerfile's reviewed image digest changed")
		}
		repo := "docker-image://" + ref.Registry + "/" + ref.Repository
		pinned := repo + "@" + pin.GetManifestDigest()
		original := repo + ":" + ref.Reference()
		if ref.Digest != "" {
			original = repo + "@" + ref.Digest
			if ref.Tag != "" {
				// BuildKit can preserve the tag on metadata lookups with a digest.
				tagged := repo + ":" + ref.Tag + "@" + ref.Digest
				rules = append(rules, rule{Action: 2, Selector: selector{Identifier: tagged, Match: 1}, Updates: &selector{Identifier: pinned}})
			}
		}
		if original != pinned {
			rules = append(rules, rule{Action: 2, Selector: selector{Identifier: original, Match: 1}, Updates: &selector{Identifier: pinned}})
		}
		rules = append(rules, rule{Selector: selector{Identifier: pinned, Match: 1}})
	}
	data, err := json.Marshal(struct {
		Version int    `json:"version"`
		Rules   []rule `json:"rules"`
	}{1, rules})
	return string(data), err
}

// loadStandalone stages BuildKit's output on disk. Docker sees the archive
// only after a successful solve and after the cancel/publication boundary.
func (b *Builder) loadStandalone(ctx context.Context, req service.BuildRequest, id, root string, rep *report) (int, error, error) {
	f, err := os.CreateTemp(req.Standalone.Scratch, "build-output-*.tar")
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()
	if err := os.Remove(f.Name()); err != nil {
		return 0, nil, err
	}
	const maxOutput = 16 << 30
	out := &buildOutput{file: f, remaining: maxOutput}
	code, runErr := b.Client.RunAttached(ctx, id, docker.Streams{Stdout: out, Stderr: rep})
	if ctx.Err() != nil || code != 0 || runErr != nil {
		return code, errors.Join(runErr, ctx.Err()), nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return code, nil, err
	}
	err = req.Standalone.Publish(ctx, func() error {
		if err := b.checkPublication(root, maxOutput-out.remaining); err != nil {
			return err
		}
		return req.Standalone.StartPublication()
	}, func(write context.Context) error {
		return b.loadPublication(write, root, f)
	})
	return code, nil, err
}

type buildOutput struct {
	file      io.Writer
	remaining int64
}

func (o *buildOutput) Write(p []byte) (int, error) {
	if int64(len(p)) > o.remaining {
		return 0, errors.New("the build result exceeds 16 GiB")
	}
	n, err := o.file.Write(p)
	o.remaining -= int64(n)
	return n, err
}
