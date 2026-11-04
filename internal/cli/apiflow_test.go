// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"context"
	"database/sql"
	"errors"
	"hash/crc32"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/cli/apicli"
	"github.com/dockwell/dockwell/internal/cli/apicmd"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// wellFormedToken is an API token's secret with a right checksum, which
// the CLI takes without asking the control plane.
func wellFormedToken() string {
	const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	digits := func(n *big.Int, width int) string {
		out := make([]byte, width)
		base, digit := big.NewInt(62), new(big.Int)
		for i := width - 1; i >= 0; i-- {
			n.DivMod(n, base, digit)
			out[i] = base62[digit.Int64()]
		}
		return string(out)
	}
	body := strings.Repeat("7", auth.TokenLen-len(auth.TokenPrefix)-6)
	return auth.TokenPrefix + body + digits(new(big.Int).SetUint64(uint64(crc32.ChecksumIEEE([]byte(body)))), 6)
}

// stubChanges is a ChangeService whose watches answer as a test says.
type stubChanges struct {
	dockwellv1connect.UnimplementedChangeServiceHandler
	mu      sync.Mutex
	watches int
	// watch answers the nth watch, from 1.
	watch func(n int, send func(*dockwellv1.Change) error) error
}

func (s *stubChanges) WatchChange(_ context.Context, _ *connect.Request[dockwellv1.WatchChangeRequest], stream *connect.ServerStream[dockwellv1.WatchChangeResponse]) error {
	s.mu.Lock()
	s.watches++
	n := s.watches
	s.mu.Unlock()
	return s.watch(n, func(c *dockwellv1.Change) error {
		return stream.Send(&dockwellv1.WatchChangeResponse{Event: &dockwellv1.WatchChangeResponse_Change{Change: c}})
	})
}

// serveChanges serves stub on loopback and returns the environment that
// points the CLI at it.
func serveChanges(t *testing.T, stub *stubChanges) map[string]string {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(dockwellv1connect.NewChangeServiceHandler(stub))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return map[string]string{apicli.EnvURL: srv.URL, apicli.EnvToken: wellFormedToken(), "XDG_CONFIG_HOME": t.TempDir()}
}

// fellBehind is how the control plane ends a watch that fell behind.
func fellBehind() error {
	err := connect.NewError(connect.CodeUnavailable, errors.New("this watch fell behind the change, so Dockwell ended it. Watch it again"))
	err.Meta().Set(authapi.ReasonKey, "watch_behind")
	return err
}

const stubChange = "chg_01JD6Z3R8M9QF2K7T4XW5BNC0V"

// appliedChange is a change that started applying 3 hours ago and whose
// one step took 42 seconds.
func appliedChange() *dockwellv1.Change {
	start := time.Now().Add(-3 * time.Hour)
	return &dockwellv1.Change{
		Id: stubChange, State: dockwellv1.ChangeState_CHANGE_STATE_APPLIED, ApplyTime: timestamppb.New(start),
		Steps: []*dockwellv1.StepProgress{{
			State: dockwellv1.StepState_STEP_STATE_DONE, ServerId: "srv_1", Message: "Open port 80",
			StartTime: timestamppb.New(start), FinishTime: timestamppb.New(start.Add(42 * time.Second)),
		}},
	}
}

// A watch that falls behind the change is watched again: the change goes
// on, and so does following it, until it ends.
func TestAWatchThatFellBehindWatchesAgain(t *testing.T) {
	stub := &stubChanges{watch: func(n int, send func(*dockwellv1.Change) error) error {
		if n == 1 {
			applying := appliedChange()
			applying.State, applying.Steps[0].State, applying.Steps[0].FinishTime = dockwellv1.ChangeState_CHANGE_STATE_APPLYING, dockwellv1.StepState_STEP_STATE_RUNNING, nil
			if err := send(applying); err != nil {
				return err
			}
			return fellBehind()
		}
		return send(appliedChange())
	}}
	env := serveChanges(t, stub)
	r := run(t, env, "changes", "watch", stubChange)
	if r.code != exitOK || !strings.Contains(r.out, "Applied "+stubChange) || stub.watches != 2 {
		t.Fatalf("changes watch, once the watch fell behind = %d after %d watches:\n%s%s", r.code, stub.watches, r.out, r.err)
	}
	if n := strings.Count(r.out, "Open port 80"); n != 2 {
		t.Errorf("the step shows %d times, want once running and once done:\n%s", n, r.out)
	}
	r = run(t, env, "changes", "watch", stubChange, "--json")
	if r.code != exitOK || !strings.Contains(r.out, "CHANGE_STATE_APPLIED") {
		t.Errorf("changes watch --json, once the watch fell behind = %d:\n%s%s", r.code, r.out, r.err)
	}

	// A watch that keeps falling behind stops, in the end.
	always := &stubChanges{watch: func(_ int, send func(*dockwellv1.Change) error) error {
		applying := appliedChange()
		applying.State = dockwellv1.ChangeState_CHANGE_STATE_APPLYING
		if err := send(applying); err != nil {
			return err
		}
		return fellBehind()
	}}
	r = run(t, serveChanges(t, always), "changes", "watch", stubChange)
	if r.code != exitError || !strings.Contains(r.err, "It goes on applying: follow it again with dockwell changes watch "+stubChange) || always.watches != 11 {
		t.Errorf("a watch that always falls behind = %d after %d watches: %q", r.code, always.watches, r.err)
	}
}

// Watching a change that applied long ago says how long it took, by its own
// times, not how long the watch did.
func TestWatchSaysHowLongTheChangeTook(t *testing.T) {
	stub := &stubChanges{watch: func(_ int, send func(*dockwellv1.Change) error) error { return send(appliedChange()) }}
	r := run(t, serveChanges(t, stub), "changes", "watch", stubChange)
	if want := "Applied " + stubChange + " in " + apicli.Elapsed(42*time.Second) + "."; r.code != exitOK || !strings.Contains(r.out, want) {
		t.Errorf("changes watch = %d, want %q:\n%s%s", r.code, want, r.out, r.err)
	}
	stepless := &stubChanges{watch: func(_ int, send func(*dockwellv1.Change) error) error {
		c := appliedChange()
		c.Steps = nil
		return send(c)
	}}
	r = run(t, serveChanges(t, stepless), "changes", "watch", stubChange)
	if want := "Applied " + stubChange + "."; r.code != exitOK || !strings.Contains(r.out, want) {
		t.Errorf("changes watch of a change with no steps = %d, want %q:\n%s%s", r.code, want, r.out, r.err)
	}
}

// A config file the CLI can't use is said first, with its fix, not passed
// over for local access.
func TestABadConfigFileIsSaidFirst(t *testing.T) {
	for name, tt := range map[string]struct {
		content string
		mode    os.FileMode
		want    string
	}{
		"others can read it": {`{"url": "https://panel.acme.dev"}`, 0o644, "Run chmod 600"},
		"it doesn't parse":   {`{"url": `, 0o600, "isn't a config file dockwell can read"},
	} {
		t.Run(name, func(t *testing.T) {
			config := t.TempDir()
			path := filepath.Join(config, "dockwell", "config.json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tt.content), tt.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tt.mode); err != nil {
				t.Fatal(err)
			}
			r := run(t, map[string]string{"XDG_CONFIG_HOME": config, envDataDir: t.TempDir()}, "servers", "list")
			if r.code != exitError || !strings.Contains(r.err, tt.want) {
				t.Errorf("servers list with a config file that %s = %d %q, want it to say %q", name, r.code, r.err, tt.want)
			}
		})
	}
}

// Local access opens the control plane's state as it is: it never
// upgrades a state an older dockwell left, as between an upgrade and the
// control plane's restart, and says to restart it.
// Only dockwell run migrates the state, as it starts the control plane.
// Every other command beside a control plane, from local access to the
// admin commands, leaves an older state as it is and says to restart.
func TestCommandsBesideTheControlPlaneNeverMigrate(t *testing.T) {
	dir := datadir.Dir{Root: t.TempDir()}
	if err := datadir.Ensure(dir.Control()); err != nil {
		t.Fatal(err)
	}
	if _, err := seal.LoadOrCreate(dir.SealKey()); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), dir.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	older := store.SchemaVersion() - 1
	storetest.Exec(t, dir.StateDB(), "DELETE FROM schema_migrations WHERE version > "+strconv.Itoa(older))
	newest := func() int {
		raw, err := sql.Open("sqlite", "file:"+dir.StateDB())
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		var v int
		if err := raw.QueryRowContext(t.Context(), `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	env := map[string]string{"XDG_CONFIG_HOME": t.TempDir(), envDataDir: dir.Root, envLogFormat: "json"}
	for _, args := range [][]string{{"servers", "list"}, {"login", "--local"}, {"claim"}, {"recover"}, {"token", "create"}} {
		r := run(t, env, args...)
		if r.code != exitError || !strings.Contains(r.err, "runs an older dockwell than this one") || !strings.Contains(r.err, "Restart the control plane") {
			t.Errorf("%s with the state at an older schema = %d %q", strings.Join(args, " "), r.code, r.err)
		}
		if v := newest(); v != older {
			t.Fatalf("after %s the state's schema version is %d, want it left at %d", strings.Join(args, " "), v, older)
		}
	}
	if _, err := os.Stat(dir.LocalToken()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("local access kept a token for a state it couldn't use: %v", err)
	}
}

// On the control server, a user other than root, who looks in a data
// directory of their own by default, is told to use sudo.
func TestLocalAccessAsAnotherUserSaysSudo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root looks in the system's data directory already")
	}
	system := t.TempDir()
	defer func(was string) { systemDataDir = was }(systemDataDir)
	systemDataDir = system
	env := map[string]string{"XDG_CONFIG_HOME": t.TempDir(), "XDG_STATE_HOME": t.TempDir()}

	r := run(t, env, "servers", "list")
	if r.code != exitError || !strings.Contains(r.err, "doesn't know which control plane to call") {
		t.Errorf("with no control plane on this server = %d %q", r.code, r.err)
	}
	if err := datadir.Ensure(datadir.Dir{Root: system}.Control()); err != nil {
		t.Fatal(err)
	}
	r = run(t, env, "servers", "list")
	if r.code != exitError || !strings.Contains(r.err, "with sudo") || !strings.Contains(r.err, system) {
		t.Errorf("with the control plane in the system's data directory = %d %q", r.code, r.err)
	}
	if err := os.Chmod(system, 0o700&^0o100); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(system, 0o700) //nolint:errcheck // the test's own directory
	r = run(t, env, "servers", "list")
	if r.code != exitError || !strings.Contains(r.err, "only root can look in "+system) || !strings.Contains(r.err, "with sudo") {
		t.Errorf("with a system data directory only root can look in = %d %q", r.code, r.err)
	}
}

// A mistyped verb is a usage error that points to the noun's help.
func TestAMistypedVerbIsAUsageError(t *testing.T) {
	env := map[string]string{"XDG_CONFIG_HOME": t.TempDir()}
	for _, args := range [][]string{{"servers", "lsit"}, {"projects", "craete", "shop"}} {
		r := run(t, env, args...)
		help := "dockwell " + args[0] + " --help"
		if r.code != exitUsage || !strings.Contains(r.err, `Run "`+help+`" to see its commands`) {
			t.Errorf("dockwell %s = %d %q, want a usage error that points to %s", strings.Join(args, " "), r.code, r.err, help)
		}
	}
	if r := run(t, env, "servers", "lsit"); !strings.Contains(r.err, "though it has list") {
		t.Errorf("a mistyped verb doesn't name the one it's like: %q", r.err)
	}
}

// Only commands that propose a change show its plan and offer to apply it:
// discarding one is a plain command.
func TestOnlyProposalsOfferToApply(t *testing.T) {
	env := map[string]string{"XDG_CONFIG_HOME": t.TempDir()}
	for _, args := range [][]string{{"changes", "propose"}, {"changes", "undo"}, {"drift", "revert"}, {"drift", "adopt"}} {
		r := run(t, env, append(args, "--help")...)
		if r.code != exitOK || !strings.Contains(r.out, "It shows the plan and asks before applying it") || !strings.Contains(r.out, "--plan-only") || !strings.Contains(r.out, "--yes") {
			t.Errorf("dockwell %s --help doesn't offer to apply:\n%s", strings.Join(args, " "), r.out)
		}
	}
	r := run(t, env, "changes", "discard", "--help")
	if r.code != exitOK || strings.Contains(r.out, "shows the plan") || strings.Contains(r.out, "--plan-only") || strings.Contains(r.out, "--yes") {
		t.Errorf("dockwell changes discard --help offers to apply:\n%s", r.out)
	}
}

// A kind that lives on one server takes --on once.
func TestOnTakesOneServerForAKindOnOne(t *testing.T) {
	env := map[string]string{"XDG_CONFIG_HOME": t.TempDir()}
	r := run(t, env, "firewall-rules", "create", "http", "--port", "80", "--on", "hz-1", "--on", "hz-2")
	if r.code != exitUsage || !strings.Contains(r.err, "a firewall rule is on one server, so --on takes one, but got hz-1 and hz-2") {
		t.Errorf("--on twice for a firewall rule = %d %q", r.code, r.err)
	}
	r = run(t, env, "services", "create", "--help")
	if !strings.Contains(r.out, "repeat it for each") {
		t.Errorf("a service doesn't take --on for each server it runs on:\n%s", r.out)
	}
}

// The server's own commands never build the API commands, so nothing in
// them can stop dockwell run; the API commands say when they can't be
// built.
func TestTheServersCommandsDontBuildTheAPIs(t *testing.T) {
	built := 0
	defer func(was func() []*apicli.Method) { apiMethods = was }(apiMethods)
	apiMethods = func() []*apicli.Method {
		built++
		return []*apicli.Method{{Name: "dockwell.v1.Nothing.Broken", Noun: "nowhere", Verb: "list"}}
	}
	for _, args := range [][]string{{"version"}, {"run", "--help"}, {"--data-dir", t.TempDir(), "role"}, {"token", "create", "--help"}} {
		if r := run(t, nil, args...); r.code != exitOK {
			t.Errorf("dockwell %s, with API commands that can't be built = %d %q", strings.Join(args, " "), r.code, r.err)
		}
	}
	if built != 0 {
		t.Errorf("the server's own commands built the API commands %d times", built)
	}
	r := run(t, nil, "servers", "list")
	if r.code != exitError || !strings.Contains(r.err, "can't build the API commands") || built != 1 {
		t.Errorf("an API command that can't be built = %d %q", r.code, r.err)
	}
}

// Every flag a field takes avoids the CLI's own: those every API command
// has (--url, --token, --json, --yes, --plan-only), the global flags, the
// flags a kind's commands have for where a resource lives and for the
// change's summary, and any other flag its command has. The one field
// that shares a fixed flag's name is ProposeChange's summary, which is
// that same summary.
func TestFieldFlagsAvoidTheCLIsOwn(t *testing.T) {
	root, err := newRootCmd(IO{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	reserved := map[string]bool{}
	for _, name := range apicli.ReservedFlags {
		reserved[name] = true
	}
	root.PersistentFlags().VisitAll(func(f *pflag.Flag) { reserved[f.Name] = true })
	kindFlags := []string{"summary", "project", "environment", "on"}
	fields := 0
	var walk func(cmd *cobra.Command, kind bool)
	walk = func(cmd *cobra.Command, kind bool) {
		cmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if _, ok := f.Annotations[apicli.FieldAnnotation]; !ok {
				return
			}
			fields++
			switch {
			case reserved[f.Name]:
				t.Errorf("%s --%s, a field's flag, takes one of the CLI's own", cmd.CommandPath(), f.Name)
			case kind && slices.Contains(kindFlags, f.Name):
				t.Errorf("%s --%s, a field's flag, takes one a kind's commands have", cmd.CommandPath(), f.Name)
			}
			for p := cmd.Parent(); p != nil; p = p.Parent() {
				if p.PersistentFlags().Lookup(f.Name) != nil {
					t.Errorf("%s --%s, a field's flag, hides %s's", cmd.CommandPath(), f.Name, p.CommandPath())
				}
			}
		})
		for _, c := range cmd.Commands() {
			walk(c, kind || slices.Contains(kindNouns(), c.Name()))
		}
	}
	walk(root, false)
	if fields < 30 {
		t.Fatalf("found %d flags for fields; the test no longer finds them", fields)
	}
	for _, args := range [][]string{{"claim-link", "check", "--help"}, {"sessions", "sign-in-with-link", "--help"}} {
		r := run(t, nil, args...)
		if !strings.Contains(r.out, "--claim-token") && !strings.Contains(r.out, "--link-token") {
			t.Errorf("dockwell %s doesn't take the link's token as a flag of its own:\n%s", strings.Join(args[:2], " "), r.out)
		}
	}
}

// kindNouns are the commands for each kind of resource.
func kindNouns() []string {
	var out []string
	for _, k := range apicmd.Kinds() {
		out = append(out, k.Noun)
	}
	return out
}
