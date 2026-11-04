// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/known/durationpb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth/authtest"
	"github.com/dockwell/dockwell/internal/cli/apicli"
	"github.com/dockwell/dockwell/internal/cli/apicmd"
	"github.com/dockwell/dockwell/internal/resource/variable"
)

// runIn runs the CLI with standard input and, when interactive, as if a
// person were at a terminal.
func runIn(t *testing.T, env map[string]string, input string, interactive bool, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Execute(t.Context(), args, IO{
		Out:         &out,
		Err:         &errOut,
		Getenv:      func(k string) string { return env[k] },
		In:          strings.NewReader(input),
		Interactive: func() bool { return interactive },
	})
	return result{code: code, out: out.String(), err: errOut.String()}
}

// claimWithTokens claims the install from link with a software passkey, as
// the panel would, and makes a read-write and a read-only API token with
// the session, as Profile and security would. It returns the panel's
// loopback URL, on 127.0.0.1 as the CLI on the control server reaches it.
func claimWithTokens(t *testing.T, link string) (url, readWrite, readOnly string) {
	t.Helper()
	m := linkRE.FindStringSubmatch(link)
	base, token := m[1], m[3]
	port := strings.TrimPrefix(base, "http://localhost:")
	var d net.Dialer
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, "127.0.0.1:"+port)
	}}}
	c := dockwellv1connect.NewAuthServiceClient(client, base, connect.WithInterceptors(originHeader(base)))
	var begin *connect.Response[dockwellv1.BeginClaimResponse]
	deadline := time.Now().Add(5 * time.Second)
	for {
		begin, err = c.BeginClaim(t.Context(), connect.NewRequest(&dockwellv1.BeginClaimRequest{Token: token, Name: "Mira Chen", Email: "mira@acme.dev"}))
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("BeginClaim: %v", err)
	}
	cred, _, err := (&authtest.Authenticator{}).Create(begin.Msg.GetOptionsJson(), base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.FinishClaim(t.Context(), connect.NewRequest(&dockwellv1.FinishClaimRequest{
		CeremonyId: begin.Msg.GetCeremonyId(), Token: token, CredentialJson: cred,
	})); err != nil {
		t.Fatalf("FinishClaim: %v", err)
	}
	tokens := dockwellv1connect.NewTokenServiceClient(client, base, connect.WithInterceptors(originHeader(base)))
	create := func(name string, scope dockwellv1.TokenScope) string {
		res, err := tokens.CreateToken(t.Context(), connect.NewRequest(&dockwellv1.CreateTokenRequest{
			Name: name, Scope: scope, Expiration: &dockwellv1.CreateTokenRequest_Ttl{Ttl: durationpb.New(time.Hour)},
		}))
		if err != nil {
			t.Fatalf("CreateToken: %v", err)
		}
		return res.Msg.GetSecret()
	}
	return "http://127.0.0.1:" + port, create("CLI test", dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), create("CLI reader", dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY)
}

var changeRE = regexp.MustCompile(`chg_[0-9A-Z]{26}`)

func TestTheCLIAgainstARealControlPlane(t *testing.T) {
	dir := t.TempDir()
	out, _ := startRunWithOutput(t, dir, "/claim#")
	url, secret, readOnly := claimWithTokens(t, linkRE.FindString(out.String()))
	env := map[string]string{
		apicli.EnvURL: url, apicli.EnvToken: secret,
		"XDG_CONFIG_HOME": t.TempDir(), envLogFormat: "json",
	}

	t.Run("list servers", func(t *testing.T) {
		r := run(t, env, "servers", "list")
		if r.code != exitOK {
			t.Fatalf("servers list = %d:\n%s%s", r.code, r.out, r.err)
		}
		for _, want := range []string{"Name", "Connection", "Last seen", "(control)", "srv_"} {
			if !strings.Contains(r.out, want) {
				t.Errorf("servers list doesn't show %q:\n%s", want, r.out)
			}
		}
	})

	t.Run("json", func(t *testing.T) {
		r := run(t, env, "servers", "list", "--json")
		var got struct {
			Servers []struct {
				ID            string `json:"id"`
				Name          string `json:"name"`
				ControlServer bool   `json:"controlServer"`
				Connection    string `json:"connection"`
			} `json:"servers"`
		}
		if err := json.Unmarshal([]byte(r.out), &got); err != nil || r.code != exitOK {
			t.Fatalf("servers list --json = %d, %v:\n%s%s", r.code, err, r.out, r.err)
		}
		if len(got.Servers) != 1 || !got.Servers[0].ControlServer || !strings.HasPrefix(got.Servers[0].ID, "srv_") || got.Servers[0].Connection == "" {
			t.Errorf("servers list --json = %+v, want the control server in the API's shape", got)
		}
	})

	var created string
	t.Run("create a project, confirm and apply", func(t *testing.T) {
		r := runIn(t, env, "y\n", true, "projects", "create", "shop", "--display-name", "Shop")
		if r.code != exitOK {
			t.Fatalf("projects create = %d:\n%s%s", r.code, r.out, r.err)
		}
		for _, want := range []string{
			"Create project shop", "Planned ", " by the API token CLI test from the CLI.",
			"+ project shop", "Create", "+ display_name", "Shop", "+ environment production", "2 to create · 0 to change · 0 to destroy",
			"Applied chg_", "Undo it within 7 days",
		} {
			if !strings.Contains(r.out, want) {
				t.Errorf("projects create didn't print %q:\n%s", want, r.out)
			}
		}
		if !strings.Contains(r.err, "Apply this plan? (y/N)") {
			t.Errorf("projects create didn't ask before applying:\n%s", r.err)
		}
		created = changeRE.FindString(r.out)

		r = run(t, env, "changes", "get", created)
		if r.code != exitOK || !strings.Contains(r.out, "\nApplied ") || !strings.Contains(r.out, "from the CLI") {
			t.Errorf("changes get %s = %d:\n%s%s", created, r.code, r.out, r.err)
		}
		r = run(t, env, "projects", "create", "shop", "--yes")
		if r.code != exitUsage || !strings.Contains(r.err, "project shop already exists. Change it with dockwell projects update shop") {
			t.Errorf("creating shop again = %d %q", r.code, r.err)
		}
	})

	// shop is there too now, so each names its project.
	t.Run("a secret's value comes from standard input or a file, never a flag", func(t *testing.T) {
		r := runIn(t, env, "s3cret-from-stdin\n", false, "secrets", "create", "DB_PASSWORD", "--value-file", "-", "--project", "default", "--yes")
		if r.code != exitOK || !strings.Contains(r.out, "+ secret DB_PASSWORD") || !strings.Contains(r.out, "Applied chg_") {
			t.Fatalf("secrets create from standard input = %d:\n%s%s", r.code, r.out, r.err)
		}
		file := filepath.Join(t.TempDir(), "value")
		if err := os.WriteFile(file, []byte("s3cret-from-a-file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		r = run(t, env, "secrets", "update", "DB_PASSWORD", "--value-file", file, "--project", "default", "--yes")
		if r.code != exitOK || !strings.Contains(r.out, "~ secret DB_PASSWORD") {
			t.Fatalf("secrets update from a file = %d:\n%s%s", r.code, r.out, r.err)
		}
		// The CLI refuses a value no secret can hold, naming the limit a
		// secret has, before it proposes anything; one at the limit, with
		// the newline an editor adds, goes through.
		big := filepath.Join(t.TempDir(), "big")
		if err := os.WriteFile(big, []byte(strings.Repeat("x", variable.MaxValue+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		r = run(t, env, "secrets", "update", "DB_PASSWORD", "--value-file", big, "--project", "default", "--yes")
		if r.code == exitOK || !strings.Contains(r.err, "the value is over 64 KiB, so no secret can hold it") {
			t.Errorf("a value over the limit = %d %q, want it refused, naming 64 KiB", r.code, r.err)
		}
		if err := os.WriteFile(big, []byte(strings.Repeat("x", variable.MaxValue)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		r = run(t, env, "secrets", "update", "DB_PASSWORD", "--value-file", big, "--project", "default", "--yes")
		if r.code != exitOK || !strings.Contains(r.out, "~ secret DB_PASSWORD") {
			t.Fatalf("a value at the limit = %d:\n%s%s", r.code, r.out, r.err)
		}
		r = run(t, env, "variables", "create", "LOG_LEVEL", "--value", "debug", "--project", "default", "--yes")
		if r.code != exitOK || !strings.Contains(r.out, "debug") {
			t.Fatalf("variables create = %d:\n%s%s", r.code, r.out, r.err)
		}
		list := run(t, env, "variables", "list", "--project", "default")
		if list.code != exitOK || !strings.Contains(list.out, "DB_PASSWORD") || !strings.Contains(list.out, "LOG_LEVEL") {
			t.Errorf("variables list = %d:\n%s%s", list.code, list.out, list.err)
		}
		r = run(t, env, "secrets", "create", "OTHER", "--value", "s3cret-on-the-command-line", "--project", "default", "--yes")
		if r.code != exitUsage || !strings.Contains(r.err, "unknown flag: --value") {
			t.Errorf("a secret's value as a flag = %d %q, want it refused", r.code, r.err)
		}
		id := regexp.MustCompile(`sec_[0-9A-Z]{26}`).FindString(run(t, env, "variables", "list", "--project", "default", "--json").out)
		r = run(t, env, "secrets", "reveal", id, "--proof", "anything")
		if r.code == exitOK || !strings.Contains(r.err, "reveal a secret") {
			t.Errorf("secrets reveal with a token = %d %q, want it refused", r.code, r.err)
		}
		for _, text := range []string{r.out, r.err, list.out, list.err} {
			if strings.Contains(text, "s3cret") {
				t.Errorf("the CLI printed a secret's value:\n%s", text)
			}
		}
	})

	t.Run("no means no", func(t *testing.T) {
		r := runIn(t, env, "n\n", true, "projects", "create", "blog")
		if r.code != exitError || !strings.Contains(r.err, "so Dockwell discarded it. Nothing changed") {
			t.Fatalf("answering no = %d:\n%s%s", r.code, r.out, r.err)
		}
		r = run(t, env, "projects", "create", "blog")
		if r.code != exitUsage || !strings.Contains(r.err, "no terminal to ask whether to apply it") || !strings.Contains(r.err, "--yes") {
			t.Errorf("with no terminal = %d %q", r.code, r.err)
		}
		r = run(t, env, "changes", "list")
		if r.code != exitOK || !strings.Contains(r.out, "Discarded") || !strings.Contains(r.out, "Planned") {
			t.Errorf("changes list = %d:\n%s%s", r.code, r.out, r.err)
		}
	})

	t.Run("undo", func(t *testing.T) {
		r := runIn(t, env, "y\n", true, "changes", "undo", created)
		if r.code != exitOK {
			t.Fatalf("changes undo = %d:\n%s%s", r.code, r.out, r.err)
		}
		for _, want := range []string{"It undoes " + created, "- project shop", "Destroy", "0 to create · 0 to change · 2 to destroy", "Applied chg_"} {
			if !strings.Contains(r.out, want) {
				t.Errorf("changes undo didn't print %q:\n%s", want, r.out)
			}
		}
		r = run(t, env, "changes", "undo", created, "--yes")
		if r.code != exitError || !strings.Contains(r.err, created) {
			t.Errorf("undoing twice = %d %q", r.code, r.err)
		}
	})

	t.Run("json through a whole change", func(t *testing.T) {
		r := run(t, env, "projects", "create", "shop", "--yes", "--json")
		if r.code != exitOK {
			t.Fatalf("projects create --json = %d:\n%s%s", r.code, r.out, r.err)
		}
		lines := strings.Split(strings.TrimSpace(r.out), "\n")
		var first struct {
			Change struct {
				ID    string `json:"id"`
				State string `json:"state"`
				Plan  struct {
					Summary struct {
						Create int `json:"create"`
					} `json:"summary"`
				} `json:"plan"`
			} `json:"change"`
		}
		if err := json.Unmarshal([]byte(lines[0]), &first); err != nil || first.Change.State != "CHANGE_STATE_PLANNED" || first.Change.Plan.Summary.Create != 2 {
			t.Errorf("the first line isn't the planned change, %v:\n%s", err, r.out)
		}
		last := lines[len(lines)-1]
		if !strings.Contains(last, `"CHANGE_STATE_APPLIED"`) {
			t.Errorf("the last line isn't the applied change:\n%s", r.out)
		}
		for _, l := range lines {
			if !json.Valid([]byte(l)) {
				t.Errorf("not a line of JSON: %s", l)
			}
		}

		// A plan left planned is one response, so it is indented.
		r = run(t, env, "projects", "create", "docs", "--plan-only", "--json")
		var planned struct {
			Change struct {
				ID string `json:"id"`
			} `json:"change"`
		}
		if err := json.Unmarshal([]byte(r.out), &planned); r.code != exitOK || err != nil || !strings.HasPrefix(r.out, "{\n  \"change\": {\n") {
			t.Errorf("projects create --plan-only --json = %d, %v, want one indented response:\n%s%s", r.code, err, r.out, r.err)
		}
		if r = run(t, env, "changes", "discard", planned.Change.ID); r.code != exitOK || !strings.Contains(r.out, "Discarded") {
			t.Errorf("changes discard %s = %d:\n%s%s", planned.Change.ID, r.code, r.out, r.err)
		}
	})

	t.Run("a choice once there are two projects", func(t *testing.T) {
		r := run(t, env, "environments", "create", "staging", "--yes")
		if r.code != exitUsage || !strings.Contains(r.err, "2 projects, default and shop") || !strings.Contains(r.err, "Pass --project with one of them") {
			t.Errorf("with no terminal = %d %q", r.code, r.err)
		}
		r = runIn(t, env, "3\nshop\ny\n", true, "environments", "create", "staging")
		if r.code != exitOK {
			t.Fatalf("environments create = %d:\n%s%s", r.code, r.out, r.err)
		}
		for _, want := range []string{"There are 2 projects. Which one?", "1  default", "2  shop", `"3" isn't one of them.`} {
			if !strings.Contains(r.err, want) {
				t.Errorf("the prompt didn't say %q:\n%s", want, r.err)
			}
		}
		if !strings.Contains(r.out, "+ environment staging") || !strings.Contains(r.out, "Applied chg_") {
			t.Errorf("environments create:\n%s", r.out)
		}
		r = run(t, env, "environments", "create", "qa", "--project", "default", "--yes")
		if r.code != exitOK || !strings.Contains(r.out, "Applied chg_") {
			t.Errorf("environments create --project = %d:\n%s%s", r.code, r.out, r.err)
		}
	})

	t.Run("a destructive plan needs a person", func(t *testing.T) {
		// A second server that joined, whose agent never connected: its
		// steps are planned from what it last reported, so a rule that
		// blocks SSH there is planned without a driver.
		r := run(t, env, "token", "create", "--data-dir", dir)
		ticket := regexp.MustCompile(`dockwell join (dwj1\.\S+)`).FindStringSubmatch(r.out)
		if ticket == nil {
			t.Fatalf("token create = %d:\n%s%s", r.code, r.out, r.err)
		}
		if r = run(t, env, "join", ticket[1], "--data-dir", t.TempDir(), "--name", "hz-2"); r.code != exitOK {
			t.Fatalf("join = %d:\n%s%s", r.code, r.out, r.err)
		}
		r = run(t, env, "firewall-rules", "create", "block-ssh", "--action", "deny", "--port", "22", "--yes")
		if r.code != exitUsage || !strings.Contains(r.err, "Pass --on with one of them") {
			t.Errorf("without --on, once there are two servers = %d %q", r.code, r.err)
		}
		r = run(t, env, "firewall-rules", "create", "block-ssh", "--action", "deny", "--port", "22", "--on", "hz-2", "--yes")
		if r.code != apicli.ExitNeedsPerson {
			t.Fatalf("firewall-rules create = %d:\n%s%s", r.code, r.out, r.err)
		}
		for _, want := range []string{
			"On hz-2, offline", "+ firewall.rule block-ssh", "May cut off remote access to hz-2, such as SSH.",
			"It destroys data or may cut off access",
		} {
			if !strings.Contains(r.out, want) {
				t.Errorf("the plan doesn't say %q:\n%s", want, r.out)
			}
		}
		id := changeRE.FindString(r.out)
		if !strings.Contains(r.err, id+" destroys data or may cut off access, so it needs a person") || !strings.Contains(r.err, "http://localhost:") ||
			!strings.Contains(r.err, "/changes/"+id) {
			t.Errorf("the refusal = %q", r.err)
		}
		r = run(t, env, "changes", "apply", id, "--yes")
		if r.code != apicli.ExitNeedsPerson || !strings.Contains(r.err, "needs a person") {
			t.Errorf("changes apply = %d:\n%s%s", r.code, r.out, r.err)
		}
		// So does one that gives a container a server path.
		r = run(t, env, "containers", "create", "logs", "--image", "nginx:1.27", "--mounts", `{"hostPath":"/srv/logs","target":"/logs"}`, "--on", "hz-2", "--yes")
		if r.code != apicli.ExitNeedsPerson || !strings.Contains(r.out, "Gives it access to hz-2: the server path /srv/logs (read-write). A person approves this.") ||
			!strings.Contains(r.err, "gives a container more access to its server, so it needs a person to approve it") ||
			!strings.Contains(r.err, "the CLI uses one even after dockwell login") {
			t.Errorf("containers create with a server path = %d:\n%s%s", r.code, r.out, r.err)
		}
		r = run(t, env, "api-tokens", "revoke", "tok_01JD6Z3R8M9QF2K7T4XW5BNC0V")
		if r.code != apicli.ExitNeedsPerson || !strings.Contains(r.err, "no API token may do") {
			t.Errorf("api-tokens revoke = %d %q", r.code, r.err)
		}
		r = run(t, env, "api-tokens", "list")
		if r.code != apicli.ExitNeedsPerson || !strings.Contains(r.err, "Open the panel at http://localhost:") {
			t.Errorf("api-tokens list = %d %q", r.code, r.err)
		}
	})

	t.Run("a read-only token", func(t *testing.T) {
		ro := map[string]string{apicli.EnvURL: url, apicli.EnvToken: readOnly, "XDG_CONFIG_HOME": t.TempDir()}
		if r := run(t, ro, "changes", "list"); r.code != exitOK {
			t.Errorf("changes list = %d %q", r.code, r.err)
		}
		r := run(t, ro, "projects", "create", "wiki", "--yes")
		if r.code != exitError || !strings.Contains(r.err, "your API token is read-only, so dockwell projects create can't make changes") {
			t.Errorf("projects create = %d %q", r.code, r.err)
		}
	})

	t.Run("login and logout", func(t *testing.T) {
		config := t.TempDir()
		none := map[string]string{"XDG_CONFIG_HOME": config, "HOME": t.TempDir(), envDataDir: t.TempDir()}
		r := runIn(t, none, secret+"\n", false, "login", url)
		if r.code != exitOK || !strings.Contains(r.out, "Signed in to "+url+" with dwt_") {
			t.Fatalf("login = %d:\n%s%s", r.code, r.out, r.err)
		}
		info, err := os.Stat(filepath.Join(config, "dockwell", "config.json"))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("the config file = %v, %v; want mode 0600", info, err)
		}
		if r = run(t, none, "servers", "list"); r.code != exitOK {
			t.Errorf("servers list after login = %d %q", r.code, r.err)
		}
		r = runIn(t, none, "dwt_not-a-token\n", false, "login", url)
		if r.code != exitUsage || !strings.Contains(r.err, "isn't a whole API token") {
			t.Errorf("login with a bad token = %d %q", r.code, r.err)
		}
		r = runIn(t, none, secret+"\n", false, "login", "http://203.0.113.7:7600")
		if r.code != exitUsage || !strings.Contains(r.err, "unencrypted") {
			t.Errorf("login over plain HTTP = %d %q", r.code, r.err)
		}
		if r = run(t, none, "logout"); r.code != exitOK || !strings.Contains(r.out, "Forgot the API token for "+url) {
			t.Errorf("logout = %d:\n%s%s", r.code, r.out, r.err)
		}
		if r = run(t, none, "servers", "list"); r.code != exitError || !strings.Contains(r.err, "dockwell login") {
			t.Errorf("servers list after logout = %d %q", r.code, r.err)
		}
	})

	t.Run("local access on the control server", func(t *testing.T) {
		local := map[string]string{"XDG_CONFIG_HOME": t.TempDir(), envDataDir: dir, envLogFormat: "json", "USER": "root"}
		cached := filepath.Join(dir, "control", "local-token.json")
		// A destructive command needs a person, which local access can say
		// without making a token.
		r := run(t, local, "api-tokens", "revoke", "tok_01JD6Z3R8M9QF2K7T4XW5BNC0V")
		if r.code != apicli.ExitNeedsPerson || !strings.Contains(r.err, "no API token may do") || !strings.Contains(r.err, "Sign in to the panel at http://localhost:") {
			t.Errorf("api-tokens revoke with local access = %d %q", r.code, r.err)
		}
		if _, err := os.Stat(cached); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("local access made a token to refuse a destructive command: %v", err)
		}
		r = run(t, local, "servers", "list")
		if r.code != exitOK || !strings.Contains(r.out, "(control)") {
			t.Fatalf("servers list with local access = %d:\n%s%s", r.code, r.out, r.err)
		}
		info, err := os.Stat(cached)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("the local token's file = %v, %v; want mode 0600", info, err)
		}
		// A control plane that moved, as after a restart on another port, is
		// found again, with the same token.
		var before localToken
		raw, _ := os.ReadFile(cached)
		_ = json.Unmarshal(raw, &before)
		moved := before
		moved.URL = "http://127.0.0.1:1"
		raw, _ = json.Marshal(moved)
		if err := os.WriteFile(cached, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if r := run(t, local, "servers", "list"); r.code != exitOK {
			t.Errorf("servers list once the control plane moved = %d %q", r.code, r.err)
		}
		var after localToken
		raw, _ = os.ReadFile(cached)
		_ = json.Unmarshal(raw, &after)
		if after.ID != before.ID || after.URL != before.URL {
			t.Errorf("after a move the local token is %s at %s, want %s at %s", after.ID, after.URL, before.ID, before.URL)
		}
		r = run(t, local, "projects", "create", "wiki", "--yes")
		if r.code != exitError || !strings.Contains(r.err, "local access on this server is read-only") || !strings.Contains(r.err, "dockwell login --local") {
			t.Errorf("a change with local access = %d %q", r.code, r.err)
		}
		r = run(t, local, "login", "--local")
		if r.code != exitOK || !strings.Contains(r.out, "Local access on this server can make changes until") {
			t.Fatalf("login --local = %d:\n%s%s", r.code, r.out, r.err)
		}
		r = run(t, local, "projects", "create", "wiki", "--yes")
		if r.code != exitOK || !strings.Contains(r.out, "Applied chg_") {
			t.Errorf("a change after login --local = %d:\n%s%s", r.code, r.out, r.err)
		}
		r = run(t, env, "changes", "list")
		if !strings.Contains(r.out, "Local access on") || !strings.Contains(r.out, "read-write") {
			t.Errorf("the change isn't by the local token:\n%s", r.out)
		}
		if r = run(t, local, "logout", "--local"); r.code != exitOK || !strings.Contains(r.out, "Revoked local access's token") {
			t.Errorf("logout --local = %d:\n%s%s", r.code, r.out, r.err)
		}
		r = run(t, local, "projects", "create", "docs", "--yes")
		if r.code != exitError || !strings.Contains(r.err, "read-only") {
			t.Errorf("a change after logout --local = %d %q", r.code, r.err)
		}
		r = run(t, map[string]string{"XDG_CONFIG_HOME": t.TempDir(), envDataDir: t.TempDir()}, "servers", "list")
		if r.code != exitError || !strings.Contains(r.err, "doesn't know which control plane to call") {
			t.Errorf("away from the control server = %d %q", r.code, r.err)
		}
	})
}

// TestEveryMethodHasACommand fails when a method of dockwell.v1 has no
// command, as when the schema changed and make generate didn't run.
func TestEveryMethodHasACommand(t *testing.T) {
	commands := map[string]*apicli.Method{}
	for _, m := range apicmd.Methods() {
		commands[m.Name] = m
	}
	var methods int
	protoregistry.GlobalFiles.RangeFilesByPackage("dockwell.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				md := sd.Methods().Get(j)
				methods++
				m, ok := commands[string(md.FullName())]
				switch {
				case !ok:
					t.Errorf("%s has no command. Run make generate", md.FullName())
				case m.Call == nil && !md.IsStreamingClient():
					t.Errorf("%s's command can't call it", md.FullName())
				}
				delete(commands, string(md.FullName()))
			}
		}
		return true
	})
	for name := range commands {
		t.Errorf("%s has a command but no method. Run make generate", name)
	}
	if methods == 0 {
		t.Fatal("no dockwell.v1 methods are registered")
	}
}

// TestCommandNamesDontCollide checks that no API command takes the name of
// one of this server's commands, or a name confusingly like one, such as
// tokens beside token.
func TestCommandNamesDontCollide(t *testing.T) {
	root, err := newRootCmd(IO{Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	var builtin, api []string
	for _, c := range root.Commands() {
		switch c.GroupID {
		case groupServer:
			builtin = append(builtin, c.Name())
		case apicli.GroupID:
			api = append(api, c.Name())
		}
	}
	for _, name := range api {
		for _, b := range builtin {
			if name == b || name == b+"s" || name+"s" == b {
				t.Errorf("the API command %s is too like dockwell %s. Name it apart with the (dockwell.v1.command) option", name, b)
			}
		}
	}
	seen := map[string]bool{}
	for _, c := range root.Commands() {
		if seen[c.Name()] {
			t.Errorf("two commands are named %s", c.Name())
		}
		seen[c.Name()] = true
	}
	if !slices.Contains(api, "servers") || !slices.Contains(api, "changes") || !slices.Contains(api, "drift") || !slices.Contains(api, "projects") {
		t.Errorf("the API commands are %v", api)
	}
}
