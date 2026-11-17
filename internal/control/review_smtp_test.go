// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/probe/smtptest"
	"github.com/dockwell/dockwell/internal/store"
)

// Moving an app from a mail server without TLS to one that takes STARTTLS tells the
// app to require STARTTLS, in its own words: the word for the old security isn't
// carried over, or the app would sign in wherever a server between strips STARTTLS.
func TestReviewSMTPAMoveTellsTheAppTheNewSecurity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	useCatalogDir(t, dir)
	raw, err := os.ReadFile(filepath.Join("testdata", "notes-mail.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	cat := catalogClient(b)
	c := changeClient(b, b.origin)
	srv := smtptest.New(t, false)
	apply(t, c, propose(t, c, "Add the mail relay", connectionEdit("relay", mailConnection(srv))))
	secure := mailConnection(srv)
	secure.DisplayName = "Secure"
	secure.Smtp.Host, secure.Smtp.Port, secure.Smtp.Security = "smtp.example.com", 587, resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS
	apply(t, c, propose(t, c, "Add the secure server", connectionEdit("secure", secure)))

	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "notes", Project: "notes", Needs: map[string]string{"mail": "connection:relay"}})
	if done := apply(t, c, installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Notes ended %v: %s", done.GetState(), done.GetError())
	}
	if env := notesEnv(t, e); !slices.Contains(env, "SMTP_REQUIRE_TLS=false") {
		t.Fatalf("on the relay, Notes runs with %v", env)
	}
	res, err := cat.SetCatalogNeed(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogNeedRequest{Id: "notes", Need: "mail", Choice: "connection:secure"}))
	if err != nil {
		t.Fatal(err)
	}
	if done := apply(t, c, res.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the move ended %v: %s", done.GetState(), done.GetError())
	}
	env := notesEnv(t, e)
	for _, want := range []string{"SMTP_HOST=smtp.example.com", "SMTP_PORT=587", "SMTP_REQUIRE_TLS=true", "SMTP_SECURE=false"} {
		if !slices.Contains(env, want) {
			t.Errorf("moved, Notes runs without %s: %v", want, env)
		}
	}
}

// An upgrade keeps what a person typed and left empty: no password appears where
// none was typed, and a transport typed as log stays log.
func TestReviewSMTPAnUpgradeKeepsEmptyAndOtherTypedValues(t *testing.T) {
	t.Parallel()
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
		"MAIL_TRANSPORT": "log", "SMTP_HOST": "relay.internal", "SMTP_PORT": "25", "SMTP_REQUIRE_TLS": "false",
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
	if done := apply(t, c, res.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("upgrading Notes ended %v: %s", done.GetState(), done.GetError())
	}
	after := notesEnv(t, e)
	for _, v := range before {
		if !slices.Contains(after, v) && !slices.Contains([]string{"PATH", "HOSTNAME"}, v) {
			t.Errorf("before %s, after the upgrade %v", v, after)
		}
	}
}

// An upgrade keeps a sender a person typed, though they typed no server.
func TestReviewSMTPAnUpgradeKeepsATypedSenderAlone(t *testing.T) {
	t.Parallel()
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
		"MAIL_TRANSPORT": "log", "MAIL_FROM": "notes@example.com",
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
	if done := apply(t, c, res.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("upgrading Notes ended %v: %s", done.GetState(), done.GetError())
	}
	after := notesEnv(t, e)
	for _, v := range before {
		if !slices.Contains(after, v) && !slices.Contains([]string{"PATH", "HOSTNAME"}, v) {
			t.Errorf("before %s, after the upgrade %v", v, after)
		}
	}
}

// An upgrade keeps a transport other than SMTP a person typed, and its sender.
func TestReviewSMTPAnUpgradeKeepsAnotherTransport(t *testing.T) {
	t.Parallel()
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
		"MAIL_TRANSPORT": "sendmail", "MAIL_FROM": "notes@example.com",
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
	if done := apply(t, c, res.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("upgrading Notes ended %v: %s", done.GetState(), done.GetError())
	}
	after := notesEnv(t, e)
	for _, v := range before {
		if !slices.Contains(after, v) && !slices.Contains([]string{"PATH", "HOSTNAME"}, v) {
			t.Errorf("before %s, after the upgrade %v", v, after)
		}
	}
}
