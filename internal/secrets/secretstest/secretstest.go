// SPDX-License-Identifier: AGPL-3.0-only

// Package secretstest gives tests a secrets.Keeper under one fixed install
// key, so every engine a test makes over one database can read what the
// others sealed.
package secretstest

import (
	"bytes"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// Key is the install key tests use.
func Key() *seal.Key {
	k, err := seal.FromBytes(bytes.Repeat([]byte{0x5e}, 32))
	if err != nil {
		panic(err)
	}
	return k
}

// Keeper returns a keeper over db with Key, reading specs of kinds and of
// projects and environments.
func Keeper(db *store.DB, kinds *resource.Registry) *secrets.Keeper {
	return secrets.New(db, Key(), kinds.With(scope.Types()...))
}
