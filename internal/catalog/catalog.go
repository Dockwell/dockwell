// SPDX-License-Identifier: AGPL-3.0-only

// Package catalog holds the apps Dockwell can install: entries in the format ADR
// 0029 sets out, which it reads and fills in for an install. For now the first
// set is built in, written here and so free of any other catalog's terms, and
// Dockwell/catalog's signed index takes over from Load when it exists. See
// docs/adr/0029-a-catalog-format-fed-from-other-catalogs.md.
package catalog

import (
	"cmp"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net/mail"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"go.yaml.in/yaml/v4"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

//go:embed entries/*.yaml
var entries embed.FS

// Catalog is a set of entries, sorted by name.
type Catalog struct {
	entries []*catalogv1.Entry
}

// builtIn is what ships in the binary, read once.
var builtIn = sync.OnceValues(func() (*Catalog, error) { return load(entries) })

// Load returns the built-in catalog. commit is the build's, which an entry built
// in takes as the commit it came from.
func Load(commit string) (*Catalog, error) {
	c, err := builtIn()
	if err != nil {
		return nil, err
	}
	out := &Catalog{entries: make([]*catalogv1.Entry, len(c.entries))}
	for i, e := range c.entries {
		e = proto.CloneOf(e)
		e.GetSource().Commit = cmp.Or(e.GetSource().GetCommit(), commit)
		out.entries[i] = e
	}
	return out, nil
}

// load reads every entry in a directory of them.
func load(dir fs.FS) (*Catalog, error) {
	files, err := fs.Glob(dir, "entries/*.yaml")
	if err != nil {
		return nil, err
	}
	c := &Catalog{}
	for _, file := range files {
		raw, err := fs.ReadFile(dir, file)
		if err != nil {
			return nil, err
		}
		e, err := Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("the catalog entry %s: %w", file, err)
		}
		if want := strings.TrimSuffix(path.Base(file), ".yaml"); e.GetId() != want {
			return nil, fmt.Errorf("the catalog entry %s has the ID %s. Name the file %s.yaml", file, e.GetId(), e.GetId())
		}
		c.entries = append(c.entries, e)
	}
	slices.SortFunc(c.entries, func(a, b *catalogv1.Entry) int { return strings.Compare(a.GetName(), b.GetName()) })
	return c, nil
}

// Parse reads one entry from YAML, in the schema's own field names, and checks
// it. A field the schema doesn't have is refused, as it would otherwise do
// nothing without a word.
func Parse(raw []byte) (*catalogv1.Entry, error) {
	var tree any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("reading its YAML: %w", err)
	}
	asJSON, err := json.Marshal(tree)
	if err != nil {
		return nil, fmt.Errorf("reading its YAML: %w", err)
	}
	e := &catalogv1.Entry{}
	if err := protojson.Unmarshal(asJSON, e); err != nil {
		return nil, fmt.Errorf("reading its fields: %w", err)
	}
	return e, Check(e)
}

// All returns every entry, sorted by name. They're the catalog's: read them, and
// change none.
func (c *Catalog) All() []*catalogv1.Entry { return c.entries }

// Entry finds one by its ID.
func (c *Catalog) Entry(id string) (*catalogv1.Entry, bool) {
	i := slices.IndexFunc(c.entries, func(e *catalogv1.Entry) bool { return e.GetId() == id })
	if i < 0 {
		return nil, false
	}
	return c.entries[i], true
}

// A digest is what pins an image, as the PRD requires.
var pinned = regexp.MustCompile(`^\s*image:\s*\S+@sha256:[0-9a-f]{64}\s*$`)

// inputName is the name a Compose file's ${NAME} takes.
var inputName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Check says what's wrong with an entry, as the catalog's CI would refuse it: its
// ID, what it says of where it came from, its inputs, and an image not pinned by
// digest.
func Check(e *catalogv1.Entry) error {
	if err := resource.CheckLabel("catalog entry", e.GetId(), "postgres"); err != nil {
		return err
	}
	for field, value := range map[string]string{
		"name": e.GetName(), "summary": e.GetSummary(), "category": e.GetCategory(), "version": e.GetVersion(),
		"license": e.GetLicense(), "source's repository": e.GetSource().GetRepository(), "compose": e.GetCompose(),
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("it has no %s. Every entry says it", field)
		}
	}
	if e.GetRelease() == 0 {
		return errors.New("it has no release. Start at 1, and go up with each change to the entry")
	}
	images := 0
	for line := range strings.Lines(e.GetCompose()) {
		if strings.HasPrefix(strings.TrimSpace(line), "image:") {
			images++
			if !pinned.MatchString(line) {
				return fmt.Errorf("%s isn't pinned by digest. Add its @sha256 digest", strings.TrimSpace(line))
			}
		}
	}
	if images == 0 {
		return errors.New("its Compose file names no image")
	}
	seen := map[string]bool{}
	for _, in := range e.GetInputs() {
		name := in.GetName()
		switch {
		case !inputName.MatchString(name):
			return fmt.Errorf("the input %q needs a name a Compose file can use, in capitals, such as ADMIN_PORT", name)
		case seen[name]:
			return fmt.Errorf("two inputs are named %s. Rename one", name)
		case !strings.Contains(e.GetCompose(), "${"+name+"}"):
			return fmt.Errorf("its Compose file doesn't use the input %s. Use it as ${%s}, or remove it", name, name)
		case in.GetType() == catalogv1.InputType_INPUT_TYPE_SECRET && in.GetDefaultValue() != "":
			return fmt.Errorf("the secret %s has a default, which everyone who installs it would share. Set generate, or leave it to a person", name)
		case in.GetGenerate() > 0 && in.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET:
			return fmt.Errorf("the input %s sets generate, which only a secret takes", name)
		case in.GetGenerate() > 0 && in.GetGenerate() < 16:
			return fmt.Errorf("the secret %s generates %d characters. Generate at least 16", name, in.GetGenerate())
		}
		seen[name] = true
	}
	return nil
}

// Asks reports an input a person gives: one Dockwell generates isn't.
func Asks(in *catalogv1.Input) bool { return in.GetGenerate() == 0 }

// Secret reports an input kept as a secret.
func Secret(in *catalogv1.Input) bool { return in.GetType() == catalogv1.InputType_INPUT_TYPE_SECRET }

// Suggest says what each input takes when no one gives it anything, by name,
// with each port one that isn't in taken, or that an input before it took. Secrets
// have nothing to suggest.
func Suggest(e *catalogv1.Entry, taken map[uint32]bool) map[string]string {
	out := map[string]string{}
	used := map[uint32]bool{}
	for _, in := range e.GetInputs() {
		if Secret(in) || in.GetDefaultValue() == "" {
			continue
		}
		if in.GetType() != catalogv1.InputType_INPUT_TYPE_PORT {
			out[in.GetName()] = in.GetDefaultValue()
			continue
		}
		port, err := strconv.ParseUint(in.GetDefaultValue(), 10, 16)
		if err != nil {
			continue
		}
		for p := uint32(port); p <= 65535; p++ {
			if !taken[p] && !used[p] {
				used[p] = true
				out[in.GetName()] = strconv.FormatUint(uint64(p), 10)
				break
			}
		}
	}
	return out
}

// Filled is an entry's inputs, filled in for an install.
type Filled struct {
	// Variables are the values of what its Compose file uses, secrets among them.
	Variables map[string]string
	// Secrets are those kept as secrets, by name.
	Secrets map[string]string
	// Plain are the others, which the install's pin keeps.
	Plain map[string]string
}

// Longest is how many bytes a value may hold.
const longest = 4 << 10

// Fill gives every input its value: what's given, or what's suggested, or for a
// secret no one types, one made here. It refuses a value that isn't what its
// input takes, by the input's label, and a value for no input.
func Fill(e *catalogv1.Entry, given, suggested map[string]string) (*Filled, error) {
	out := &Filled{Variables: map[string]string{}, Secrets: map[string]string{}, Plain: map[string]string{}}
	for name := range given {
		i := slices.IndexFunc(e.GetInputs(), func(in *catalogv1.Input) bool { return in.GetName() == name })
		if i < 0 {
			return nil, fmt.Errorf("%s takes no input %s. It takes %s", e.GetName(), name, names(e))
		}
		if !Asks(e.GetInputs()[i]) {
			return nil, fmt.Errorf("%s takes no value for %s, as Dockwell generates it", e.GetName(), name)
		}
	}
	for _, in := range e.GetInputs() {
		name := in.GetName()
		value := strings.TrimSpace(cmp.Or(given[name], suggested[name]))
		if in.GetGenerate() > 0 {
			var err error
			if value, err = generate(int(in.GetGenerate())); err != nil {
				return nil, err
			}
		}
		if err := checkValue(in, value); err != nil {
			return nil, err
		}
		out.Variables[name] = value
		switch {
		case !Secret(in):
			out.Plain[name] = value
		case value != "":
			out.Secrets[name] = value
		}
	}
	return out, nil
}

// checkValue refuses a value that isn't what its input takes. It never quotes one,
// as it may be a secret.
func checkValue(in *catalogv1.Input, value string) error {
	label := in.GetLabel()
	switch {
	case value == "" && in.GetOptional():
		return nil
	case value == "":
		return fmt.Errorf("%s needs a value", label)
	case len(value) > longest:
		return fmt.Errorf("%s is %d KiB long, and takes up to %d KiB", label, len(value)>>10, longest>>10)
	case strings.ContainsAny(value, "\r\n"):
		return fmt.Errorf("%s takes one line", label)
	}
	switch in.GetType() {
	case catalogv1.InputType_INPUT_TYPE_PORT:
		if port, err := strconv.ParseUint(value, 10, 16); err != nil || port == 0 {
			return fmt.Errorf("%s takes a port from 1 to 65535", label)
		}
	case catalogv1.InputType_INPUT_TYPE_EMAIL:
		if a, err := mail.ParseAddress(value); err != nil || a.Address != value {
			return fmt.Errorf("%s takes an email address, such as you@example.com", label)
		}
	case catalogv1.InputType_INPUT_TYPE_UNSPECIFIED, catalogv1.InputType_INPUT_TYPE_TEXT, catalogv1.InputType_INPUT_TYPE_SECRET:
	}
	return nil
}

func names(e *catalogv1.Entry) string {
	var out []string
	for _, in := range e.GetInputs() {
		if Asks(in) {
			out = append(out, in.GetName())
		}
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}

// generated is what a generated secret is made of: letters and digits alone, so it
// means the same in a URL, a shell and a config file.
const generated = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// generate makes a secret of n characters.
func generate(n int) (string, error) {
	var b strings.Builder
	size := big.NewInt(int64(len(generated)))
	for range n {
		i, err := rand.Int(rand.Reader, size)
		if err != nil {
			return "", fmt.Errorf("generating a secret: %w", err)
		}
		b.WriteByte(generated[i.Int64()])
	}
	return b.String(), nil
}
