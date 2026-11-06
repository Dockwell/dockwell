// SPDX-License-Identifier: AGPL-3.0-only

package secrets_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
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

// secretAt is where revision 1 of a secret keeps its spec.
func secretAt(resourceID string) store.SpecAt {
	return store.InRevision(resourceID, string(variable.SecretKind), 1)
}

// sealIn seals revision 1 of a secret's spec.
func sealIn(t *testing.T, db *store.DB, k *secrets.Keeper, resourceID string, plain []byte) []byte {
	t.Helper()
	return sealAt(t, db, k, secretAt(resourceID), plain)
}

func sealAt(t *testing.T, db *store.DB, k *secrets.Keeper, at store.SpecAt, plain []byte) []byte {
	t.Helper()
	var out []byte
	if err := db.Update(t.Context(), func(tx *store.Tx) (err error) {
		out, err = k.Seal(t.Context(), tx, at, plain)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func openIn(t *testing.T, db *store.DB, k *secrets.Keeper, at store.SpecAt, sealed []byte) ([]byte, error) {
	t.Helper()
	var out []byte
	err := db.View(t.Context(), func(tx *store.Tx) (err error) {
		out, err = k.Open(t.Context(), tx, at, sealed)
		return err
	})
	return out, err
}

// A sealed spec holds nothing of the spec, and opens only where it was
// sealed for: that resource and kind, and that revision of it, or that item
// of that change, under this install's key, and not once changed. A spec of
// a kind that may hold secrets that isn't sealed is refused.
func TestSealAndOpen(t *testing.T) { storetest.Each(t, testSealAndOpen) }

func testSealAndOpen(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	k := secretstest.Keeper(db, kinds())
	plain := []byte("value: hunter2 hunter2")
	sealed := sealIn(t, db, k, "sec_A", plain)
	if !secrets.Sealed(sealed) || bytes.Contains(sealed, []byte("hunter2")) {
		t.Fatalf("a sealed spec: %q", sealed)
	}
	if got, err := openIn(t, db, k, secretAt("sec_A"), sealed); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("Open = %q, %v", got, err)
	}
	again := sealIn(t, db, k, "sec_A", plain)
	if bytes.Equal(again, sealed) {
		t.Error("sealing the same spec twice gave the same bytes")
	}
	item := store.InChange("sec_A", string(variable.SecretKind), "chg_1", 0)
	inItem := sealAt(t, db, k, item, plain)
	if got, err := openIn(t, db, k, item, inItem); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("Open of a change item's = %q, %v", got, err)
	}
	secret := string(variable.SecretKind)
	for what, try := range map[string]func() ([]byte, error){
		"for another resource": func() ([]byte, error) { return openIn(t, db, k, secretAt("sec_B"), sealed) },
		"as another kind": func() ([]byte, error) {
			return openIn(t, db, k, store.InRevision("sec_A", string(variable.VariableKind), 1), sealed)
		},
		"as another revision": func() ([]byte, error) { return openIn(t, db, k, store.InRevision("sec_A", secret, 2), sealed) },
		"as a change item":    func() ([]byte, error) { return openIn(t, db, k, store.InChange("sec_A", secret, "chg_1", 1), sealed) },
		"as a revision, from a change item": func() ([]byte, error) {
			return openIn(t, db, k, secretAt("sec_A"), inItem)
		},
		"as another item": func() ([]byte, error) { return openIn(t, db, k, store.InChange("sec_A", secret, "chg_1", 1), inItem) },
		"as another change's item": func() ([]byte, error) {
			return openIn(t, db, k, store.InChange("sec_A", secret, "chg_2", 0), inItem)
		},
		"changed": func() ([]byte, error) {
			bad := bytes.Clone(sealed)
			bad[len(bad)-1] ^= 1
			return openIn(t, db, k, secretAt("sec_A"), bad)
		},
		"cut short": func() ([]byte, error) { return openIn(t, db, k, secretAt("sec_A"), sealed[:8]) },
		"under another install's key": func() ([]byte, error) {
			return openIn(t, db, secrets.New(db, otherKey(t), kinds()), secretAt("sec_A"), sealed)
		},
		// In the clear, as someone with the database and not the key file
		// might write one of their own.
		"for a kind that may hold secrets, not sealed": func() ([]byte, error) {
			return openIn(t, db, k, store.InRevision("res_A", string(container.Kind), 1), []byte{0x0a, 0x01, 'x'})
		},
	} {
		if got, err := try(); err == nil || got != nil {
			t.Errorf("opening a sealed spec %s = %q, %v; want it refused", what, got, err)
		}
	}
	// A spec of a kind that holds no secret isn't sealed, and comes back as
	// it is.
	if got, err := openIn(t, db, k, store.InRevision("res_A", string(firewall.Kind), 1), []byte{0x18, 0x16}); err != nil || !bytes.Equal(got, []byte{0x18, 0x16}) {
		t.Errorf("an unsealed firewall rule opened as %q, %v", got, err)
	}
}

// A digest is keyed and bound to its change: the same spec digests the
// same under one install's key for one change, and differently under
// another's key or for another change, and isn't its plain hash.
func TestDigestIsKeyed(t *testing.T) {
	db, _ := storetest.Open(t, store.SQLite)
	a, b := secretstest.Keeper(db, kinds()), secrets.New(db, otherKey(t), kinds())
	spec := []byte("password=hunter2")
	plain := sha256.Sum256(spec)
	switch {
	case !bytes.Equal(a.Digest("chg_1", spec), secretstest.Keeper(db, kinds()).Digest("chg_1", spec)):
		t.Error("one key digested one spec two ways")
	case bytes.Equal(a.Digest("chg_1", spec), b.Digest("chg_1", spec)):
		t.Error("two keys digested a spec the same")
	case bytes.Equal(a.Digest("chg_1", spec), a.Digest("chg_2", spec)):
		t.Error("two changes digested a spec the same, so their fingerprints can confirm they set one value")
	case bytes.Equal(a.Digest("chg_1", spec), plain[:]):
		t.Error("the digest is the spec's plain hash")
	}
}

// A control plane that keeps running beside dockwell rotate-key holds no
// retired key-encryption key: a data key still wrapped under the old one,
// as in a backup taken before, opens nothing in it once another process
// retires that key, though it opened just before.
func TestARetiredKeyOpensNothingInARunningControlPlane(t *testing.T) {
	storetest.Each(t, testARetiredKeyOpensNothingInARunningControlPlane)
}

func testARetiredKeyOpensNothingInARunningControlPlane(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	running := secretstest.Keeper(db, kinds())
	sealed := sealIn(t, db, running, "sec_1", []byte("value"))
	if got, err := openIn(t, db, running, secretAt("sec_1"), sealed); err != nil || string(got) != "value" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	var before []store.DataKey
	_ = db.View(t.Context(), func(tx *store.Tx) error {
		old, err := tx.ActiveKEK(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		before, err = tx.DataKeysUnder(t.Context(), old.ID, "", 10)
		return err
	})
	if len(before) != 1 {
		t.Fatalf("data keys = %+v, want one", before)
	}
	// dockwell rotate-key, beside it.
	if _, _, err := secretstest.Keeper(db, kinds()).RotateKEK(t.Context(), "cli:mira@hz-1", store.SurfaceCLI); err != nil {
		t.Fatal(err)
	}
	if got, err := openIn(t, db, running, secretAt("sec_1"), sealed); err != nil || string(got) != "value" {
		t.Errorf("after rotating, Open = %q, %v", got, err)
	}
	// The data key as a backup from before has it, wrapped under the
	// retired key.
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		return tx.RewrapDataKey(t.Context(), before[0].ID, before[0].KEKID, before[0].Wrapped)
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := openIn(t, db, running, secretAt("sec_1"), sealed); err == nil {
		t.Errorf("the running control plane opened a spec with the retired key: %q", got)
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
			if got, err := openIn(t, db, keeper, secretAt(id), s); err != nil || string(got) != "value of "+id {
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
	if got, err := openIn(t, db, fresh, secretAt("sec_4"), s); err != nil || string(got) != "four" {
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
		pinned, err = k.Decode(t.Context(), tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
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

// A list opens many specs, so it can't read a key for each.
func TestOpenAllReadsEachKeyOnce(t *testing.T) { storetest.Each(t, testOpenAllReadsEachKeyOnce) }

func testOpenAllReadsEachKeyOnce(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	k := secretstest.Keeper(db, kinds())
	read := func(specs []store.StoredSpec) ([][]byte, int64, error) {
		t.Helper()
		var n atomic.Int64
		ctx := store.CountingQueries(t.Context(), &n)
		var out [][]byte
		err := db.View(ctx, func(tx *store.Tx) (err error) {
			out, err = k.OpenAll(ctx, tx, specs)
			return err
		})
		return out, n.Load(), err
	}
	var specs []store.StoredSpec
	for i := range 12 {
		name := fmt.Sprintf("sec_%d", i)
		specs = append(specs, store.StoredSpec{At: secretAt(name), Spec: sealIn(t, db, k, name, []byte("value "+name))})
	}
	got, many, err := read(specs)
	if err != nil {
		t.Fatal(err)
	}
	for i := range specs {
		if want := fmt.Sprintf("value sec_%d", i); string(got[i]) != want {
			t.Errorf("spec %d opened as %q, want %q", i, got[i], want)
		}
	}
	if _, one, err := read(specs[:1]); err != nil || many != one {
		t.Errorf("OpenAll takes %d queries for 12 specs and %d for one, %v; want the same", many, one, err)
	}
	// None is a no-op, and a spec's seal still binds it to its place.
	if got, _, err := read(nil); err != nil || len(got) != 0 {
		t.Errorf("OpenAll of nothing = %v, %v", got, err)
	}
	wrong := []store.StoredSpec{{At: secretAt("sec_0"), Spec: specs[1].Spec}}
	if _, _, err := read(wrong); !errors.Is(err, secrets.ErrOpen) {
		t.Errorf("a spec sealed for another = %v, want ErrOpen", err)
	} else if !strings.Contains(err.Error(), "Restore the database and the key file from the same backup") {
		t.Errorf("a list's spec sealed for another = %v, want it to say what to do", err)
	}
	if _, _, err := read([]store.StoredSpec{{At: secretAt("sec_0"), Spec: []byte("plain")}}); !errors.Is(err, secrets.ErrUnsealed) {
		t.Errorf("a spec that should be sealed and isn't = %v, want ErrUnsealed", err)
	} else if !strings.Contains(err.Error(), "sec_0") || !strings.Contains(err.Error(), "Restore the database from a backup") {
		t.Errorf("a list's spec that should be sealed and isn't = %v, want it to name the row and say what to do", err)
	}
}
