// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/terminal"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/terminals"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestTerminalWorkingDirectory(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testTerminalWorkingDirectory)
}

func testTerminalWorkingDirectory(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addServerWith(t, nil)
	srv := f.lastDocker
	id := srv.id.ServerID
	c := terminalClient(b, b.origin)

	// Host shell reports its live cwd after cd.
	term := openTerminal(t, b, id)
	a := attachTo(t, b, term.GetId(), 0)
	dir := t.TempDir()
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	a.write("cd " + realDir + " && echo cwd-$((1+1))\n")
	a.waitFor("cwd-2")

	got, err := c.GetTerminalWorkingDirectory(t.Context(), connect.NewRequest(&dockwellv1.GetTerminalWorkingDirectoryRequest{
		TerminalId: term.GetId(),
	}))
	if err != nil {
		t.Fatalf("GetTerminalWorkingDirectory: %v", err)
	}
	if !got.Msg.GetKnown() || got.Msg.GetPath() != realDir || !strings.HasPrefix(got.Msg.GetHome(), "/") {
		t.Errorf("host cwd = %+v, want known path %q with home fallback", got.Msg, realDir)
	}

	// Container shell reports neither cwd nor host home so Files never silently opens the host's /root.
	e.AddLocalImage("acme/app:1", dockertest.Image{Shell: "/bin/sh"})
	e.AddContainer(docker.Container{Name: "/web-1", Config: docker.Config{Image: "acme/app:1"}})
	ctrRes, err := c.OpenTerminal(t.Context(), connect.NewRequest(&dockwellv1.OpenTerminalRequest{
		ServerId: id, Container: "web-1", Columns: 80, Rows: 24,
	}))
	if err != nil {
		t.Fatalf("open container terminal: %v", err)
	}
	ctrCwd, err := c.GetTerminalWorkingDirectory(t.Context(), connect.NewRequest(&dockwellv1.GetTerminalWorkingDirectoryRequest{
		TerminalId: ctrRes.Msg.GetTerminal().GetId(),
	}))
	if err != nil {
		t.Fatalf("container GetTerminalWorkingDirectory: %v", err)
	}
	if ctrCwd.Msg.GetKnown() || ctrCwd.Msg.GetPath() != "" || ctrCwd.Msg.GetHome() != "" {
		t.Errorf("container cwd = %+v, want unknown with empty path and empty home", ctrCwd.Msg)
	}

	// Older agent without ReadsTerminalCwd returns empty home so the panel uses Files' own default.
	srv.stop()
	waitConnected(t, f, id, false)
	srv.terms = terminal.New(terminal.Options{Shell: []string{"/bin/sh"}, Home: t.TempDir()})
	srv.hello = func(h *agentv1.Hello) { h.ReadsTerminalCwd = false }
	srv.connect(t, f)
	term2 := openTerminal(t, b, id)
	oldCwd, err := c.GetTerminalWorkingDirectory(t.Context(), connect.NewRequest(&dockwellv1.GetTerminalWorkingDirectoryRequest{
		TerminalId: term2.GetId(),
	}))
	if err != nil {
		t.Fatalf("older agent cwd: %v", err)
	}
	if oldCwd.Msg.GetKnown() || oldCwd.Msg.GetHome() != "" {
		t.Errorf("older agent cwd = %+v, want unknown with empty home", oldCwd.Msg)
	}
}

func TestTerminalPreferencesAndSavedCommands(t *testing.T) {
	t.Parallel()
	storetest.Each(t, testTerminalPreferencesAndSavedCommands)
}

func testTerminalPreferencesAndSavedCommands(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	c := terminalClient(b, b.origin)

	// Default preferences are empty (meaning panel defaults).
	prefs, err := c.GetTerminalPreferences(t.Context(), connect.NewRequest(&dockwellv1.GetTerminalPreferencesRequest{}))
	if err != nil {
		t.Fatalf("GetTerminalPreferences: %v", err)
	}
	if prefs.Msg.GetPreferences().GetFontSize() != 0 || prefs.Msg.GetPreferences().GetScrollback() != 0 {
		t.Errorf("initial prefs = %+v, want zero defaults", prefs.Msg.GetPreferences())
	}

	// Invalid preferences are rejected.
	if _, err := c.UpdateTerminalPreferences(t.Context(), connect.NewRequest(&dockwellv1.UpdateTerminalPreferencesRequest{
		Preferences: &dockwellv1.TerminalPreferences{FontSize: 50},
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("font size 50 = %v, want InvalidArgument", err)
	}
	if _, err := c.UpdateTerminalPreferences(t.Context(), connect.NewRequest(&dockwellv1.UpdateTerminalPreferencesRequest{
		Preferences: &dockwellv1.TerminalPreferences{FontFamily: "Comic Sans"},
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("unknown font family = %v, want InvalidArgument", err)
	}

	// Valid preferences persist per user.
	blink := false
	savedPrefs, err := c.UpdateTerminalPreferences(t.Context(), connect.NewRequest(&dockwellv1.UpdateTerminalPreferencesRequest{
		Preferences: &dockwellv1.TerminalPreferences{
			FontSize:    15,
			FontFamily:  "fira-code",
			LineHeight:  1.4,
			CursorStyle: dockwellv1.TerminalCursorStyle_TERMINAL_CURSOR_STYLE_BAR,
			CursorBlink: &blink,
			Scrollback:  10000,
		},
	}))
	if err != nil {
		t.Fatalf("UpdateTerminalPreferences: %v", err)
	}
	if p := savedPrefs.Msg.GetPreferences(); p.GetFontSize() != 15 || p.GetFontFamily() != "fira-code" || p.GetCursorBlink() || p.GetScrollback() != 10000 {
		t.Errorf("saved prefs = %+v", p)
	}
	reloaded, err := c.GetTerminalPreferences(t.Context(), connect.NewRequest(&dockwellv1.GetTerminalPreferencesRequest{}))
	if err != nil || reloaded.Msg.GetPreferences().GetFontSize() != 15 || reloaded.Msg.GetPreferences().GetCursorStyle() != dockwellv1.TerminalCursorStyle_TERMINAL_CURSOR_STYLE_BAR {
		t.Fatalf("reloaded prefs = %+v, %v", reloaded, err)
	}

	// H2: control characters (ESC, bare CR, ^C, DEL) are refused on save and import; CRLF folds to LF.
	for _, bad := range []string{"echo \x1b[31mhi", "echo a\rb", "echo \x03", "echo \x7f"} {
		if _, err := c.SaveCommand(t.Context(), connect.NewRequest(&dockwellv1.SaveCommandRequest{
			Name:    "Bad",
			Command: bad,
		})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("SaveCommand(%q) = %v, want InvalidArgument", bad, err)
		}
		if _, err := c.ImportSavedCommands(t.Context(), connect.NewRequest(&dockwellv1.ImportSavedCommandsRequest{
			Commands: []*dockwellv1.SavedCommandInput{{Name: "Bad", Command: bad}},
		})); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("ImportSavedCommands(%q) = %v, want InvalidArgument", bad, err)
		}
	}

	// Saved commands: secret guard refuses without explicit confirmation.
	_, err = c.SaveCommand(t.Context(), connect.NewRequest(&dockwellv1.SaveCommandRequest{
		Name:    "API call",
		Command: "curl -H 'Authorization: Bearer ghp_1234567890abcdefghij1234567890abcd' https://api.github.com",
		Group:   "Debug",
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || reasonOf(err) != terminals.ReasonLooksLikeSecret {
		t.Fatalf("saving secret without confirmation = %v, want looks_like_secret", err)
	}

	// Personal command folds CRLF to LF and strips trailing newlines.
	personal, err := c.SaveCommand(t.Context(), connect.NewRequest(&dockwellv1.SaveCommandRequest{
		Name:    "Disk usage",
		Command: "df -h\r\ndu -sh /var/log\r\n",
		Group:   "Diagnostics",
	}))
	if err != nil || !strings.HasPrefix(personal.Msg.GetCommand().GetId(), "tcm_") || personal.Msg.GetCommand().GetShared() {
		t.Fatalf("save personal = %+v, %v", personal, err)
	}
	if got := personal.Msg.GetCommand().GetCommand(); got != "df -h\ndu -sh /var/log" {
		t.Fatalf("personal command body = %q, want normalized LF", got)
	}

	// Shared command records creator/editor and writes Activity without body.
	sharedSecretBody := "docker ps --filter status=running"
	shared, err := c.SaveCommand(t.Context(), connect.NewRequest(&dockwellv1.SaveCommandRequest{
		Name:    "Container status",
		Command: sharedSecretBody,
		Group:   "Docker",
		Shared:  true,
	}))
	if err != nil || !shared.Msg.GetCommand().GetShared() {
		t.Fatalf("save shared = %+v, %v", shared, err)
	}
	if shared.Msg.GetCommand().GetCreatedBy() == "" || shared.Msg.GetCommand().GetUpdatedBy() == "" {
		t.Fatalf("shared command missing author/editor: %+v", shared.Msg.GetCommand())
	}

	// Edit personal command and move it to shared, then move it back to personal (audited as shared delete).
	edited, err := c.SaveCommand(t.Context(), connect.NewRequest(&dockwellv1.SaveCommandRequest{
		Id:      personal.Msg.GetCommand().GetId(),
		Name:    "Disk usage (human)",
		Command: "df -hT",
		Group:   "Diagnostics",
		Shared:  true,
	}))
	if err != nil || edited.Msg.GetCommand().GetName() != "Disk usage (human)" || !edited.Msg.GetCommand().GetShared() {
		t.Fatalf("edit command = %+v, %v", edited, err)
	}
	movedBack, err := c.SaveCommand(t.Context(), connect.NewRequest(&dockwellv1.SaveCommandRequest{
		Id:      edited.Msg.GetCommand().GetId(),
		Name:    "Disk usage (personal)",
		Command: "df -hT",
		Group:   "Diagnostics",
		Shared:  false,
	}))
	if err != nil || movedBack.Msg.GetCommand().GetShared() {
		t.Fatalf("move back to personal = %+v, %v", movedBack, err)
	}

	// Import commands: secret guard applies to batches too.
	_, err = c.ImportSavedCommands(t.Context(), connect.NewRequest(&dockwellv1.ImportSavedCommandsRequest{
		Commands: []*dockwellv1.SavedCommandInput{
			{Name: "Safe", Command: "uptime", Group: "System"},
			{Name: "Secret", Command: "TOKEN=sk-1234567890abcdefghij12345 ./run.sh", Group: "System"},
		},
	}))
	if reasonOf(err) != terminals.ReasonLooksLikeSecret {
		t.Fatalf("import with secret = %v, want looks_like_secret", err)
	}
	imported, err := c.ImportSavedCommands(t.Context(), connect.NewRequest(&dockwellv1.ImportSavedCommandsRequest{
		Commands: []*dockwellv1.SavedCommandInput{
			{Name: "Uptime", Command: "uptime", Group: "System"},
			{Name: "Memory", Command: "free -h", Group: "System", Shared: true},
		},
	}))
	if err != nil || imported.Msg.GetImported() != 2 || len(imported.Msg.GetCommands()) != 4 {
		t.Fatalf("import = %+v, %v", imported, err)
	}

	// Delete one shared command.
	if _, err := c.DeleteSavedCommand(t.Context(), connect.NewRequest(&dockwellv1.DeleteSavedCommandRequest{
		Id:     shared.Msg.GetCommand().GetId(),
		Shared: true,
	})); err != nil {
		t.Fatalf("DeleteSavedCommand: %v", err)
	}
	listed, err := c.ListSavedCommands(t.Context(), connect.NewRequest(&dockwellv1.ListSavedCommandsRequest{}))
	if err != nil || len(listed.Msg.GetCommands()) != 3 {
		t.Fatalf("ListSavedCommands after delete = %d, %v", len(listed.Msg.GetCommands()), err)
	}

	// M4: Verify Activity events exist for shared command save/import/move-out/delete and never contain bodies.
	evts, err := eventClient(b, b.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{
		Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_SETTINGS},
	}))
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var saves, deletes int
	for _, ev := range evts.Msg.GetEvents() {
		switch ev.GetAction() {
		case terminals.EventCommandSave:
			saves++
		case terminals.EventCommandDelete:
			deletes++
		}
		if strings.Contains(ev.String(), sharedSecretBody) {
			t.Fatalf("shared command body leaked into Activity event: %v", ev)
		}
	}
	// Saves: Container status + Disk usage (moved to shared) + Memory (imported shared) = 3.
	// Deletes: Disk usage (moved out of shared) + Container status (deleted) = 2.
	if saves != 3 || deletes != 2 {
		t.Errorf("Activity shared command events: saves=%d deletes=%d, want 3 saves and 2 deletes", saves, deletes)
	}

	// API tokens are refused for all new TerminalService methods too.
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := terminalClient(bearer{token: token, rt: b.client.Transport}, b.origin)
	if _, err := script.GetTerminalPreferences(t.Context(), connect.NewRequest(&dockwellv1.GetTerminalPreferencesRequest{})); reasonOf(err) != authapi.ReasonNeedsSession {
		t.Errorf("token GetTerminalPreferences = %v, want needs_session", err)
	}
	if _, err := script.ListSavedCommands(t.Context(), connect.NewRequest(&dockwellv1.ListSavedCommandsRequest{})); reasonOf(err) != authapi.ReasonNeedsSession {
		t.Errorf("token ListSavedCommands = %v, want needs_session", err)
	}
}

func TestRemovedUserTerminalSettingsAndSharedAuthors(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newInviteStand(t, backend)
		ownerTerm := terminalClient(s.owner, s.origin)
		adaBrowser, adaID := s.joins(t, "ada@example.com", "Ada Lovelace", dockwellv1.Role_ROLE_ADMIN)
		adaTerm := terminalClient(adaBrowser, s.origin)

		// Owner creates a shared command, Ada edits it -> created_by=Mira Chen, updated_by=Ada Lovelace.
		created, err := ownerTerm.SaveCommand(t.Context(), connect.NewRequest(&dockwellv1.SaveCommandRequest{
			Name:    "Restart API",
			Command: "docker restart api",
			Shared:  true,
		}))
		if err != nil {
			t.Fatalf("owner SaveCommand: %v", err)
		}
		if got := created.Msg.GetCommand(); got.GetCreatedBy() != "Mira Chen" || got.GetUpdatedBy() != "Mira Chen" {
			t.Fatalf("created shared command authors = (%q, %q), want Mira Chen", got.GetCreatedBy(), got.GetUpdatedBy())
		}
		edited, err := adaTerm.SaveCommand(t.Context(), connect.NewRequest(&dockwellv1.SaveCommandRequest{
			Id:      created.Msg.GetCommand().GetId(),
			Name:    "Restart API cleanly",
			Command: "docker restart -t 30 api",
			Shared:  true,
		}))
		if err != nil {
			t.Fatalf("ada edit shared command: %v", err)
		}
		if got := edited.Msg.GetCommand(); got.GetCreatedBy() != "Mira Chen" || got.GetUpdatedBy() != "Ada Lovelace" {
			t.Fatalf("edited shared command authors = (%q, %q), want (Mira Chen, Ada Lovelace)", got.GetCreatedBy(), got.GetUpdatedBy())
		}

		// L7: Ada saves personal preferences and a personal command (with plain-text secret),
		// then is removed; both personal settings rows must be deleted in the same transaction.
		if _, err := adaTerm.UpdateTerminalPreferences(t.Context(), connect.NewRequest(&dockwellv1.UpdateTerminalPreferencesRequest{
			Preferences: &dockwellv1.TerminalPreferences{FontSize: 18},
		})); err != nil {
			t.Fatalf("ada UpdateTerminalPreferences: %v", err)
		}
		if _, err := adaTerm.SaveCommand(t.Context(), connect.NewRequest(&dockwellv1.SaveCommandRequest{
			Name:                 "Ada secret",
			Command:              "export API_TOKEN=sk_live_1234567890abcdef",
			AllowPlainTextSecret: true,
		})); err != nil {
			t.Fatalf("ada SaveCommand: %v", err)
		}
		proof := stepUpFor(t, s.owner, dockwellv1.StepUpAction_STEP_UP_ACTION_REMOVE_MEMBER, adaID)
		if _, err := s.members.RemoveMember(t.Context(), connect.NewRequest(&dockwellv1.RemoveMemberRequest{
			UserId: adaID,
			Proof:  proof,
		})); err != nil {
			t.Fatalf("RemoveMember: %v", err)
		}
		if err := s.srv.store.View(t.Context(), func(tx *store.Tx) error {
			if _, err := tx.GetSetting(t.Context(), store.SettingUserTerminalPreferences(adaID)); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("removed user preferences still present: %v", err)
			}
			if _, err := tx.GetSetting(t.Context(), store.SettingUserTerminalCommands(adaID)); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("removed user saved commands still present: %v", err)
			}
			if _, err := tx.GetSetting(t.Context(), store.SettingSharedTerminalCommands); err != nil {
				t.Errorf("shared commands missing after member removal: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatalf("store.View: %v", err)
		}
	})
}
