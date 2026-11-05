// SPDX-License-Identifier: AGPL-3.0-only

package delivery

import (
	"crypto/hpke"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

var api = resource.Ref{Kind: "service", Scope: "env_1", Name: "api"}

// seal seals values as the control plane does (secrets.SealValues), with
// the suite and info this package says.
func seal(t *testing.T, public []byte, server string, ref resource.Ref, values map[string]string) []byte {
	t.Helper()
	kem, kdf, aead := Suite()
	pub, err := kem.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := proto.Marshal(&agentv1.Values{Values: values})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := hpke.Seal(pub, kdf, aead, Info(server, ref), plain)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// Values sealed for one stream, server and resource open only there, and
// a refusal never says them.
func TestValuesOpenOnlyWhereTheyWereSealedFor(t *testing.T) {
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	sealed := seal(t, k.Public(), "srv_A", api, map[string]string{"DATABASE_URL": "postgres://hunter2@db"})
	got, err := k.Open("srv_A", api, sealed)
	if err != nil || got["DATABASE_URL"] != "postgres://hunter2@db" {
		t.Fatalf("Open = %v, %v", got, err)
	}
	other, _ := NewKey()
	for what, try := range map[string]func() (map[string]string, error){
		"on another server": func() (map[string]string, error) { return k.Open("srv_B", api, sealed) },
		"for another service": func() (map[string]string, error) {
			return k.Open("srv_A", resource.Ref{Kind: "service", Scope: "env_1", Name: "web"}, sealed)
		},
		"on another stream": func() (map[string]string, error) { return other.Open("srv_A", api, sealed) },
		"cut short":         func() (map[string]string, error) { return k.Open("srv_A", api, sealed[:len(sealed)-1]) },
	} {
		if v, err := try(); err == nil || v != nil {
			t.Errorf("opening values %s = %v, %v; want it refused", what, v, err)
		} else if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the error says the value: %v", err)
		}
	}
}
