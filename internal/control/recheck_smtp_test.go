// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/catalogapi"
	"github.com/dockwell/dockwell/internal/store"
)

// A sender release 1 gave by default, which no one typed, isn't a typed setting.
func TestRecheckSMTPADefaultSenderIsNotTyped(t *testing.T) {
	dir := t.TempDir()
	useCatalogDir(t, dir)
	raw, err := os.ReadFile(filepath.Join("testdata", "notes-mail-1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Release 1 gives its sender by default, as many entries give a sender's name.
	first := strings.Replace(string(raw), "{ name: MAIL_FROM, label: Sender, type: INPUT_TYPE_TEXT, optional: true }",
		`{ name: MAIL_FROM, label: Sender, type: INPUT_TYPE_TEXT, defaultValue: "notes@localhost" }`, 1)
	if err := os.WriteFile(filepath.Join(dir, "notes.yaml"), []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	cat := catalogClient(b)
	c := changeClient(b, b.origin)
	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "notes", Project: "notes"})
	if done := apply(t, c, installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Notes ended %v: %s", done.GetState(), done.GetError())
	}
	t.Logf("before: %v", notesEnv(t, e))
	next, err := os.ReadFile(filepath.Join("testdata", "notes-mail.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.yaml"), next, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "notes", SkipBackup: true}))
	if err != nil {
		t.Fatal(err)
	}
	if notes := notesOf(res.Msg.GetChange(), catalogapi.TypedKeptNote); len(notes) != 0 {
		t.Errorf("no one typed a mail setting, but the plan says %q", notes)
	}
	if done := apply(t, c, res.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("upgrading Notes ended %v: %s", done.GetState(), done.GetError())
	}
	t.Logf("after: %v", notesEnv(t, e))
	list, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range list.Msg.GetInstalls() {
		if p := in.GetPin().GetNeeds()["mail"]; in.GetProject() == "notes" && p.GetOwn() {
			t.Errorf("the install pins mail as set by hand: %v", p)
		}
	}
}

// What a person typed in parts a release also gives by default stays, alone too.
func TestRecheckSMTPTypedTransportAndPortAlone(t *testing.T) {
	dir := t.TempDir()
	useCatalogDir(t, dir)
	first, err := os.ReadFile(filepath.Join("testdata", "notes-mail-1.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.yaml"), first, 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	cat := catalogClient(b)
	c := changeClient(b, b.origin)
	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "notes", Project: "notes", Values: map[string]string{
		"MAIL_TRANSPORT": "sendmail", "SMTP_PORT": "2525", "SMTP_SECURE": "true", "SMTP_REQUIRE_TLS": "false",
	}})
	if done := apply(t, c, installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Notes ended %v: %s", done.GetState(), done.GetError())
	}
	before := notesEnv(t, e)
	next, err := os.ReadFile(filepath.Join("testdata", "notes-mail.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.yaml"), next, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&dockwellv1.UpgradeCatalogInstallRequest{Id: "notes", SkipBackup: true}))
	if err != nil {
		t.Fatal(err)
	}
	// They were typed, so they're kept, and the plan says so (the re-check's N3).
	if notes := notesOf(res.Msg.GetChange(), catalogapi.TypedKeptNote); len(notes) != 1 || !strings.Contains(notes[0], "you typed") {
		t.Errorf("the upgrade's plan says %q", notes)
	}
	if done := apply(t, c, res.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("upgrading Notes ended %v: %s", done.GetState(), done.GetError())
	}
	after := notesEnv(t, e)
	for _, v := range before {
		if !slices.Contains(after, v) && !strings.HasPrefix(v, "PATH=") && !strings.HasPrefix(v, "HOSTNAME=") {
			t.Errorf("before %s, after the upgrade %v", v, after)
		}
	}
}
