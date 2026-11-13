// SPDX-License-Identifier: AGPL-3.0-only

// Package link is the "link" kind: how one of an app's needs is met, such as its
// database, by what meets it and the variables and secrets it sets in the app's
// environment, which the app's services use by name. It has revisions and plans
// like any kind, and runs on no server: the control plane holds it, as it holds a
// variable. See
// docs/adr/0084-what-an-app-needs-met-by-a-new-service-one-you-run-or-a-connection.md.
package link

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the link kind.
const Kind resource.Kind = "link"

// Type is the kind.
type Type struct{}

var (
	_ resource.Type     = Type{}
	_ resource.Unplaced = Type{}
	_ resource.Referrer = Type{}
	_ resource.Tied     = Type{}
)

// Kind returns "link".
func (Type) Kind() resource.Kind { return Kind }

// Level is the environment, as the app's.
func (Type) Level() resource.Level { return resource.LevelEnvironment }

// IDPrefix is "lnk".
func (Type) IDPrefix() string { return "lnk" }

// New returns an empty Link.
func (Type) New() proto.Message { return &resourcev1.Link{} }

// Unplaced says the control plane holds it.
func (Type) Unplaced() {}

// CheckName takes a need's name, as a service's.
func (Type) CheckName(name string) error {
	return resource.CheckLabel("link", name, "db")
}

var (
	// envName is what a Compose file can call a variable.
	envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// databaseName is what every engine takes as it is, unquoted (ADR 0038).
	databaseName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// Validate refuses a link that can't say what it sets.
func (Type) Validate(spec proto.Message) error {
	l := spec.(*resourcev1.Link)
	switch {
	case strings.TrimSpace(l.GetLabel()) == "" || strings.ContainsAny(l.GetLabel(), "\r\n"):
		return errors.New("a link needs a label on one line, such as Database")
	case l.GetService() == "":
		return errors.New("a link needs the service that meets it, by name")
	case l.GetMeetsEnvironment() != "" && !strings.HasPrefix(l.GetMeetsEnvironment(), "env_"):
		return errors.New("a link names the environment of what meets it by its ID, such as env_…")
	case resourcev1.LinkEngine_name[int32(l.GetEngine())] == "" || l.GetEngine() == resourcev1.LinkEngine_LINK_ENGINE_UNSPECIFIED:
		return errors.New("a link needs the engine of what meets it")
	case len(l.GetAccepts()) == 0:
		return errors.New("a link needs the engines its app takes")
	case len(l.GetSets()) == 0:
		return errors.New("a link needs the names it sets, such as DB_HOST")
	}
	for _, a := range l.GetAccepts() {
		if a.GetEngine() == resourcev1.LinkEngine_LINK_ENGINE_UNSPECIFIED {
			return errors.New("each engine a link's app takes needs naming")
		}
		if err := CheckVersions(a.GetVersions()); err != nil {
			return err
		}
	}
	for field, name := range map[string]string{"database": l.GetDatabaseName(), "user": l.GetUser()} {
		if name != "" && !databaseName.MatchString(name) {
			return fmt.Errorf("a link's %s %q needs lowercase letters, digits and underscores, starting with a letter, at most 32", field, name)
		}
	}
	seen := map[string]bool{}
	for _, s := range l.GetSets() {
		switch {
		case !envName.MatchString(s.GetName()):
			return fmt.Errorf("%q isn't a name a service can read, such as DB_HOST", s.GetName())
		case seen[s.GetName()]:
			return fmt.Errorf("a link sets %s twice", s.GetName())
		case s.GetPart() == resourcev1.LinkPart_LINK_PART_UNSPECIFIED || resourcev1.LinkPart_name[int32(s.GetPart())] == "":
			return fmt.Errorf("a link sets %s, and needs to say what it holds", s.GetName())
		case s.GetTemplate() != "" && s.GetPart() != resourcev1.LinkPart_LINK_PART_URL:
			return fmt.Errorf("a link writes %s from a template, which only a URL takes", s.GetName())
		}
		seen[s.GetName()] = true
	}
	return nil
}

// References names the service that meets it, in this environment, so it can't go
// while the link reads it. One in another environment is the link's to check as it's
// planned (ADR 0084).
func (Type) References(spec proto.Message) []resource.Reference {
	l := spec.(*resourcev1.Link)
	if l.GetMeetsEnvironment() != "" {
		return nil
	}
	return []resource.Reference{{Kind: "service", Name: l.GetService(), Verb: "read", Apart: true}}
}

// Ties are the values a link sets, a secret for each that holds the password, so a
// change that deletes its app or its database takes it, with them, once nothing left
// reads them (ADR 0084).
//
// A link to a server another environment runs stands for the network it reaches it
// on too, named for it (<need>-link), which goes with it.
func (Type) Ties(name string, spec proto.Message) []resource.Ref {
	l := spec.(*resourcev1.Link)
	var out []resource.Ref
	if l.GetMeetsEnvironment() != "" {
		out = append(out, resource.Ref{Kind: "network", Name: name + "-link"})
	}
	for _, s := range l.GetSets() {
		kind := resource.Kind("variable")
		if Secret(s) {
			kind = "secret"
		}
		out = append(out, resource.Ref{Kind: kind, Name: s.GetName()})
	}
	return out
}

// ReferenceKinds is services.
func (Type) ReferenceKinds() []resource.Kind { return []resource.Kind{"service"} }

// Secret reports a part that holds the password, and so is kept as a secret.
func Secret(s *resourcev1.LinkSet) bool {
	switch s.GetPart() {
	case resourcev1.LinkPart_LINK_PART_PASSWORD:
		return true
	case resourcev1.LinkPart_LINK_PART_URL:
		return s.GetTemplate() == "" || strings.Contains(s.GetTemplate(), "{password}")
	default:
		return false
	}
}

// A comparator is one test of a version: an operator, and the dotted numbers it
// compares with, at their own precision.
var comparator = regexp.MustCompile(`^(>=|<=|>|<|=)?(\d+(?:\.\d+)*)$`)

// CheckVersions refuses versions that aren't comparators on dotted numbers,
// separated by spaces, such as ">=10.6 <12". Empty takes any.
func CheckVersions(versions string) error {
	for _, c := range strings.Fields(versions) {
		if !comparator.MatchString(c) {
			return fmt.Errorf("%q isn't a version to compare with. Write one as >=8.0, <12 or =16, separated by spaces", c)
		}
	}
	return nil
}

// Takes reports whether a version is within versions: each comparator holds of the
// version cut to the comparator's own precision, so <=8.4 takes 8.4.11, and >=10.6
// takes 10.6.21. A version is read as its leading dotted numbers, so 11.4.4-MariaDB
// is 11.4.4. One with no number in front, such as latest, is taken by no range but
// the empty one. Empty takes any.
func Takes(versions, version string) bool {
	cs := strings.Fields(versions)
	if len(cs) == 0 {
		return true
	}
	have := Numbers(version)
	if len(have) == 0 {
		return false
	}
	for _, c := range cs {
		m := comparator.FindStringSubmatch(c)
		if m == nil {
			return false
		}
		want := Numbers(m[2])
		got := make([]int, len(want))
		copy(got, have)
		cmp := compare(got, want)
		ok := false
		switch m[1] {
		case ">=":
			ok = cmp >= 0
		case "<=":
			ok = cmp <= 0
		case ">":
			ok = cmp > 0
		case "<":
			ok = cmp < 0
		default:
			ok = cmp == 0
		}
		if !ok {
			return false
		}
	}
	return true
}

// Numbers are a version's leading dotted numbers: 11.4.4 for "11.4.4-MariaDB".
func Numbers(version string) []int {
	var out []int
	for part := range strings.SplitSeq(version, ".") {
		digits := part
		if i := strings.IndexFunc(part, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
			digits = part[:i]
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			break
		}
		out = append(out, n)
		if digits != part {
			break
		}
	}
	return out
}

func compare(a, b []int) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}
