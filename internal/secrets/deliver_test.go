// SPDX-License-Identifier: AGPL-3.0-only

package secrets_test

import (
	"bytes"
	"testing"

	"github.com/dockwell/dockwell/internal/delivery"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/secrets"
)

// What the control plane seals for a stream, server and resource, the
// agent opens there, and the sealed values hold nothing of them. An agent
// that sent no delivery key gets none.
func TestSealedValuesOpenOnTheAgent(t *testing.T) {
	k, err := delivery.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	api := resource.Ref{Kind: "service", Scope: "env_1", Name: "api"}
	sealed, err := secrets.SealValues(k.Public(), "srv_A", api, map[string]string{"DATABASE_URL": "postgres://hunter2@db"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("hunter2")) {
		t.Fatal("the sealed values hold the plaintext")
	}
	if got, err := k.Open("srv_A", api, sealed); err != nil || got["DATABASE_URL"] != "postgres://hunter2@db" {
		t.Fatalf("Open = %v, %v", got, err)
	}
	if _, err := k.Open("srv_B", api, sealed); err == nil {
		t.Error("values sealed for srv_A opened on srv_B")
	}
	if _, err := secrets.SealValues(nil, "srv_A", api, nil); err == nil {
		t.Error("sealed values for an agent with no delivery key")
	}
}
