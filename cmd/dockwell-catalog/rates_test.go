// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog/convert"
)

// stubStores has rates convert each store to what runs says, by format, and its
// default branch be at head.
func stubStores(t *testing.T, runs map[string]convert.Run, head string) *[]string {
	t.Helper()
	var asked []string
	fetch, heads := fetchStore, headOf
	t.Cleanup(func() { fetchStore, headOf = fetch, heads })
	fetchStore = func(_ context.Context, store convert.Store, _, commit string) (convert.Run, error) {
		asked = append(asked, store.Format+"@"+commit)
		return runs[store.Format], nil
	}
	headOf = func(context.Context, string) (string, error) { return head, nil }
	return &asked
}

func writeRecord(t *testing.T, rec ratesRecord) string {
	t.Helper()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "rates.json")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func readRecord(t *testing.T, file string) ratesRecord {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var rec ratesRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// rates converts each store at the commit its record pins and says how many of its
// apps install, and why the rest are left out, counted by kind. It fails when an app
// that converted no longer does, naming it, and when the record doesn't say what
// converts now; --record writes it, and --pin moves each store to its default
// branch first.
func TestRatesHoldTheRecord(t *testing.T) {
	const commit = "1111111111111111111111111111111111111111"
	run := convert.Run{Results: []convert.Result{
		{ID: "notes", Entry: &catalogv1.Entry{Id: "notes"}},
		{ID: "board", Entry: &catalogv1.Entry{Id: "board"}},
		{ID: "wiki", LeftOut: "it ships conf/app.ini beside its Compose file", Class: convert.ClassShipped},
		{ID: "chat", LeftOut: "it runs scripts", Class: convert.ClassScripts},
		{ID: "mail", LeftOut: "it runs scripts too", Class: convert.ClassScripts},
	}}
	asked := stubStores(t, map[string]convert.Run{"coolify": run}, "2222222222222222222222222222222222222222")
	same := storeRates{
		Format: "coolify", Repository: "https://example.com/coolify", Commit: commit, Apps: 5,
		Converted: []string{"board", "notes"}, LeftOut: map[string]int{"scripts": 2, "shipped": 1},
	}

	var out bytes.Buffer
	if err := run2(t, writeRecord(t, ratesRecord{Stores: []storeRates{same}}), &out); err != nil {
		t.Fatalf("rates with a record that says what converts: %v", err)
	}
	for _, want := range []string{"Coolify's templates", "111111111111", "5", "2", "40%", "Coolify's templates leaves out 3:", "2  Runs scripts on the server (scripts)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the table doesn't say %q:\n%s", want, out.String())
		}
	}
	if !slices.Equal(*asked, []string{"coolify@" + commit}) {
		t.Errorf("it read %v", *asked)
	}

	fell := same
	fell.Converted = []string{"board", "notes", "wiki"}
	if err := run2(t, writeRecord(t, ratesRecord{Stores: []storeRates{fell}}), &out); err == nil || !strings.Contains(err.Error(), "Coolify's templates no longer converts wiki") {
		t.Errorf("an app that no longer converts: %v", err)
	}
	rose := same
	rose.Converted, rose.LeftOut = []string{"notes"}, map[string]int{"scripts": 2, "shipped": 2}
	if err := run2(t, writeRecord(t, ratesRecord{Stores: []storeRates{rose}}), &out); err == nil || !strings.Contains(err.Error(), "converts 2 of 5, where the record says 1 of 5. Record it") {
		t.Errorf("a record that's behind: %v", err)
	}

	file := writeRecord(t, ratesRecord{Stores: []storeRates{rose}})
	if err := rates(t.Context(), []string{"--file", file, "--record"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(t, file); len(got.Stores) != 1 || !slices.Equal(got.Stores[0].Converted, same.Converted) || got.Stores[0].LeftOut["scripts"] != 2 || got.Stores[0].Commit != commit {
		t.Errorf("--record wrote %+v", got)
	}
	if err := rates(t.Context(), []string{"--file", file, "--pin"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if got := readRecord(t, file); got.Stores[0].Commit != "2222222222222222222222222222222222222222" {
		t.Errorf("--pin left the store at %s", got.Stores[0].Commit)
	}
}

func run2(t *testing.T, file string, out *bytes.Buffer) error {
	t.Helper()
	out.Reset()
	return rates(t.Context(), []string{"--file", file}, out, out)
}

// The repository's record names each store the control plane converts, once, at a
// commit in full.
func TestTheRecordNamesEachStore(t *testing.T) {
	rec := readRecord(t, filepath.Join("..", "..", RatesFile))
	var formats []string
	for _, s := range rec.Stores {
		store, ok := convert.StoreOf(s.Format)
		switch {
		case !ok:
			t.Errorf("the record names a format the converter doesn't read: %q", s.Format)
		case s.Repository != store.Upstream:
			t.Errorf("%s is read at %s, not its own repository %s", s.Format, s.Repository, store.Upstream)
		case len(s.Commit) != 40:
			t.Errorf("%s's commit %q isn't one in full", s.Format, s.Commit)
		case s.Apps == 0 || len(s.Converted) == 0:
			t.Errorf("%s records no apps", s.Format)
		}
		formats = append(formats, s.Format)
	}
	for _, s := range convert.Stores {
		if !slices.Contains(formats, s.Format) {
			t.Errorf("the record has no %s", s.Format)
		}
	}
}
