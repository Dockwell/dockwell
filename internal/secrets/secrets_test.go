// SPDX-License-Identifier: AGPL-3.0-only

package secrets_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/container/containertest"
	"github.com/dockwell/dockwell/internal/resource/firewall"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

type fleet map[string]resource.Agent

func (f fleet) Agent(id string) (resource.Agent, bool) {
	a, ok := f[id]
	return a, ok
}

func kinds() *resource.Registry {
	return resource.NewRegistry(&container.Driver{}, &firewall.Driver{}, &service.Driver{}, variable.Type{}, variable.SecretType{}).
		With(scope.Types()...)
}

func otherKey(t *testing.T) *seal.Key {
	t.Helper()
	k, err := seal.LoadOrCreate(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// sealIn seals a secret's spec.
func sealIn(t *testing.T, db *store.DB, k *secrets.Keeper, resourceID string, plain []byte) []byte {
	t.Helper()
	var out []byte
	if err := db.Update(t.Context(), func(tx *store.Tx) (err error) {
		out, err = k.Seal(t.Context(), tx, resourceID, variable.SecretKind, plain)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func openIn(t *testing.T, db *store.DB, k *secrets.Keeper, resourceID string, kind resource.Kind, sealed []byte) ([]byte, error) {
	t.Helper()
	var out []byte
	err := db.View(t.Context(), func(tx *store.Tx) (err error) {
		out, err = k.Open(t.Context(), tx, resourceID, kind, sealed)
		return err
	})
	return out, err
}

// A sealed spec holds nothing of the spec, opens only for the resource and
// kind it was sealed for, under this install's key, and not once changed.
func TestSealAndOpen(t *testing.T) { storetest.Each(t, testSealAndOpen) }

func testSealAndOpen(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	k := secretstest.Keeper(db, kinds())
	plain := []byte("value: hunter2 hunter2")
	sealed := sealIn(t, db, k, "sec_A", plain)
	if !secrets.Sealed(sealed) || bytes.Contains(sealed, []byte("hunter2")) {
		t.Fatalf("a sealed spec: %q", sealed)
	}
	if got, err := openIn(t, db, k, "sec_A", variable.SecretKind, sealed); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("Open = %q, %v", got, err)
	}
	again := sealIn(t, db, k, "sec_A", plain)
	if bytes.Equal(again, sealed) {
		t.Error("sealing the same spec twice gave the same bytes")
	}
	for what, try := range map[string]func() ([]byte, error){
		"for another resource": func() ([]byte, error) { return openIn(t, db, k, "sec_B", variable.SecretKind, sealed) },
		"as another kind":      func() ([]byte, error) { return openIn(t, db, k, "sec_A", variable.VariableKind, sealed) },
		"changed": func() ([]byte, error) {
			bad := bytes.Clone(sealed)
			bad[len(bad)-1] ^= 1
			return openIn(t, db, k, "sec_A", variable.SecretKind, bad)
		},
		"cut short": func() ([]byte, error) { return openIn(t, db, k, "sec_A", variable.SecretKind, sealed[:8]) },
		"under another install's key": func() ([]byte, error) {
			return openIn(t, db, secrets.New(db, otherKey(t), kinds()), "sec_A", variable.SecretKind, sealed)
		},
	} {
		if got, err := try(); err == nil || got != nil {
			t.Errorf("opening a sealed spec %s = %q, %v; want it refused", what, got, err)
		}
	}
	// A spec stored before sealing comes back as it is.
	if got, err := openIn(t, db, k, "res_A", container.Kind, []byte{0x0a, 0x01, 'x'}); err != nil || !bytes.Equal(got, []byte{0x0a, 0x01, 'x'}) {
		t.Errorf("an unsealed spec opened as %q, %v", got, err)
	}
}

// A digest is keyed: the same spec digests the same under one install's
// key, and differently under another's, and isn't its plain hash.
func TestDigestIsKeyed(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	a, b := secretstest.Keeper(db, kinds()), secrets.New(db, otherKey(t), kinds())
	spec := []byte("password=hunter2")
	plain := sha256.Sum256(spec)
	switch {
	case !bytes.Equal(a.Digest(spec), secretstest.Keeper(db, kinds()).Digest(spec)):
		t.Error("one key digested one spec two ways")
	case bytes.Equal(a.Digest(spec), b.Digest(spec)):
		t.Error("two keys digested a spec the same")
	case bytes.Equal(a.Digest(spec), plain[:]):
		t.Error("the digest is the spec's plain hash")
	}
}

// Rotating the key-encryption key wraps every data key again and retires
// the old one, forgetting it; every sealed spec still opens, in this
// process and in a new one, and the rotation is in the audit log.
func TestRotateKEK(t *testing.T) { storetest.Each(t, testRotateKEK) }

func testRotateKEK(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	k := secretstest.Keeper(db, kinds())
	sealed := map[string][]byte{}
	for _, id := range []string{"sec_1", "sec_2", "sec_3"} {
		sealed[id] = sealIn(t, db, k, id, []byte("value of "+id))
	}
	var before store.KeyEncryptionKey
	_ = db.View(t.Context(), func(tx *store.Tx) (err error) {
		before, err = tx.ActiveKEK(t.Context())
		return err
	})
	kekID, n, err := k.RotateKEK(t.Context(), "cli:mira@hz-1", store.SurfaceCLI)
	if err != nil || n != 3 || kekID == before.ID {
		t.Fatalf("RotateKEK = %s, %d, %v; want a new key with 3 data keys wrapped again", kekID, n, err)
	}
	fresh := secretstest.Keeper(db, kinds())
	for id, s := range sealed {
		for _, keeper := range []*secrets.Keeper{k, fresh} {
			if got, err := openIn(t, db, keeper, id, variable.SecretKind, s); err != nil || string(got) != "value of "+id {
				t.Errorf("after rotating, %s opens as %q, %v", id, got, err)
			}
		}
	}
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		old, err := tx.GetKEK(t.Context(), before.ID)
		if err != nil || old.RetiredAt == nil || len(old.Sealed) != 0 {
			t.Errorf("the old key after rotating: %+v, %v; want it retired and forgotten", old, err)
		}
		if left, err := tx.DataKeysUnder(t.Context(), before.ID, "", 10); err != nil || len(left) != 0 {
			t.Errorf("data keys left under the old key: %v, %v", left, err)
		}
		events, err := tx.ListEvents(t.Context(), 10)
		if err != nil || len(events) == 0 || events[0].Action != "secrets.rotate_key" || events[0].Actor != "cli:mira@hz-1" {
			t.Errorf("the audit log after rotating: %+v, %v", events, err)
		}
		return nil
	})
	// New specs are sealed under the new key.
	s := sealIn(t, db, k, "sec_4", []byte("four"))
	if got, err := openIn(t, db, fresh, "sec_4", variable.SecretKind, s); err != nil || string(got) != "four" {
		t.Errorf("a spec sealed after rotating opens as %q, %v", got, err)
	}
}

// A server gets the values of a service only when the service's latest
// revision is placed on it, and only for that revision's spec.
func TestValuesGoOnlyWhereTheServiceRuns(t *testing.T) {
	storetest.Each(t, testValuesGoOnlyWhereTheServiceRuns)
}

func testValuesGoOnlyWhereTheServiceRuns(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	logger := slog.New(slog.DiscardHandler)
	if _, err := revision.Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		for _, id := range []string{"srv_A", "srv_B"} {
			if err := tx.CreateServer(t.Context(), store.Server{ID: id, Name: id, CertSerial: id}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	k := secretstest.Keeper(db, kinds())
	docker := map[string]*containertest.Fake{"srv_A": containertest.NewFake(), "srv_B": containertest.NewFake()}
	agents := fleet{}
	for id, d := range docker {
		agents[id] = resource.Local{
			Drivers: resource.NewRegistry(&service.Driver{Containers: container.Driver{Runtime: d}}), Server: id, Values: k,
		}
	}
	e := revision.New(db, kinds(), agents, k, logger)
	var env string
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		tree, err := scope.Load(t.Context(), tx)
		if err != nil {
			t.Fatal(err)
		}
		_, e, err := tree.Resolve("", "")
		env = e.ID
		return err
	})
	api := &resourcev1.Service{
		Runs:    &resourcev1.Service_App{App: &resourcev1.Container{Image: "acme/api:1"}},
		Secrets: map[string]string{"DATABASE_URL": "DB_URL"},
	}
	plan, err := e.Propose(t.Context(), revision.Proposal{Summary: "Run api", Actor: "usr_test", Edits: []revision.Edit{
		{Ref: resource.Ref{Kind: variable.SecretKind, Scope: env, Name: "DB_URL"}, Spec: &resourcev1.Secret{Value: "postgres://hunter2@db"}},
		{Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: "api"}, Spec: api, Servers: []string{"srv_A"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Apply(t.Context(), plan.Change); err != nil {
		t.Fatal(err)
	}
	ref := resource.Ref{Kind: service.Kind, Scope: env, Name: "api"}
	if c, _ := docker["srv_B"].Inspect(t.Context(), service.Target(ref)); c != nil {
		t.Fatal("api runs on srv_B")
	}
	var pinned proto.Message
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), string(service.Kind), env, "api")
		if err != nil {
			t.Fatal(err)
		}
		pinned, err = k.Decode(t.Context(), tx, r.ID, service.Kind, r.Spec)
		return err
	})
	values, err := k.Values(t.Context(), "srv_A", ref, pinned)
	if err != nil || values["DATABASE_URL"] != "postgres://hunter2@db" {
		t.Errorf("values for srv_A = %v, %v", values, err)
	}
	if _, err := k.Values(t.Context(), "srv_B", ref, pinned); !errors.Is(err, secrets.ErrNotPlaced) {
		t.Errorf("values for srv_B, where api doesn't run: %v, want it refused", err)
	}
	other := &resourcev1.Service{Runs: api.Runs, Secrets: map[string]string{"STOLEN": "DB_URL"}}
	if _, err := k.Values(t.Context(), "srv_A", ref, other); !errors.Is(err, secrets.ErrNotPlaced) || !strings.Contains(err.Error(), "isn't the latest revision") {
		t.Errorf("values for a spec that isn't api's: %v, want it refused", err)
	}
}
