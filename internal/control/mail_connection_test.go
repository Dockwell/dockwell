// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/catalogapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/probe"
	"github.com/dockwell/dockwell/internal/probe/smtptest"
	"github.com/dockwell/dockwell/internal/store"
)

const mailSecret = "the-mail-servers-password"

// mailConnection is a mail server's connection on this machine's loopback, where an
// app's network may go without TLS, signing in as notes@example.com.
func mailConnection(srv *smtptest.Server) *resourcev1.ConnectionSpec {
	return &resourcev1.ConnectionSpec{
		DisplayName: "Mail relay", Kind: resourcev1.LinkKind_LINK_KIND_SMTP, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		Smtp: &resourcev1.ConnectionSmtp{
			Host: srv.Host, Port: uint32(srv.Port), Security: resourcev1.SmtpSecurity_SMTP_SECURITY_NONE,
			User: "notes@example.com", Password: mailSecret, Sender: "notes@example.com", SenderName: "Notes",
		},
	}
}

// notesEnv is the environment of the newest container of Notes.
func notesEnv(t *testing.T, e *dockertest.Engine) []string {
	t.Helper()
	for _, c := range slices.Backward(e.Containers()) {
		if strings.Contains(c, "notes") {
			got, err := e.Client().InspectContainer(t.Context(), c)
			if err != nil {
				t.Fatal(err)
			}
			return got.Config.Env
		}
	}
	t.Fatal("Notes doesn't run")
	return nil
}

// An install sends mail through a connection to a mail server: the sheet offers it
// with who it sends as, the install links the app to it, each name the need sets
// reading the connection through the link, the user and password as secrets, and
// how it's secured in the app's own words. The app's link may say a sender of its
// own, which reaches the app as a change; the connection's security can't change
// under it; and the connection lists where it is and who it sends as, never who
// signs in (ADR 0099).
func TestAnInstallSendsMailThroughAConnection(t *testing.T) {
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

	conns := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	list, err := conns.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := protojson.Marshal(list.Msg); strings.Contains(string(raw), mailSecret) || strings.Contains(string(raw), `"notes@example.com"`) && !strings.Contains(string(raw), `"sender":"notes@example.com"`) {
		t.Errorf("the list holds who signs in: %s", raw)
	}
	if got := list.Msg.GetConnections()[0]; got.GetHost() != srv.Host || got.GetPort() != uint32(srv.Port) ||
		got.GetSecurity() != resourcev1.SmtpSecurity_SMTP_SECURITY_NONE || got.GetSender() != "notes@example.com" || got.GetSenderName() != "Notes" {
		t.Errorf("the relay lists as %v", got)
	}

	got, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "notes"}))
	if err != nil {
		t.Fatal(err)
	}
	var mail []string
	for _, o := range got.Msg.GetNeeds() {
		if o.GetNeed() != "mail" {
			continue
		}
		mail = append(mail, o.GetChoice())
		if o.GetChoice() == "connection:relay" && (o.GetConnectionLabel() != "Mail relay" || o.GetConnectionSender() != "notes@example.com" || o.GetWhy() != "") {
			t.Errorf("the sheet offers the relay as %v", o)
		}
	}
	if !slices.Equal(mail, []string{"unset", "connection:relay"}) {
		t.Fatalf("the sheet offers mail as %v", mail)
	}

	installed := installApp(t, cat, &dockwellv1.InstallCatalogEntryRequest{Id: "notes", Project: "notes", Needs: map[string]string{"mail": "connection:relay"}})
	if raw, _ := protojson.Marshal(installed); strings.Contains(string(raw), mailSecret) {
		t.Error("the install's plan holds the password")
	}
	if done := apply(t, c, installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Notes ended %v: %s", done.GetState(), done.GetError())
	}
	env := notesEnv(t, e)
	for _, want := range []string{
		"MAIL_TRANSPORT=smtp", "SMTP_HOST=" + srv.Host, "SMTP_PORT=" + strconv.Itoa(srv.Port), "SMTP_SECURE=false", "SMTP_REQUIRE_TLS=false",
		"SMTP_USER=notes@example.com", "SMTP_PASSWORD=" + mailSecret, "MAIL_FROM=notes@example.com",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("Notes runs without %s: %v", want, env)
		}
	}
	if len(srv.Commands()) != 0 {
		t.Errorf("installing Notes reached the mail server: %v", srv.Commands())
	}

	// Its link says a sender of its own, which reaches Notes.
	envID := installedEnv(t, cat, "notes")
	var l resourcev1.Link
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), "link", envID, "mail")
		if err != nil {
			return err
		}
		return proto.Unmarshal(r.Spec, &l)
	}); err != nil {
		t.Fatal(err)
	}
	if l.GetKind() != resourcev1.LinkKind_LINK_KIND_SMTP || l.GetConnection() != "relay" || l.GetSender() != "" || len(l.GetSets()) != 7 {
		t.Errorf("the install links mail as %v", &l)
	}
	l.Sender, l.SenderName = "blog@example.com", "Blog"
	edit := &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "link", Scope: envID, Name: "mail"}, Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &l}}}
	if done := apply(t, c, propose(t, c, "Send Notes's mail as the blog", edit)); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the sender's change ended %v: %s", done.GetState(), done.GetError())
	}
	if env := notesEnv(t, e); !slices.Contains(env, "MAIL_FROM=blog@example.com") {
		t.Errorf("with its link's sender, Notes runs with %v", env)
	}

	// The relay's security can't change under Notes, which was told it in its words.
	moved := mailConnection(srv)
	moved.Smtp.Security, moved.Smtp.Port = resourcev1.SmtpSecurity_SMTP_SECURITY_TLS, 465
	if _, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Use TLS", Edits: []*dockwellv1.Edit{connectionEdit("relay", moved)}})); err == nil ||
		!strings.Contains(err.Error(), "tells its app how the connection relay is secured") {
		t.Errorf("moving the relay to TLS under Notes: %v", err)
	}
	// Its password changes, reaching Notes in the same plan.
	again := mailConnection(srv)
	again.Smtp.Password = "a-new-password"
	if done := apply(t, c, propose(t, c, "New password", connectionEdit("relay", again))); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the new password ended %v: %s", done.GetState(), done.GetError())
	}
	if env := notesEnv(t, e); !slices.Contains(env, "SMTP_PASSWORD=a-new-password") {
		t.Errorf("with a new password, Notes runs with %v", env)
	}

	// Checked from a network of the check's own, the relay on this server's loopback
	// is refused before anything connects, and nothing is sent.
	checked, err := conns.CheckConnection(t.Context(), connect.NewRequest(&dockwellv1.CheckConnectionRequest{Name: "relay"}))
	if err != nil {
		t.Fatal(err)
	}
	if checked.Msg.GetPassed() || checked.Msg.GetStep() != probe.StepConnect || checked.Msg.GetCode() != probe.CodeRefused {
		t.Errorf("the relay checks as %v", checked.Msg)
	}
	if _, err := conns.CheckConnection(t.Context(), connect.NewRequest(&dockwellv1.CheckConnectionRequest{Name: "relay", Prefix: "notes/"})); err == nil {
		t.Error("a mail server's check took a prefix")
	}
	if slices.ContainsFunc(srv.Commands(), func(c string) bool { return c == "MAIL" || c == "RCPT" || c == "DATA" }) {
		t.Errorf("the mail server was asked to send: %v", srv.Commands())
	}
}

// From an app's network, a mail server's check signs in and sends nothing.
func TestAMailServerIsCheckedThroughItsAgent(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	e := f.addDockerServer(t)
	const env = "env_01jwd8r6q4wexz3c6v0wzm5k8h"
	project := docker.Project(env)
	e.AddNetwork(docker.Network{Name: project + "_default", IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "127.0.0.0/8"}}}})
	e.AddLocalImage("ghcr.io/acme/notes:1", dockertest.Image{Config: docker.Config{Cmd: []string{"notes"}}})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), project+"-notes-1", &docker.CreateRequest{
		Config: docker.Config{Image: "ghcr.io/acme/notes:1", Labels: map[string]string{
			docker.LabelProject: project, docker.LabelService: "notes", docker.LabelNumber: "1",
		}},
		HostConfig: docker.HostConfig{NetworkMode: project + "_default"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	session, ok := f.agents.Session(f.lastDocker.id.ServerID)
	if !ok {
		t.Fatal("no agent")
	}
	srv := smtptest.New(t, false)
	srv.Set(func(s *smtptest.Server) { s.StartTLS, s.Plain = false, true })
	req := probe.Request{Kind: probe.KindSMTP, SMTP: &probe.SMTP{Host: srv.Host, Port: srv.Port, Security: probe.SecurityNone, User: "notes@example.com", Password: mailSecret}}
	got, err := session.ProbeConnection(t.Context(), env, "notes", req)
	if err != nil {
		t.Fatal(err)
	}
	if !got.GetPassed() || !slices.Equal(srv.SignedIn(), []string{"notes@example.com:" + mailSecret}) {
		t.Errorf("the relay, from the app's network, checks as %v, signed in %v", got, srv.SignedIn())
	}
	if slices.ContainsFunc(srv.Commands(), func(c string) bool { return c == "MAIL" || c == "RCPT" || c == "DATA" }) {
		t.Errorf("the check asked to send: %v", srv.Commands())
	}
}

// An upgrade to a release that takes as a need what a person typed as inputs keeps
// what they typed, under the same names, as the app's own: the need reads "set by
// hand", and nothing a person gave is dropped (ADR 0099, #754).
func TestAnUpgradeKeepsTypedMailSettings(t *testing.T) {
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
		"MAIL_TRANSPORT": "smtp", "SMTP_HOST": "smtp.example.com", "SMTP_PORT": "587", "SMTP_USER": "notes@example.com",
		"SMTP_PASSWORD": mailSecret, "MAIL_FROM": "notes@example.com",
	}})
	if done := apply(t, c, installed); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("installing Notes ended %v: %s", done.GetState(), done.GetError())
	}
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
	if notes := notesOf(res.Msg.GetChange(), catalogapi.TypedKeptNote); len(notes) != 1 || !strings.Contains(notes[0], "keeps the mail settings you typed") {
		t.Errorf("the upgrade's plan says %v", notes)
	}
	if done := apply(t, c, res.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("upgrading Notes ended %v: %s", done.GetState(), done.GetError())
	}
	env := notesEnv(t, e)
	for _, want := range []string{
		"MAIL_TRANSPORT=smtp", "SMTP_HOST=smtp.example.com", "SMTP_PORT=587", "SMTP_SECURE=false", "SMTP_REQUIRE_TLS=true",
		"SMTP_USER=notes@example.com", "SMTP_PASSWORD=" + mailSecret, "MAIL_FROM=notes@example.com",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("upgraded, Notes runs without %s: %v", want, env)
		}
	}
	list, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range list.Msg.GetInstalls() {
		if p := in.GetPin().GetNeeds()["mail"]; in.GetProject() == "notes" && !p.GetOwn() {
			t.Errorf("the install pins mail as %v", p)
		}
	}
}

// installedEnv is the environment an install of project runs in, by ID.
func installedEnv(t *testing.T, cat dockwellv1connect.CatalogServiceClient, project string) string {
	t.Helper()
	list, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range list.Msg.GetInstalls() {
		if in.GetProject() == project {
			return in.GetEnvironmentId()
		}
	}
	t.Fatalf("there's no install of %s", project)
	return ""
}
