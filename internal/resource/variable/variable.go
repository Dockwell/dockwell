// SPDX-License-Identifier: AGPL-3.0-only

// Package variable is the "variable" and "secret" kinds: the values of an
// environment that its services use by reference, under the names they
// expect. A variable isn't secret, and plans show it; a secret is kept
// encrypted, goes only to the servers that run a service that uses it, and
// is never shown but to a person who confirms with a passkey. Neither runs
// on a server itself: each is a resource.Valuer, with revisions and plans,
// whose value goes to the servers of the services that use it. See
// docs/adr/0022-secrets-encrypted-at-rest.md.
package variable

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
)

// The kinds.
const (
	VariableKind resource.Kind = "variable"
	SecretKind   resource.Kind = "secret"
)

// MaxValue bounds a value, well within what a process's environment takes.
const MaxValue = 64 << 10

// Types returns the variable and secret types.
func Types() []resource.Type { return []resource.Type{Type{}, SecretType{}} }

var nameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,254}$`)

// CheckName requires a name a process's environment takes: letters,
// digits and underscores, starting with a letter or an underscore, such as
// DATABASE_URL. Variables, secrets and the names services see them as all
// take such names. what names the thing, such as "variable".
func CheckName(what, name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%q isn't a valid %s name: use up to 255 letters, digits and underscores, starting with a letter or an underscore, such as DATABASE_URL",
			name, what)
	}
	return nil
}

func checkValue(what, value string) error {
	switch {
	case len(value) > MaxValue:
		return fmt.Errorf("a %s's value can be at most %d KiB, and this one is %d KiB", what, MaxValue>>10, (len(value)+1023)>>10)
	case strings.ContainsRune(value, 0):
		return fmt.Errorf("a %s's value can't hold a zero byte, which no process's environment can", what)
	}
	return nil
}

// Type is the variable kind.
type Type struct{}

var (
	_ resource.Valuer      = Type{}
	_ resource.NameChecker = Type{}
	_ resource.IDPrefixer  = Type{}
)

// Kind returns "variable".
func (Type) Kind() resource.Kind { return VariableKind }

// Level is the environment.
func (Type) Level() resource.Level { return resource.LevelEnvironment }

// IDPrefix is "var".
func (Type) IDPrefix() string { return id.Variable }

// New returns an empty Variable.
func (Type) New() proto.Message { return &resourcev1.Variable{} }

// Validate checks the value fits in a process's environment.
func (Type) Validate(spec proto.Message) error {
	return checkValue("variable", spec.(*resourcev1.Variable).GetValue())
}

// CheckName requires a name a process's environment takes.
func (Type) CheckName(name string) error { return CheckName("variable", name) }

// Value returns the variable's value.
func (Type) Value(spec proto.Message) string { return spec.(*resourcev1.Variable).GetValue() }

// SecretType is the secret kind.
type SecretType struct{}

var (
	_ resource.Valuer      = SecretType{}
	_ resource.NameChecker = SecretType{}
	_ resource.IDPrefixer  = SecretType{}
)

// Kind returns "secret".
func (SecretType) Kind() resource.Kind { return SecretKind }

// Level is the environment.
func (SecretType) Level() resource.Level { return resource.LevelEnvironment }

// IDPrefix is "sec".
func (SecretType) IDPrefix() string { return id.Secret }

// New returns an empty Secret.
func (SecretType) New() proto.Message { return &resourcev1.Secret{} }

// Validate requires a value that fits in a process's environment. An empty
// secret is almost always a mistake, such as an empty file read for it.
func (SecretType) Validate(spec proto.Message) error {
	v := spec.(*resourcev1.Secret).GetValue()
	if v == "" {
		return errors.New("a secret needs a value. Give it one, from standard input or a file on the command line")
	}
	return checkValue("secret", v)
}

// CheckName requires a name a process's environment takes.
func (SecretType) CheckName(name string) error { return CheckName("secret", name) }

// Value returns the secret's value.
func (SecretType) Value(spec proto.Message) string { return spec.(*resourcev1.Secret).GetValue() }
