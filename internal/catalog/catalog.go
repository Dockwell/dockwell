// SPDX-License-Identifier: AGPL-3.0-only

// Package catalog holds the apps Dockwell can install: entries in the format ADR
// 0029 sets out, which it reads and fills in for an install. Dockwell's own are
// written in Dockwell/catalog, which every install reads as a source; the binary
// holds a snapshot of it (package snapshot), which BuiltIn reads. See
// docs/adr/0029-a-catalog-format-fed-from-other-catalogs.md and
// docs/adr/0077-dockwells-catalog-in-its-own-repository.md.
package catalog

import (
	"cmp"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math/big"
	"net/mail"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v4"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog/snapshot"
	"github.com/dockwell/dockwell/internal/resource"
)

// Catalog is a set of entries, sorted by name.
type Catalog struct {
	entries []*catalogv1.Entry
}

// builtIn is what ships in the binary, Dockwell/catalog's snapshot, read once.
var builtIn = sync.OnceValues(func() (*Catalog, error) { return load(snapshot.Entries(), "*.yaml") })

// DirEnv names a directory of entries, read each time and taking over from an entry
// of Dockwell's own of the same ID. It's what a newer release looks like before the
// catalog can bring one, for trying an entry and for the tests that upgrade one.
const DirEnv = "DOCKWELL_CATALOG_DIR"

// FromDir returns the entries DirEnv names, or an empty catalog without it, read
// each time.
func FromDir() (*Catalog, error) {
	dir := os.Getenv(DirEnv)
	if dir == "" {
		return &Catalog{}, nil
	}
	return load(os.DirFS(dir), "*.yaml")
}

// Over returns the catalog with each of more's entries in place of its own of the
// same ID, and the rest of more beside them, as DirEnv's take over.
func (c *Catalog) Over(more *Catalog) *Catalog {
	out := &Catalog{entries: slices.Clone(c.entries)}
	for _, e := range more.entries {
		out.entries = slices.DeleteFunc(out.entries, func(had *catalogv1.Entry) bool { return had.GetId() == e.GetId() })
		out.entries = append(out.entries, e)
	}
	sortEntries(out.entries)
	return out
}

// Load returns the catalog: what's built in, and what DirEnv adds. commit is the
// build's, which an entry built in takes as the commit it came from.
func Load(commit string) (*Catalog, error) {
	out, err := BuiltIn(commit)
	if err != nil {
		return nil, err
	}
	more, err := FromDir()
	if err != nil {
		return nil, err
	}
	return out.Over(more), nil
}

// sortEntries sorts by name as people read one, whatever its case and accents, so n8n
// and pgAdmin sit among the others rather than after them all, and É with E. Two of
// one name keep an order of their own, by ID.
func sortEntries(entries []*catalogv1.Entry) {
	slices.SortFunc(entries, func(a, b *catalogv1.Entry) int {
		return cmp.Or(strings.Compare(sortKey(a.GetName()), sortKey(b.GetName())), strings.Compare(a.GetName(), b.GetName()), strings.Compare(a.GetId(), b.GetId()))
	})
}

// BuiltIn returns the entries built into the binary alone, Dockwell/catalog's
// snapshot, whatever DirEnv names, for what must tell Dockwell's own entries from
// any other.
func BuiltIn(commit string) (*Catalog, error) {
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

// sortKey is a name as it sorts: in lower case, with its accents taken off.
func sortKey(name string) string {
	folded, _, err := transform.String(transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC), name)
	if err != nil {
		folded = name
	}
	return strings.ToLower(folded)
}

// With returns the catalog with a source's entries beside its own, sorted as it is.
// An entry of an ID the catalog has is left out, as one would take over from the
// other: what checks a source's entries says so first.
func (c *Catalog) With(more []*catalogv1.Entry) *Catalog {
	out := &Catalog{entries: slices.Clone(c.entries)}
	for _, e := range more {
		if !slices.ContainsFunc(out.entries, func(had *catalogv1.Entry) bool { return had.GetId() == e.GetId() }) {
			out.entries = append(out.entries, e)
		}
	}
	sortEntries(out.entries)
	return out
}

// load reads every entry that matches a pattern.
func load(dir fs.FS, pattern string) (*Catalog, error) {
	files, err := fs.Glob(dir, pattern)
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
	sortEntries(c.entries)
	return c, nil
}

// Parse reads one entry from YAML, in the schema's own field names, and checks
// it. A field the schema doesn't have is refused, as it would otherwise do
// nothing without a word.
func Parse(raw []byte) (*catalogv1.Entry, error) {
	e, err := Decode(raw)
	if err != nil {
		return nil, err
	}
	return e, Check(e)
}

// ParseTagged reads one entry as Parse does, and checks it as CheckTagged does.
func ParseTagged(raw []byte) (*catalogv1.Entry, error) {
	e, err := Decode(raw)
	if err != nil {
		return nil, err
	}
	return e, CheckTagged(e)
}

// Decode reads an entry from YAML as Parse does, and checks nothing beyond its
// fields, for part of one, such as what a converter adds to an entry it makes.
func Decode(raw []byte) (*catalogv1.Entry, error) {
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
	return e, nil
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

// taggedImage is an image line that names its image plainly, by a tag, with no
// digest: what an entry converted on the control plane writes until an install pins
// it (ADR 0078). Its groups are what comes before the image, and the image.
var taggedImage = regexp.MustCompile(`^(\s*image:\s*)([^\s@#"']+:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})\s*$`)

// digestOf is the digest an image line pins, as @sha256:<hex>.
var digestOf = regexp.MustCompile(`@sha256:[0-9a-f]{64}`)

// digestShape is a digest as an image is pinned by one.
var digestShape = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Digest says a registry's answer is a digest an image can be pinned by: sha256 and
// 64 hex digits, and nothing else.
func Digest(d string) bool { return digestShape.MatchString(d) }

// MovingTags are tags that name a branch rather than a version, which moves.
var MovingTags = map[string]bool{"latest": true, "stable": true, "main": true, "master": true, "nightly": true, "dev": true, "develop": true, "edge": true, "beta": true, "server": true}

// Moving says an image names a tag that moves, such as latest, with or without the
// digest an install pinned it to: what runs is the build the tag pointed at when the
// install's plan pinned it, and a newer build is an update to it. An image named with
// no tag is latest, as Docker pulls it, unless a digest alone names it, which follows
// nothing.
func Moving(image string) bool {
	image, digest, pinned := strings.Cut(image, "@")
	colon := strings.LastIndex(image, ":")
	if colon <= strings.LastIndex(image, "/") {
		return !pinned || digest == ""
	}
	return MovingTags[image[colon+1:]]
}

// MovingImages are the images an entry's services run by a tag that moves, as its
// file names them, once each.
func MovingImages(compose string) []string {
	var out []string
	for _, image := range TaggedImages(compose) {
		if Moving(image) {
			out = append(out, image)
		}
	}
	for image := range PinnedDigests(compose) {
		if Moving(image) && !slices.Contains(out, image) {
			out = append(out, image)
		}
	}
	slices.Sort(out)
	return out
}

// MaxGenerate is the most characters a secret an entry generates may have, as an
// entry is a stranger's file once it comes from a source, and generating one costs
// the control plane what it says.
const MaxGenerate = 256

// Tagged says an entry names an image by its tag alone: one converted on the control
// plane, which an install pins by digest when it's planned (ADR 0078).
func Tagged(e *catalogv1.Entry) bool {
	for line := range strings.Lines(e.GetCompose()) {
		if strings.HasPrefix(strings.TrimSpace(line), "image:") && !pinned.MatchString(line) {
			return true
		}
	}
	return false
}

// serviceImageLines are the lines of a Compose file, from 1, that hold a service's
// own image, as a plain scalar on its key's line: what an install runs. An image:
// line anywhere else, such as a variable in a service's environment, is none of them.
func serviceImageLines(compose string) map[int]bool {
	var doc yaml.Node
	if yaml.Unmarshal([]byte(compose), &doc) != nil || len(doc.Content) != 1 {
		return nil
	}
	out := map[int]bool{}
	services := mappingValue(doc.Content[0], "services")
	for i := 1; services != nil && i < len(services.Content); i += 2 {
		if image := mappingValue(services.Content[i], "image"); image != nil && image.Kind == yaml.ScalarNode && image.Style == 0 {
			out[image.Line] = true
		}
	}
	return out
}

// mappingValue is a mapping's value of a key, or nil.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// TaggedImages are the images an entry's services run that its file names by tag
// alone, in the order its file names them, once each.
func TaggedImages(compose string) []string {
	lines := serviceImageLines(compose)
	var out []string
	n := 0
	for line := range strings.Lines(compose) {
		n++
		if !lines[n] {
			continue
		}
		if m := taggedImage.FindStringSubmatch(strings.TrimRight(line, "\n")); len(m) == 3 && !slices.Contains(out, m[2]) {
			out = append(out, m[2])
		}
	}
	return out
}

// Pin writes each image an entry's services run by tag as the digest digests gives
// it, by the image as the file names it, on the services' own image lines alone. An
// image digests hasn't got is left as it is.
func Pin(compose string, digests map[string]string) string {
	lines := serviceImageLines(compose)
	var b strings.Builder
	n := 0
	for line := range strings.Lines(compose) {
		n++
		body, nl := strings.CutSuffix(line, "\n")
		if m := taggedImage.FindStringSubmatch(body); lines[n] && len(m) == 3 && digests[m[2]] != "" {
			body = m[1] + m[2] + "@" + digests[m[2]]
		}
		b.WriteString(body)
		if nl {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// Unpin is a file as it read before an install pinned its images by digest.
func Unpin(compose string) string {
	var b strings.Builder
	for line := range strings.Lines(compose) {
		if strings.HasPrefix(strings.TrimSpace(line), "image:") {
			line = digestOf.ReplaceAllString(line, "")
		}
		b.WriteString(line)
	}
	return b.String()
}

// PinnedDigests are the digests a file pins, by the image as a tagged entry names
// it, such as nginx:1.27, so an install that's pinned keeps what it runs for an
// image whose tag didn't change.
func PinnedDigests(compose string) map[string]string {
	out := map[string]string{}
	lines := serviceImageLines(compose)
	n := 0
	for line := range strings.Lines(compose) {
		n++
		if !lines[n] {
			continue
		}
		trimmed := strings.TrimSpace(line)
		image, ok := strings.CutPrefix(trimmed, "image:")
		if !ok {
			continue
		}
		image = strings.TrimSpace(image)
		if at := strings.Index(image, "@sha256:"); at > 0 && digestOf.MatchString(image[at:]) {
			out[image[:at]] = image[at+1:]
		}
	}
	return out
}

// inputName is the name a Compose file's ${NAME} takes.
var inputName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Check says what's wrong with an entry, as the catalog's CI would refuse it: its
// ID, what it says of where it came from, its inputs, and an image not pinned by
// digest.
func Check(e *catalogv1.Entry) error {
	return check(e, false)
}

// CheckTagged checks an entry as Check does, but for its images, which it may name
// by a tag, written plainly, rather than by digest: one the control plane converted
// from another store, which an install pins when it's planned (ADR 0078). A tag that
// moves, such as latest, is pinned as a version's is, and its newer builds are
// updates (ADR 0085).
func CheckTagged(e *catalogv1.Entry) error {
	return check(e, true)
}

func check(e *catalogv1.Entry, byTag bool) error {
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
	if err := checkPresentation(e); err != nil {
		return err
	}
	images := 0
	for line := range strings.Lines(e.GetCompose()) {
		if strings.HasPrefix(strings.TrimSpace(line), "image:") {
			images++
			if pinned.MatchString(line) {
				continue
			}
			m := taggedImage.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
			switch {
			case !byTag:
				return fmt.Errorf("%s isn't pinned by digest. Add its @sha256 digest", strings.TrimSpace(line))
			case m == nil:
				return fmt.Errorf("%s doesn't name its image plainly, by a version", strings.TrimSpace(line))
			case strings.Contains(m[2][strings.LastIndex(m[2], ":"):], "/"):
				return fmt.Errorf("%s names no tag", strings.TrimSpace(line))
			}
		}
	}
	if images == 0 {
		return errors.New("its Compose file names no image")
	}
	if err := checkDatabases(e); err != nil {
		return err
	}
	if err := checkAccess(e); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, in := range e.GetInputs() {
		name := in.GetName()
		switch {
		case !inputName.MatchString(name):
			return fmt.Errorf("the input %q needs a name a Compose file can use, in capitals, such as ADMIN_PORT", name)
		case seen[name]:
			return fmt.Errorf("two inputs are named %s. Rename one", name)
		case !regexp.MustCompile(`\$\{` + name + `(\}|:?[-?+])`).MatchString(e.GetCompose()):
			return fmt.Errorf("its Compose file doesn't use the input %s. Use it as ${%s}, or as ${%s:-default}, or remove it", name, name, name)
		case in.GetType() == catalogv1.InputType_INPUT_TYPE_SECRET && in.GetDefaultValue() != "":
			return fmt.Errorf("the secret %s has a default, which everyone who installs it would share. Set generate, or leave it to a person", name)
		case in.GetGenerate() > 0 && in.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET:
			return fmt.Errorf("the input %s sets generate, which only a secret takes", name)
		case in.GetGenerate() > 0 && in.GetGenerate() < 16:
			return fmt.Errorf("the secret %s generates %d, too few to guess at. Generate at least 16", name, in.GetGenerate())
		case in.GetGenerate() > MaxGenerate:
			return fmt.Errorf("the secret %s generates %d, more than the %d Dockwell generates", name, in.GetGenerate(), MaxGenerate)
		case in.GetGenerateAs() != catalogv1.GenerateAs_GENERATE_AS_UNSPECIFIED && in.GetGenerate() == 0:
			return fmt.Errorf("the input %s says how to write what it generates, and generates nothing. Set generate, or take generate_as away", name)
		case in.GetSetOnce() && in.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET:
			return fmt.Errorf("the input %s is set once, which only a secret is. Take set_once away", name)
		case (in.GetMinLength() > 0 || in.GetCharacters() != catalogv1.Characters_CHARACTERS_UNSPECIFIED) && !typed(in):
			return fmt.Errorf("the input %s says how long it is or what characters it takes, which only text and a secret do. Take min_length and characters away", name)
		case in.GetMinLength() > 0 && in.GetGenerateAs() != catalogv1.GenerateAs_GENERATE_AS_UNSPECIFIED:
			return fmt.Errorf("the input %s sets min_length, and its generate_as says what it takes already. Take min_length away", name)
		case in.GetGenerate() > 0 && in.GetMinLength() > in.GetGenerate():
			return fmt.Errorf("the input %s takes at least %d characters, and generates %d. Generate at least %d", name, in.GetMinLength(), in.GetGenerate(), in.GetMinLength())
		case in.GetCharacters() == catalogv1.Characters_CHARACTERS_URL_SAFE && in.GetGenerateAs() == catalogv1.GenerateAs_GENERATE_AS_BASE64:
			return fmt.Errorf("the input %s takes what a URL carries, and generates base64, which has + / and =. Take characters or generate_as away", name)
		}
		seen[name] = true
		if Secret(in) {
			if err := standsAlone(e.GetCompose(), name); err != nil {
				return err
			}
		}
	}
	if err := checkCompanions(e); err != nil {
		return err
	}
	return checkNeeds(e)
}

// checkDatabases refuses a database the entry marks that its Compose file doesn't run.
func checkDatabases(e *catalogv1.Entry) error {
	if len(e.GetDatabases()) == 0 {
		return nil
	}
	var file struct {
		Services map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(e.GetCompose()), &file); err != nil {
		return fmt.Errorf("its Compose file isn't YAML: %w", err)
	}
	for _, name := range e.GetDatabases() {
		if _, ok := file.Services[name]; !ok {
			return fmt.Errorf("it marks %s as a database, which its Compose file doesn't run. Name one of its services", name)
		}
	}
	return nil
}

// checkAccess refuses what an entry says of signing in, sign-ups and its address
// that names no input or service of its own, and a note that isn't one short line.
func checkAccess(e *catalogv1.Entry) error {
	inputs := map[string]*catalogv1.Input{}
	for _, in := range e.GetInputs() {
		inputs[in.GetName()] = in
	}
	if s := e.GetSignIn(); s != nil {
		password := inputs[s.GetPasswordInput()]
		switch {
		case s.GetUser() != "" && s.GetUserInput() != "":
			return errors.New("its sign-in names a user and an input for one. Keep one")
		case s.GetUserInput() != "" && (inputs[s.GetUserInput()] == nil || Secret(inputs[s.GetUserInput()])):
			return fmt.Errorf("its sign-in takes the user from %s, which isn't an input of its own that isn't a secret", s.GetUserInput())
		case s.GetPasswordInput() != "" && (password == nil || !Secret(password)):
			return fmt.Errorf("its sign-in takes the password from %s, which isn't one of its secret inputs", s.GetPasswordInput())
		case s.GetChangedIn() != "" && !password.GetSetOnce():
			return fmt.Errorf("its sign-in says where the password changes in the app, and %s isn't set once. Set set_once on it, or take changed_in away", s.GetPasswordInput())
		case e.GetWeb().GetService() == "":
			return errors.New("it says how to sign in, and has no web service to sign in to. Set web")
		}
	}
	services, err := composeServices(e.GetCompose())
	if err != nil {
		return err
	}
	if s := e.GetSignups(); s != nil {
		if _, ok := services[s.GetService()]; !ok || !envName.MatchString(s.GetVariable()) || s.GetClosed() == "" {
			return fmt.Errorf("its sign-ups close with %s=%s on %s, which needs a service it runs, a variable's name and a value", s.GetVariable(), s.GetClosed(), s.GetService())
		}
	}
	for _, a := range e.GetAddressVariables() {
		if _, ok := services[a.GetService()]; !ok || !envName.MatchString(a.GetVariable()) || a.GetValue() == "" {
			return fmt.Errorf("its address sets %s on %s, which needs a service it runs, a variable's name and a value", a.GetVariable(), a.GetService())
		}
		if strings.Contains(strings.ReplaceAll(a.GetValue(), "{domain}", ""), "{") {
			return fmt.Errorf("its address sets %s to %q, which names something other than {domain}", a.GetVariable(), a.GetValue())
		}
	}
	for _, n := range e.GetNotes() {
		if strings.TrimSpace(n) == "" || strings.ContainsAny(n, "\r\n") || len(n) > maxNote {
			return fmt.Errorf("its note %q isn't one line of up to %d characters. Split it, or say it in fewer words", n, maxNote)
		}
	}
	return nil
}

// maxNote is how long a note may be: one line, as a person reads it.
const maxNote = 120

// envName is a variable a service's environment takes, in any case, as Ghost's url is.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// composeServices is a Compose file's services, by name.
func composeServices(compose string) (map[string]any, error) {
	var file struct {
		Services map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(compose), &file); err != nil {
		return nil, fmt.Errorf("its Compose file isn't YAML: %w", err)
	}
	return file.Services, nil
}

// standsAlone refuses a secret inside a longer value. An upgrade stands something in for
// a secret it never opens, which reaches a service's environment only as a whole value,
// by reference, so in a longer one it would write that in place of the secret, and a
// rotation couldn't reach it either. It reads the file's YAML, as text can't tell
// "a ${X} b" from "${X}".
//
// A service's command, entrypoint and health check's test are kept by the names their
// words use, and each word is written with the values it names as the container is
// made (ADR 0022), so a secret may stand anywhere in one of their words, as in
// redis-server --requirepass ${PASSWORD} or --api-key=${KEY} (ADR 0085).
func standsAlone(file, name string) error {
	var tree any
	if err := yaml.Unmarshal([]byte(file), &tree); err != nil {
		return fmt.Errorf("its Compose file isn't YAML: %w", err)
	}
	whole := regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*=)?\$\{` + regexp.QuoteMeta(name) + `\}$`)
	// The name whole, so KEY isn't found in ${KEY2}.
	uses := regexp.MustCompile(`\$\{` + regexp.QuoteMeta(name) + `(\}|:?[-?+])`)
	var walk func(any) error
	walk = func(v any) error {
		switch v := v.(type) {
		case string:
			if uses.MatchString(v) && !whole.MatchString(v) {
				return fmt.Errorf("the secret %s is inside a longer value, %q, which an upgrade and a rotation couldn't reach. Give the app the secret as a value of its own", name, strings.ReplaceAll(v, "\n", " "))
			}
		case []any:
			for _, item := range v {
				if err := walk(item); err != nil {
					return err
				}
			}
		case map[string]any:
			for _, key := range slices.Sorted(maps.Keys(v)) {
				if err := walk(v[key]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	top, ok := tree.(map[string]any)
	if !ok {
		return walk(tree)
	}
	for _, key := range slices.Sorted(maps.Keys(top)) {
		if key == "configs" && uses.MatchString(fmt.Sprint(top[key])) {
			// A config's content is a file Dockwell writes and a plan shows, never a
			// secret, even as all it holds (ADR 0085).
			return fmt.Errorf("a config writes the secret %s into a file a plan shows, which an upgrade and a rotation couldn't reach either. Give the app the secret in its environment", name)
		}
		if key != "services" {
			if err := walk(top[key]); err != nil {
				return err
			}
			continue
		}
		services, ok := top[key].(map[string]any)
		if !ok {
			if err := walk(top[key]); err != nil {
				return err
			}
			continue
		}
		for _, svcName := range slices.Sorted(maps.Keys(services)) {
			svc, ok := services[svcName].(map[string]any)
			if !ok {
				if err := walk(services[svcName]); err != nil {
					return err
				}
				continue
			}
			for _, field := range slices.Sorted(maps.Keys(svc)) {
				value := svc[field]
				switch field {
				case "command", "entrypoint":
					continue
				case "healthcheck":
					if check, ok := value.(map[string]any); ok {
						rest := maps.Clone(check)
						delete(rest, "test")
						value = rest
					}
				}
				if err := walk(value); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// typed reports an input whose value is text a person may type, which can say how
// long it is and what characters it takes: a port and an email address say that by
// their type.
func typed(in *catalogv1.Input) bool {
	switch in.GetType() {
	case catalogv1.InputType_INPUT_TYPE_UNSPECIFIED, catalogv1.InputType_INPUT_TYPE_TEXT, catalogv1.InputType_INPUT_TYPE_SECRET:
		return true
	case catalogv1.InputType_INPUT_TYPE_PORT, catalogv1.InputType_INPUT_TYPE_EMAIL:
	}
	return false
}

// Asks reports an input a person gives: one Dockwell generates isn't, though a person
// may give their own value for it.
func Asks(in *catalogv1.Input) bool { return in.GetGenerate() == 0 }

// OwnFloor is how many characters a person's own value for a secret Dockwell
// generates has at least, where its input says no more.
const OwnFloor = 8

// MinLength is how many characters a value a person gives an input must have at
// least: what the input says, or OwnFloor for a secret Dockwell generates, or 0 for
// any. A secret the app decodes, by its generate_as, takes its own form instead.
func MinLength(in *catalogv1.Input) int {
	switch {
	case in.GetGenerateAs() != catalogv1.GenerateAs_GENERATE_AS_UNSPECIFIED:
		return 0
	case in.GetMinLength() > 0:
		return int(in.GetMinLength())
	case in.GetGenerate() > 0:
		return OwnFloor
	}
	return 0
}

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
	// Variables are the values of what its Compose file uses, with a value of no
	// meaning standing for each secret, which a service then uses by reference
	// (Stands). So no secret's value reaches the file's reading, or a plan, and a
	// value a person typed can't be mistaken for another that's the same.
	Variables map[string]string
	// Secrets are those kept as secrets, by name, with their values.
	Secrets map[string]string
	// Plain are the others, which the install's pin keeps.
	Plain map[string]string
	// Held stands for secrets the install has already, by name, with a value of no
	// meaning, so nothing reads them to upgrade it. No service keeps that value: each
	// uses the secret by reference.
	Held map[string]string
	// Linked are the values of the names needs set that hold no password, by name,
	// which the install keeps as its environment's variables, and LinkStands what
	// stands for each among Variables, which a service then uses by reference (Link).
	Linked, LinkStands map[string]string
}

// Link has the app's services use each of names, which its needs set, as the
// environment's variable rather than hold the value: each takes a value of no meaning
// in the file's reading, as a secret does, and keeps its own in Linked. Plain still
// has it, so the install's pin keeps it for an upgrade.
func (f *Filled) Link(names []string) error {
	stands, err := StandFor(f.Variables, names)
	if err != nil {
		return err
	}
	f.Linked, f.LinkStands = map[string]string{}, stands
	for name := range stands {
		f.Linked[name] = f.Plain[name]
	}
	return nil
}

// StandFor puts a value of no meaning in place of each of names among variables, and
// says what stands for each, by name. A name variables doesn't have is left out.
func StandFor(variables map[string]string, names []string) (map[string]string, error) {
	out := map[string]string{}
	for _, name := range names {
		if _, ok := variables[name]; !ok {
			continue
		}
		stand, err := generate(32)
		if err != nil {
			return nil, err
		}
		variables[name], out[name] = stand, stand
	}
	return out, nil
}

// Upgrades reports whether an entry would change what an install runs. A release goes
// up with any change to an entry, and one that fixes a word in its summary leaves what
// runs as it is, so it's no reason to offer anyone a plan, or to warn them of one.
//
// An entry that names its images by tag is compared with the install's file as it
// read before the install pinned them, so a digest the install chose is no change.
func Upgrades(e *catalogv1.Entry, pin *resourcev1.CatalogPin) bool {
	file, entry := string(pin.GetFile()), e.GetCompose()
	if Tagged(e) {
		file, entry = Unpin(file), Unpin(entry)
	}
	return e.GetRelease() > pin.GetRelease() && file != entry
}

// usedNames finds the ${NAME}s a Compose file uses.
var usedNames = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)`)

// Given reads back what an install's file used: the values its pin kept, and a value of
// no meaning for each secret, which a pin never holds. So the old file reads again
// with no secret opened, for an upgrade to see what it gave.
func Given(pin *resourcev1.CatalogPin) (variables, stands map[string]string, err error) {
	variables, stands = map[string]string{}, map[string]string{}
	for _, m := range usedNames.FindAllSubmatch(pin.GetFile(), -1) {
		name := string(m[1])
		if _, done := variables[name]; done {
			continue
		}
		if v, ok := pin.GetValues()[name]; ok {
			variables[name] = v
			continue
		}
		stand, err := generate(32)
		if err != nil {
			return nil, nil, err
		}
		variables[name], stands[name] = stand, stand
	}
	return variables, stands, nil
}

// Stands is every value that stands for a secret among Variables, by the secret's
// name, for what has services use them by reference.
func (f *Filled) Stands() map[string]string {
	out := make(map[string]string, len(f.Secrets)+len(f.Held))
	for name := range f.Secrets {
		out[name] = f.Variables[name]
	}
	maps.Copy(out, f.Held)
	return out
}

// Longest is how many bytes a value may hold.
const longest = 4 << 10

// Fill gives every input its value: what's given, or what's suggested, or for a
// secret Dockwell generates that no one gave a value of their own, one made here.
// It refuses a value that isn't what its input takes, by the input's label, and a
// value for no input.
func Fill(e *catalogv1.Entry, given, suggested map[string]string) (*Filled, error) {
	return fill(e, given, suggested, nil)
}

// Carry fills an entry's inputs for an upgrade: what's given, then what the install
// was given, then what's suggested for an input this release adds. A secret in held,
// which the install has already, stays as it is, unless a new value is given for a
// secret a person gave. One Dockwell generated keeps its value: an upgrade isn't
// what sets it anew.
func Carry(e *catalogv1.Entry, pin *resourcev1.CatalogPin, given, suggested map[string]string, held map[string]bool) (*Filled, error) {
	values := maps.Clone(given)
	if values == nil {
		values = map[string]string{}
	}
	for _, in := range e.GetInputs() {
		if v, ok := pin.GetValues()[in.GetName()]; ok && Asks(in) && values[in.GetName()] == "" {
			values[in.GetName()] = v
		}
	}
	return fill(e, values, suggested, held)
}

func fill(e *catalogv1.Entry, given, suggested map[string]string, held map[string]bool) (*Filled, error) {
	out := &Filled{Variables: map[string]string{}, Secrets: map[string]string{}, Plain: map[string]string{}, Held: map[string]string{}}
	for name := range given {
		i := slices.IndexFunc(e.GetInputs(), func(in *catalogv1.Input) bool { return in.GetName() == name })
		if i < 0 {
			return nil, fmt.Errorf("%s takes no input %s. It takes %s", e.GetName(), name, names(e))
		}
		if in := e.GetInputs()[i]; !Asks(in) && held[name] && strings.TrimSpace(given[name]) != "" {
			return nil, fmt.Errorf("an upgrade keeps the %s that Dockwell generated for %s. Leave %s out, and set a new value on the secret's page", name, e.GetName(), name)
		}
	}
	for _, in := range e.GetInputs() {
		name := in.GetName()
		value := strings.TrimSpace(cmp.Or(given[name], suggested[name]))
		if Secret(in) && held[name] && value == "" {
			stands, err := generate(32)
			if err != nil {
				return nil, err
			}
			out.Variables[name], out.Held[name] = stands, stands
			continue
		}
		// A person's own value, where Dockwell would generate one, takes its place.
		own := value != ""
		if in.GetGenerate() > 0 && !own {
			var err error
			if value, err = generateAs(int(in.GetGenerate()), in.GetGenerateAs()); err != nil {
				return nil, err
			}
		}
		if err := checkValue(in, value, own); err != nil {
			return nil, err
		}
		switch {
		case !Secret(in):
			out.Variables[name], out.Plain[name] = value, value
		case value != "":
			stands, err := generate(32)
			if err != nil {
				return nil, err
			}
			out.Variables[name], out.Secrets[name] = stands, value
		default:
			out.Variables[name] = ""
		}
	}
	return out, nil
}

// checkValue refuses a value that isn't what its input takes, and one a person gave
// that isn't what the app takes, as its input says. It never quotes one, as it may
// be a secret.
func checkValue(in *catalogv1.Input, value string, own bool) error {
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
	if own {
		return CheckOwn(in, value)
	}
	return nil
}

// CheckOwn refuses a value a person gave that the app won't take, as its input says:
// a key it decodes in another form, a value too short, or a character it reads as its
// own, such as an @ in a password it writes into a URL. A secret takes printable
// characters alone. It never quotes the value.
func CheckOwn(in *catalogv1.Input, value string) error {
	label := in.GetLabel()
	switch in.GetGenerateAs() {
	case catalogv1.GenerateAs_GENERATE_AS_HEX:
		if len(value) != int(in.GetGenerate()) || strings.Trim(value, "0123456789abcdefABCDEF") != "" {
			return fmt.Errorf("%s takes %d hex digits, as the app decodes it", label, in.GetGenerate())
		}
		return nil
	case catalogv1.GenerateAs_GENERATE_AS_BASE64:
		if b, err := base64.StdEncoding.DecodeString(value); err != nil || len(b) != int(in.GetGenerate()) {
			return fmt.Errorf("%s takes %d bytes written in base64, as the app decodes it", label, in.GetGenerate())
		}
		return nil
	case catalogv1.GenerateAs_GENERATE_AS_UNSPECIFIED:
	}
	if n := MinLength(in); utf8.RuneCountInString(value) < n {
		return fmt.Errorf("%s takes at least %d characters", label, n)
	}
	for _, r := range value {
		switch {
		case Secret(in) && !unicode.IsPrint(r):
			return fmt.Errorf("%s takes printable characters alone", label)
		case in.GetCharacters() == catalogv1.Characters_CHARACTERS_URL_SAFE && !urlSafe(r):
			return fmt.Errorf("%s takes letters, digits and - . _ ~ alone, as the app writes it into a URL", label)
		}
	}
	return nil
}

// urlSafe reports a character a URL carries as it is: RFC 3986's unreserved.
func urlSafe(r rune) bool {
	return 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune("-._~", r)
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

// generateAs makes a secret as an input asks: n letters and digits, n hex digits, or
// n random bytes in base64.
func generateAs(n int, as catalogv1.GenerateAs) (string, error) {
	switch as {
	case catalogv1.GenerateAs_GENERATE_AS_HEX:
		b := make([]byte, (n+1)/2)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("generating a secret: %w", err)
		}
		return hex.EncodeToString(b)[:n], nil
	case catalogv1.GenerateAs_GENERATE_AS_BASE64:
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("generating a secret: %w", err)
		}
		return base64.StdEncoding.EncodeToString(b), nil
	default:
		return generate(n)
	}
}

// generate makes a secret of n letters and digits.
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
