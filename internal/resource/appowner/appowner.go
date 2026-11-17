// SPDX-License-Identifier: AGPL-3.0-only

// Package appowner is the "app.owner" kind: the owner's account in a catalog app,
// which Dockwell makes once the app runs. It says who the owner is, never how
// they're made: the control plane reads the steps from the install's catalog entry
// at its release, and asks the app's agent to run them, with the password for that
// call alone, so the password is never part of any server's desired state. Whether
// it's made is what Dockwell observes, never a field of its spec. See
// docs/adr/0105-a-catalog-apps-owner-is-made-at-install.md.
package appowner

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the app owner kind.
const Kind resource.Kind = "app.owner"

// Name is what an install calls its owner: one in each install's environment.
const Name = "owner"

// Type is the kind.
type Type struct{}

var (
	_ resource.Type     = Type{}
	_ resource.Unplaced = Type{}
	_ resource.Referrer = Type{}
)

// Kind returns "app.owner".
func (Type) Kind() resource.Kind { return Kind }

// Level is the environment, as its app's.
func (Type) Level() resource.Level { return resource.LevelEnvironment }

// IDPrefix is "own".
func (Type) IDPrefix() string { return "own" }

// New returns an empty AppOwner.
func (Type) New() proto.Message { return &resourcev1.AppOwner{} }

// Unplaced says the control plane holds it.
func (Type) Unplaced() {}

// Validate refuses an owner that doesn't say whose it is.
func (Type) Validate(spec proto.Message) error {
	o := spec.(*resourcev1.AppOwner)
	switch {
	case o.GetService() == "":
		return errors.New("an app's owner needs the service it's made in, by name")
	case o.GetPassword() == "":
		return errors.New("an app's owner needs the secret its password is in, by name")
	case strings.ContainsAny(o.GetUser(), "\r\n") || strings.ContainsAny(o.GetApp(), "\r\n"):
		return errors.New("an app's owner and its app's name are each one line")
	}
	if e := o.GetEmail(); e != "" {
		if a, err := mail.ParseAddress(e); err != nil || a.Address != e {
			return fmt.Errorf("%q isn't an email address", e)
		}
	}
	return nil
}

// References are its service and its password's secret.
func (Type) References(spec proto.Message) []resource.Reference {
	o := spec.(*resourcev1.AppOwner)
	return []resource.Reference{
		{Kind: "service", Name: o.GetService(), Verb: "make its owner in", Apart: true},
		{Kind: "secret", Name: o.GetPassword(), Verb: "read"},
	}
}

// ReferenceKinds are a service and a secret.
func (Type) ReferenceKinds() []resource.Kind { return []resource.Kind{"service", "secret"} }
