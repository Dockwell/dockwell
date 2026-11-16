// SPDX-License-Identifier: AGPL-3.0-only

// Package bump moves Dockwell's catalog to new releases of its apps' images (ADR
// 0100): for each image, the newest version on its track that's aged past its wait,
// in two lanes, within the levels a policy sets and the next step beyond them, each
// checked for a second channel that says the version is the upstream's own and for
// the platforms it keeps, and written as exact edits of an entry's lines. It reads
// registries and GitHub, and writes nothing but patches: a job that holds no secret
// runs it, and another, which runs none of its code, opens the pull requests.
package bump

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"

	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/registry"
)

// PolicyDir is the folder of policies, at the catalog repository's top, beside
// entries/: one file for each entry that needs more than the defaults, as
// <id>.yaml. A server never reads it, so it's no part of the entry format, and
// changing it moves nothing.
const PolicyDir = "updates"

// Level is the largest step of a service's image that merges by itself.
type Level string

// The levels, from the most a bump may take by itself to nothing at all.
const (
	// LevelMinor takes minors and patches: the default for an app at 1.0 or later
	// on semantic versions.
	LevelMinor Level = "minor"
	// LevelPatch takes patches alone: the default for a database engine's image,
	// and for an app on 0.x or on calendar versions.
	LevelPatch Level = "patch"
	// LevelNone takes nothing by itself: every step is a person's.
	LevelNone Level = "none"
	// LevelStays never moves the image, as a database that follows what its app
	// pins, such as Plausible's ClickHouse.
	LevelStays Level = "stays"
)

// Policy is what an entry's file in updates/ says, by its Compose services.
type Policy struct {
	// Gives is the service whose tag gives the entry's version. An entry of one
	// image needs none.
	Gives string `yaml:"gives"`
	// Services are each service's own, by its name in the entry's Compose file.
	Services map[string]ServicePolicy `yaml:"services"`
}

// ServicePolicy is what a policy says of one service's image. Each field left out
// takes its default from code.
type ServicePolicy struct {
	Level Level `yaml:"level"`
	// Rebuilds says the repository pushes the same tag again by design, as Docker
	// Official Images do, which is its default for them.
	Rebuilds *bool `yaml:"rebuilds"`
	// SignedBy is who signs the image, which a person confirms once.
	SignedBy *Signer `yaml:"signedBy"`
	// MajorsInOrder says the app can't skip a major, as Nextcloud can't: its next
	// major is offered only where it's the one right after.
	MajorsInOrder bool `yaml:"majorsInOrder"`
	// Hold are versions to skip, each with why. Only a person changes it.
	Hold []Hold `yaml:"hold"`
}

// Signer is who signs an image: a cosign keyless issuer and subject, or a GitHub
// attestation's repository and workflow.
type Signer struct {
	Issuer     string `yaml:"issuer"`
	Subject    string `yaml:"subject"`
	Repository string `yaml:"repository"`
	Workflow   string `yaml:"workflow"`
}

func (s *Signer) String() string {
	if s.Repository != "" {
		return s.Repository + "'s workflow " + s.Workflow
	}
	return s.Subject + " through " + s.Issuer
}

// Hold is a version a bump skips, and why.
type Hold struct {
	Version string `yaml:"version"`
	Why     string `yaml:"why"`
}

// ReadPolicy reads an entry's policy from a catalog checkout's updates/ folder,
// strictly: a field it doesn't know is refused, as it would otherwise do nothing
// without a word. No file is the defaults alone.
func ReadPolicy(checkout, id string) (*Policy, error) {
	raw, err := os.ReadFile(filepath.Join(checkout, PolicyDir, id+".yaml")) //nolint:gosec // a file of the checkout the person running it names
	if errors.Is(err, os.ErrNotExist) {
		return &Policy{}, nil
	}
	if err != nil {
		return nil, err
	}
	return ParsePolicy(raw)
}

// ParsePolicy reads a policy from YAML, strictly, and checks what it says alone.
func ParsePolicy(raw []byte) (*Policy, error) {
	p := &Policy{}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(p); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("reading its YAML: %w", err)
	}
	for name, s := range p.Services {
		switch s.Level {
		case "", LevelMinor, LevelPatch, LevelNone, LevelStays:
		default:
			return nil, fmt.Errorf("the service %s's level is %q. Use patch, minor, none or stays: a major never merges by itself", name, s.Level)
		}
		if g := s.SignedBy; g != nil {
			keyless, attested := g.Issuer != "" && g.Subject != "", g.Repository != "" && g.Workflow != ""
			if keyless == attested || keyless && (g.Repository != "" || g.Workflow != "") || attested && (g.Issuer != "" || g.Subject != "") {
				return nil, fmt.Errorf("the service %s's signedBy names neither an issuer and a subject nor a repository and a workflow alone. Give one pair", name)
			}
		}
		for _, h := range s.Hold {
			if strings.TrimSpace(h.Version) == "" || strings.TrimSpace(h.Why) == "" {
				return nil, fmt.Errorf("the service %s holds a version without saying which or why. Give each its version and why", name)
			}
		}
	}
	return p, nil
}

// Check says what a policy names that its entry doesn't have.
func (p *Policy) Check(services []string) error {
	if p.Gives != "" && !slices.Contains(services, p.Gives) {
		return fmt.Errorf("it says %s gives the version, and the entry has no such service. Name one of %s", p.Gives, strings.Join(services, ", "))
	}
	for name := range p.Services {
		if !slices.Contains(services, name) {
			return fmt.Errorf("it names the service %s, which the entry doesn't have. Name one of %s", name, strings.Join(services, ", "))
		}
	}
	return nil
}

// Effective is what a policy comes to for one service's image, its defaults from
// code filled in.
type Effective struct {
	Level         Level
	Rebuilds      bool
	SignedBy      *Signer
	MajorsInOrder bool
	Hold          []Hold
	// Engine is the database engine the image runs, if it runs one.
	Engine *database.Engine
	// Official says it's a Docker Official Image, built from
	// docker-library/official-images.
	Official bool
}

// For is what the policy comes to for a service whose image is ref.
func (p *Policy) For(service string, ref registry.Ref) Effective {
	s := p.Services[service]
	e := Effective{
		Level:         s.Level,
		SignedBy:      s.SignedBy,
		MajorsInOrder: s.MajorsInOrder,
		Hold:          s.Hold,
		Engine:        database.Recognise(ref.Name() + ":" + ref.Tag),
		Official:      Official(ref),
	}
	if e.Level == "" {
		e.Level = defaultLevel(e.Engine, ref.Tag)
	}
	e.Rebuilds = e.Official
	if s.Rebuilds != nil {
		e.Rebuilds = *s.Rebuilds
	}
	return e
}

// defaultLevel is a level for an image no policy names: patch for a database
// engine's, wherever it appears, and for an app on 0.x or on calendar versions,
// such as 2026.11; minor for an app at 1.0 or later on semantic versions.
func defaultLevel(engine *database.Engine, tag string) Level {
	if engine != nil {
		return LevelPatch
	}
	major, ok := registry.MajorOf(tag)
	if !ok || major == 0 || major >= 2000 {
		return LevelPatch
	}
	return LevelMinor
}

// Official says an image is a Docker Official Image: Docker Hub's library/.
func Official(ref registry.Ref) bool {
	return ref.Registry == registry.Hub && strings.HasPrefix(ref.Repository, "library/")
}

// Held says why version is held, or "".
func (e Effective) Held(version string) string {
	for _, h := range e.Hold {
		if strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(h.Version), "v"), version) {
			return h.Why
		}
	}
	return ""
}

// covers says a level takes a step of kind by itself.
func (l Level) covers(k registry.Kind) bool {
	switch l {
	case LevelMinor:
		return k <= registry.KindMinor
	case LevelPatch:
		return k <= registry.KindPatch
	case LevelNone, LevelStays:
	}
	return false
}
