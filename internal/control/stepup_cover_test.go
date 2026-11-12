// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

// choose has the browser's person choose how often they're asked for their
// passkey again, with one just now, as Profile does.
func choose(t *testing.T, b *browser, policy dockwellv1.StepUpPolicy) (*dockwellv1.StepUpChoice, error) {
	t.Helper()
	res, err := b.authClient().SetStepUpPolicy(t.Context(), connect.NewRequest(&dockwellv1.SetStepUpPolicyRequest{
		Policy: policy, Proof: stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_SET_STEP_UP_POLICY, ""),
	}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetChoice(), nil
}

// auditCount counts the audit log's events whose action is action, or ends
// with it when it starts with "_".
func auditCount(t *testing.T, f *fullServer, action string) int {
	t.Helper()
	var events []store.Event
	if err := f.store.View(t.Context(), func(tx *store.Tx) (err error) {
		events, err = tx.ListEvents(t.Context(), 10_000)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	n := 0
	for i := range events {
		if a := events[i].Action; a == action || strings.HasPrefix(action, "_") && strings.HasSuffix(a, action) {
			n++
		}
	}
	return n
}

// Someone who asks once an hour reveals a secret, and saves, deletes and makes
// what lets someone back onto a server, with no passkey, within the hour of
// one: the control plane answers for the session where the agent asks. Each is
// audited as confirmed so, none reads as unconfirmed, and no API token gets any
// of it, or reads or changes the choice.
func TestOnceAnHourThroughTheAPI(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	a := b.authClient()
	got, err := a.GetStepUpPolicy(t.Context(), connect.NewRequest(&dockwellv1.GetStepUpPolicyRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if c := got.Msg.GetChoice(); c.GetPolicy() != dockwellv1.StepUpPolicy_STEP_UP_POLICY_EACH_TIME ||
		c.GetFloor() != dockwellv1.StepUpPolicy_STEP_UP_POLICY_AT_SIGN_IN || c.GetCoverTime() != nil {
		t.Fatalf("a new person's choice = %v", c)
	}
	c := changeClient(b, b.origin)
	apply(t, c, propose(t, c, "Add the database password", secretSpec("DB_PASSWORD", "s3cret-reveal-me")))
	v := variablesClient(b, b.origin)
	secret := listed(t, v, "DB_PASSWORD")
	reveal := func() error {
		_, err := v.RevealSecret(t.Context(), connect.NewRequest(&dockwellv1.RevealSecretRequest{Id: secret.GetId()}))
		return err
	}
	if err := reveal(); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Fatalf("revealing with no passkey, each time: %v", err)
	}

	choice, err := choose(t, b, dockwellv1.StepUpPolicy_STEP_UP_POLICY_HOURLY)
	if err != nil {
		t.Fatal(err)
	}
	if choice.GetEffective() != dockwellv1.StepUpPolicy_STEP_UP_POLICY_HOURLY || choice.GetCoverAtSignIn() ||
		choice.GetCoverExpireTime().AsTime().Sub(choice.GetCoverTime().AsTime()) != time.Hour {
		t.Fatalf("after choosing once an hour: %v", choice)
	}
	if err := reveal(); err != nil {
		t.Fatalf("revealing within the hour: %v", err)
	}

	id := f.addServer(t, false).id.ServerID
	files := fileClient(b, b.origin)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(dir, ".ssh", "authorized_keys")
	writeFile(t, keys, "ssh-ed25519 AAAA admin\n")
	via := filepath.Join(dir, "config.yml")
	if err := os.Symlink(keys, via); err != nil {
		t.Fatal(err)
	}
	read, err := files.ReadFile(t.Context(), connect.NewRequest(&dockwellv1.ReadFileRequest{ServerId: id, Path: via}))
	if err != nil || !read.Msg.GetWayBackIn() {
		t.Fatalf("reading through the link: %v, %v", read, err)
	}
	// Nothing confirmed and no proof: the agent asks, and the session answers.
	if _, err := files.SaveFile(t.Context(), connect.NewRequest(&dockwellv1.SaveFileRequest{
		ServerId: id, Path: via, Content: "typed\n", Version: read.Msg.GetVersion(),
	})); err != nil {
		t.Fatalf("saving a way back in within the hour: %v", err)
	}
	if got, err := os.ReadFile(keys); err != nil || string(got) != "typed\n" {
		t.Fatalf("the file holds %q, %v", got, err)
	}
	gone, err := files.DeleteFile(t.Context(), connect.NewRequest(&dockwellv1.DeleteFileRequest{ServerId: id, Path: keys}))
	if err != nil || !gone.Msg.GetWayBackIn() {
		t.Fatalf("deleting a way back in within the hour: %v, %v", gone, err)
	}
	more := filepath.Join(dir, ".ssh", "authorized_keys2")
	if _, err := files.MakeFile(t.Context(), connect.NewRequest(&dockwellv1.MakeFileRequest{ServerId: id, Path: more, Type: dockwellv1.FileType_FILE_TYPE_FILE})); err != nil {
		t.Fatalf("making a way back in within the hour: %v", err)
	}
	waitForFileEvent(t, b, id, "file.make", more)
	if n := auditCount(t, f, "auth.step_up_hour"); n != 4 {
		t.Errorf("%d covers on the audit log, want the reveal, the save, the delete and the make", n)
	}
	if n := auditCount(t, f, "_unconfirmed"); n != 0 {
		t.Errorf("%d unconfirmed events, for what the session confirmed", n)
	}
	if n := auditCount(t, f, "secret.reveal"); n != 1 {
		t.Errorf("%d reveals on record, want 1", n)
	}

	token := map[string]string{"Authorization": "Bearer " + createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)}
	for _, r := range []struct{ verb, path, body string }{
		{http.MethodGet, "/v1/stepUpPolicy", ""},
		{http.MethodPost, "/v1/stepUpPolicy:set", `{"policy":"STEP_UP_POLICY_AT_SIGN_IN"}`},
		{http.MethodPost, "/v1/secrets/" + secret.GetId() + ":reveal", `{}`},
		{http.MethodPost, "/v1/settings:setStepUpFloor", `{"floor":"STEP_UP_POLICY_AT_SIGN_IN"}`},
	} {
		if code, reason, _ := callREST(t, b, r.verb, r.path, token, r.body); code != http.StatusForbidden || reason != authapi.ReasonNeedsSession {
			t.Errorf("a token: %s %s = %d %s, want needs a session", r.verb, r.path, code, reason)
		}
	}
}

// The owner sets the least anyone may choose, with a passkey each time. What
// applies is the stricter, a choice below it is refused, and an admin can't set
// it. It's on the activity log, and GetSettings says it.
func TestTheOwnerSetsTheFloor(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	s := dockwellv1connect.NewSettingsServiceClient(b, b.origin, connect.WithProtoJSON())
	read := func() dockwellv1.StepUpPolicy {
		t.Helper()
		res, err := s.GetSettings(t.Context(), connect.NewRequest(&dockwellv1.GetSettingsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetSettings().GetStepUpFloor()
	}
	if got := read(); got != dockwellv1.StepUpPolicy_STEP_UP_POLICY_AT_SIGN_IN {
		t.Fatalf("the floor of a new install = %v, want all three allowed", got)
	}
	set := func(floor dockwellv1.StepUpPolicy, proof string) error {
		_, err := s.SetStepUpFloor(t.Context(), connect.NewRequest(&dockwellv1.SetStepUpFloorRequest{Floor: floor, Proof: proof}))
		return err
	}
	if c, err := choose(t, b, dockwellv1.StepUpPolicy_STEP_UP_POLICY_AT_SIGN_IN); err != nil || !c.GetCoverAtSignIn() {
		t.Fatalf("choosing only when signing in: %v, %v, want the sign-in's cover", c, err)
	}
	if err := set(dockwellv1.StepUpPolicy_STEP_UP_POLICY_UNSPECIFIED, ""); reasonOf(err) != "invalid" {
		t.Errorf("no floor: %v, want invalid", err)
	}
	if err := set(dockwellv1.StepUpPolicy_STEP_UP_POLICY_AT_SIGN_IN, ""); err != nil {
		t.Errorf("setting the floor it has, with no passkey: %v, want nothing to change", err)
	}
	if err := set(dockwellv1.StepUpPolicy_STEP_UP_POLICY_EACH_TIME, ""); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Fatalf("setting the floor with no passkey, though signed in covers the rest: %v", err)
	}
	if err := set(dockwellv1.StepUpPolicy_STEP_UP_POLICY_EACH_TIME, stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_SET_STEP_UP_FLOOR, "")); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != dockwellv1.StepUpPolicy_STEP_UP_POLICY_EACH_TIME {
		t.Errorf("the floor = %v, want each time", got)
	}
	got, err := b.authClient().GetStepUpPolicy(t.Context(), connect.NewRequest(&dockwellv1.GetStepUpPolicyRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if c := got.Msg.GetChoice(); c.GetPolicy() != dockwellv1.StepUpPolicy_STEP_UP_POLICY_AT_SIGN_IN ||
		c.GetEffective() != dockwellv1.StepUpPolicy_STEP_UP_POLICY_EACH_TIME || c.GetCoverTime() != nil {
		t.Errorf("under a floor of each time: %v", c)
	}
	if _, err := choose(t, b, dockwellv1.StepUpPolicy_STEP_UP_POLICY_HOURLY); reasonOf(err) != "below_floor" {
		t.Errorf("choosing below the floor: %v, want below_floor", err)
	}
	if n := auditCount(t, f, "settings.step_up_floor"); n != 1 {
		t.Errorf("%d floor events, want 1", n)
	}

	// Only the owner: it's every member's.
	hold := aMember(t, f)
	hold(onInstall(store.RoleAdmin))
	if err := set(dockwellv1.StepUpPolicy_STEP_UP_POLICY_AT_SIGN_IN, stepUpFor(t, b, dockwellv1.StepUpAction_STEP_UP_ACTION_SET_STEP_UP_FLOOR, "")); reasonOf(err) != authapi.ReasonNeedsRole || !strings.Contains(err.Error(), "only the owner") {
		t.Errorf("an admin setting the floor: %v, want only the owner", err)
	}
}

// Once an hour, a server's terminal opens with no passkey within the hour of
// one, and so does a shell in a container the agent finds as good as root,
// which opens again confirmed once the agent asks. Each is audited as confirmed
// so, and neither reads as unconfirmed.
func TestOnceAnHourOpensTerminals(t *testing.T) {
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addServerWith(t, nil)
	id := f.lastDocker.id.ServerID
	e.AddLocalImage("acme/app:1", dockertest.Image{Shell: "/bin/sh"})
	e.AddContainer(docker.Container{Name: "/admin-1", Config: docker.Config{Image: "acme/app:1"}, HostConfig: docker.HostConfig{Privileged: true}})
	c := terminalClient(b, b.origin)
	open := func(container string) error {
		_, err := c.OpenTerminal(t.Context(), connect.NewRequest(&dockwellv1.OpenTerminalRequest{ServerId: id, Container: container, Columns: 80, Rows: 24}))
		return err
	}
	if err := open("admin-1"); reasonOf(err) != authapi.ReasonNeedsStepUp {
		t.Fatalf("a privileged container's shell, each time, with no passkey: %v", err)
	}
	if _, err := choose(t, b, dockwellv1.StepUpPolicy_STEP_UP_POLICY_HOURLY); err != nil {
		t.Fatal(err)
	}
	if err := open(""); err != nil {
		t.Fatalf("a server's terminal within the hour: %v", err)
	}
	if err := open("admin-1"); err != nil {
		t.Fatalf("a privileged container's shell within the hour: %v", err)
	}
	if n := auditCount(t, f, "auth.step_up_hour"); n != 2 {
		t.Errorf("%d covers on the audit log, want the terminal and the shell", n)
	}
}
