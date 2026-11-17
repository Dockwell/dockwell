// SPDX-License-Identifier: AGPL-3.0-only

package ownerstep

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

const password = "Tr0ub4dor&3xyzQ+/pass\"word<>"

// target sends requests to a test server, and runs commands as a fake container.
type target struct {
	addr string
	mu   sync.Mutex
	runs [][]string
	envs [][]string
	exec func(words []string, env []string) ([]byte, int)
}

func (t *target) Exec(_ context.Context, words []string, _ string, env []string) ([]byte, int, error) {
	t.mu.Lock()
	t.runs = append(t.runs, words)
	t.envs = append(t.envs, slices.Clone(env))
	t.mu.Unlock()
	if t.exec != nil {
		out, code := t.exec(words, env)
		return out, code, nil
	}
	return nil, 0, nil
}

func (t *target) Address(context.Context, uint32) (string, error) { return t.addr, nil }

func serve(t *testing.T, h http.HandlerFunc) *target {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	u, _ := url.Parse(s.URL)
	return &target{addr: u.Host}
}

func owner(steps ...*resourcev1.OwnerStep) *resourcev1.OwnerRun {
	return &resourcev1.OwnerRun{Service: "web", Email: "you@example.com", User: "you", Password: "OWNER_PASSWORD", Steps: steps}
}

func values() map[string]string {
	return map[string]string{"OWNER_EMAIL": "you@example.com", "OWNER_USER": "you", "OWNER_PASSWORD": password}
}

func post(path, body string, port uint32) *resourcev1.OwnerStep {
	return &resourcev1.OwnerStep{Step: &resourcev1.OwnerStep_Request{Request: &resourcev1.OwnerRequest{
		Method: http.MethodPost, Path: path, ContentType: "application/json", Body: body, Port: port,
	}}}
}

// What an app says back is scrubbed of every value the steps sent, in every form an
// answer may echo it, before it's cut: a password across the cut leaves no prefix of
// itself (ADR 0105, H3 and the re-check's M5).
func TestWhatAnAppSaysIsScrubbedThenCut(t *testing.T) {
	t.Parallel()
	o := owner()
	vals := values()
	captured := map[string]string{"TOKEN": "session-0123456789abcdef"}
	basic := base64.StdEncoding.EncodeToString([]byte("you:" + password))
	pad := strings.Repeat("x", MaxSaid-10)
	said := pad + password + " " + url.QueryEscape(password) + " " + jsonInside(password) + " " +
		html.EscapeString(password) + " " + url.PathEscape(password) + " Basic " + basic + " token " + captured["TOKEN"]
	got := Scrub([]byte(said), vals, captured, o)
	if len(got) > MaxSaid {
		t.Fatalf("Scrub kept %d bytes, more than %d", len(got), MaxSaid)
	}
	for _, leak := range []string{password[:6], "Tr0ub4", "session-0123", basic[:8]} {
		if strings.Contains(got, leak) {
			t.Errorf("what's kept holds %q: %q", leak, got[len(got)-40:])
		}
	}
	if !strings.HasSuffix(got, "[hidden]") && !strings.Contains(got, "[hidden]") {
		t.Errorf("nothing was hidden: %q", got)
	}
	// Everything the cut keeps whole is scrubbed too.
	all := Scrub([]byte("pw="+password+"&user=you%40example.com "+html.EscapeString(password)+" "+basic+" "+captured["TOKEN"]), vals, captured, o)
	for _, leak := range []string{password, html.EscapeString(password), basic, captured["TOKEN"], "you@example.com", url.QueryEscape("you@example.com")} {
		if strings.Contains(all, leak) {
			t.Errorf("Scrub kept %q in %q", leak, all)
		}
	}
}

// A request's values are written as its body's type writes them, and what it
// captures reaches the next step; a redirect fails the step, and isn't followed.
func TestStepsSendValuesAsTheirBodyWritesThem(t *testing.T) {
	t.Parallel()
	var got map[string]string
	var asked []string
	tg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/setup":
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &got); err != nil {
				t.Errorf("the body isn't JSON: %q", body)
			}
			_, _ = w.Write([]byte(`{"data":{"token":"tok-123456","id":7}}`))
		case "/after":
			w.WriteHeader(http.StatusNoContent)
		}
	})
	o := owner(
		post("/setup", `{"email":"${OWNER_EMAIL}","password":"${OWNER_PASSWORD}"}`, 3000),
		&resourcev1.OwnerStep{Step: &resourcev1.OwnerStep_Request{Request: &resourcev1.OwnerRequest{
			Method: http.MethodPut, Path: "/after", Port: 3000, Headers: map[string]string{"Authorization": "Bearer ${TOKEN}"},
		}}},
	)
	o.Steps[0].GetRequest().Capture = map[string]string{"TOKEN": "data.token"}
	res := Run(t.Context(), tg, o, values(), false)
	if !res.Made {
		t.Fatalf("Run = %+v, want made", res)
	}
	if got["password"] != password || got["email"] != "you@example.com" {
		t.Errorf("the body sent %v", got)
	}
	if len(asked) != 2 || asked[1] != "PUT /after Bearer tok-123456" {
		t.Errorf("the steps asked %v, want the captured token on the second", asked)
	}
}

func TestARedirectFailsTheStepAndIsntFollowed(t *testing.T) {
	t.Parallel()
	var followed bool
	tg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusMovedPermanently)
	})
	res := Run(t.Context(), tg, owner(post("/setup", `{"password":"${OWNER_PASSWORD}"}`, 80)), values(), false)
	if res.Made || res.Step != 1 || !strings.Contains(res.Said, "redirect") {
		t.Errorf("Run = %+v, want step 1 failed on a redirect", res)
	}
	if followed {
		t.Error("the step followed the redirect, sending its body elsewhere")
	}
}

// A command reads every value from its environment: its words are as the entry
// wrote them, so no password shows to whoever lists the server's processes.
func TestACommandTakesValuesInItsEnvironmentNeverItsWords(t *testing.T) {
	t.Parallel()
	tg := &target{exec: func([]string, []string) ([]byte, int) { return []byte("made"), 0 }}
	words := []string{"sh", "-c", `make-owner --username "$OWNER_USER" --email "$OWNER_EMAIL" --password-from-env OWNER_PASSWORD`}
	o := owner(&resourcev1.OwnerStep{Step: &resourcev1.OwnerStep_Command{Command: &resourcev1.OwnerCommand{Words: words, User: "git"}}})
	if res := Run(t.Context(), tg, o, values(), false); !res.Made {
		t.Fatalf("Run = %+v", res)
	}
	for _, w := range tg.runs[0] {
		if strings.Contains(w, password) {
			t.Errorf("a word holds the password: %q", w)
		}
	}
	if !strings.Contains(strings.Join(tg.envs[0], "\n"), "OWNER_PASSWORD="+password) {
		t.Error("the password isn't in the command's environment")
	}
	if err := Check([]*resourcev1.OwnerStep{{Step: &resourcev1.OwnerStep_Command{Command: &resourcev1.OwnerCommand{Words: []string{"app", "--password", "${OWNER_PASSWORD}"}}}}}); err == nil {
		t.Error("Check takes a command that names ${OWNER_PASSWORD} in its words")
	}
}

// A failed command says what it printed, scrubbed.
func TestAFailedCommandSaysWhatItPrintedScrubbed(t *testing.T) {
	t.Parallel()
	tg := &target{exec: func(_ []string, env []string) ([]byte, int) {
		return []byte("refused the password " + password + " for you@example.com"), 1
	}}
	o := owner(&resourcev1.OwnerStep{Step: &resourcev1.OwnerStep_Command{Command: &resourcev1.OwnerCommand{Words: []string{"make-owner"}}}})
	res := Run(t.Context(), tg, o, values(), false)
	if res.Made || res.Step != 1 {
		t.Fatalf("Run = %+v, want step 1 failed", res)
	}
	if strings.Contains(res.Said, password) || strings.Contains(res.Said, "you@example.com") || !strings.Contains(res.Said, "[hidden]") {
		t.Errorf("it said %q", res.Said)
	}
}

// Try again reads done first, so a run that made the account and lost its answer
// finishes, rather than failing on "already exists" for ever (the re-check's M6).
func TestTryAgainReadsDoneFirst(t *testing.T) {
	t.Parallel()
	var setups int
	tg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/setup":
			setups++
			w.WriteHeader(http.StatusConflict)
		case "/me":
			w.WriteHeader(http.StatusOK)
		}
	})
	o := owner(post("/setup", `{}`, 80))
	o.Done = &resourcev1.OwnerCheck{Request: &resourcev1.OwnerRequest{Path: "/me", Port: 80}}
	if res := Run(t.Context(), tg, o, values(), true); !res.Made || setups != 0 {
		t.Errorf("Run with check first = %+v after %d setups, want made with none", res, setups)
	}
	if res := Run(t.Context(), tg, o, values(), false); res.Made || setups != 1 {
		t.Errorf("Run without = %+v after %d setups, want the setup's 409", res, setups)
	}
}

// An invite's steps succeeding isn't the owner being made: it waits until done
// reads it (the re-check's M7).
func TestAnInviteWaitsUntilDoneReadsIt(t *testing.T) {
	t.Parallel()
	registered := false
	tg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users" && !registered {
			_, _ = w.Write([]byte(`[{"email":"you@example.com","status":"invited"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"email":"you@example.com","status":"registered"}]`))
	})
	o := owner(post("/invite", `{"email":"${OWNER_EMAIL}"}`, 80))
	o.MadeWhenDone = true
	o.Done = &resourcev1.OwnerCheck{Request: &resourcev1.OwnerRequest{Path: "/users", Port: 80}, Contains: `"registered"`}
	if res := Run(t.Context(), tg, o, values(), false); res.Made || !res.Waiting {
		t.Errorf("Run = %+v, want waiting", res)
	}
	registered = true
	if res := Run(t.Context(), tg, o, values(), true); !res.Made {
		t.Errorf("Run once registered = %+v, want made", res)
	}
}

// Sign-ups that close through the app's settings are part of the owner being made:
// where the check still reads them open, it isn't.
func TestSignupsStepsMustReadClosed(t *testing.T) {
	t.Parallel()
	closed := false
	tg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/settings":
			closed = r.Method == http.MethodPatch
		case "/status":
			if !closed {
				w.WriteHeader(http.StatusForbidden)
			}
		}
	})
	o := owner(post("/setup", `{}`, 80))
	o.SignupsCheck = &resourcev1.OwnerCheck{Request: &resourcev1.OwnerRequest{Path: "/status", Port: 80}}
	o.SignupsSteps = []*resourcev1.OwnerStep{{Step: &resourcev1.OwnerStep_Request{Request: &resourcev1.OwnerRequest{Method: http.MethodPut, Path: "/settings", Port: 80}}}}
	if res := Run(t.Context(), tg, o, values(), false); res.Made {
		t.Error("made with sign-ups still open")
	}
	o.SignupsSteps[0].GetRequest().Method = http.MethodPatch
	if res := Run(t.Context(), tg, o, values(), false); !res.Made {
		t.Errorf("Run = %+v, want made once they read closed", res)
	}
}

func TestCheckRefusesWhatAnAgentCantRun(t *testing.T) {
	t.Parallel()
	for name, step := range map[string]*resourcev1.OwnerStep{
		"no port":    post("/", "", 0),
		"a redirect": {Step: &resourcev1.OwnerStep_Request{Request: &resourcev1.OwnerRequest{Port: 80, ExpectStatus: 302}}},
		"TRACE":      {Step: &resourcev1.OwnerStep_Request{Request: &resourcev1.OwnerRequest{Port: 80, Method: http.MethodTrace}}},
		"no words":   {Step: &resourcev1.OwnerStep_Command{Command: &resourcev1.OwnerCommand{}}},
		"a one-off":  {Step: &resourcev1.OwnerStep_Command{Command: &resourcev1.OwnerCommand{Words: []string{"x"}, OneOff: true}}},
		"nothing":    {},
	} {
		if err := Check([]*resourcev1.OwnerStep{step}); err == nil {
			t.Errorf("%s: Check takes it", name)
		}
	}
}

// A command's output can be captured for the steps after it, as Gitea's made-up
// password is, and a check refuses a command that names a secret in its words, where
// the shell would write the value into the command's own arguments (ADR 0105, the
// review's M2).
func TestACommandCapturesWhatItPrintsAndNamesNoSecret(t *testing.T) {
	t.Parallel()
	var got map[string]string
	tg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		if r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("you:Rand0mMadeUp")) {
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	tg.exec = func([]string, []string) ([]byte, int) {
		return []byte("New user 'you' has been successfully created!\ngenerated random password is 'Rand0mMadeUp'\n"), 0
	}
	create := &resourcev1.OwnerStep{Step: &resourcev1.OwnerStep_Command{Command: &resourcev1.OwnerCommand{
		Words:   []string{"sh", "-c", `gitea admin user create --admin --username "$OWNER_USER" --email "$OWNER_EMAIL" --random-password`},
		Capture: map[string]string{"MADE_UP": `random password is '([^']+)'`},
	}}}
	set := post("/api/v1/admin/users/${OWNER_USER}", `{"password":"${OWNER_PASSWORD}","source_id":0,"login_name":"${OWNER_USER}","must_change_password":false}`, 3000)
	set.GetRequest().Method = http.MethodPatch
	// The request signs in as the owner with the password the app made up.
	set.GetRequest().Headers = map[string]string{"Authorization": "Basic ${DOCKWELL_BASIC_MADE_UP}"}
	o := owner(create, set)
	if res := Run(t.Context(), tg, o, values(), false); !res.Made {
		t.Fatalf("Run = %+v", res)
	}
	if got["password"] != password {
		t.Errorf("the real password went %q", got["password"])
	}
	for _, w := range tg.runs[0] {
		if strings.Contains(w, password) {
			t.Errorf("the command line holds the password: %q", w)
		}
	}
	if err := CheckSecretWords([]*resourcev1.OwnerStep{create}, []string{"OWNER_PASSWORD"}); err != nil {
		t.Errorf("CheckSecretWords refuses a command with no secret: %v", err)
	}
	argv := &resourcev1.OwnerStep{Step: &resourcev1.OwnerStep_Command{Command: &resourcev1.OwnerCommand{Words: []string{"sh", "-c", `gitea admin user create --password "$OWNER_PASSWORD"`}}}}
	if err := CheckSecretWords([]*resourcev1.OwnerStep{argv}, []string{"OWNER_PASSWORD"}); err == nil {
		t.Error("CheckSecretWords takes a command that passes the password as an argument")
	}
}

// Every request carries where the app is reached, as its Host and its
// X-Forwarded-Proto, whatever the entry says (the review's M4).
func TestARequestSaysWhereTheAppIsReached(t *testing.T) {
	t.Parallel()
	var host, proto string
	tg := serve(t, func(_ http.ResponseWriter, r *http.Request) { host, proto = r.Host, r.Header.Get("X-Forwarded-Proto") })
	o := owner(post("/setup", `{}`, 80))
	o.GetSteps()[0].GetRequest().Headers = map[string]string{"Host": "elsewhere.example.com"}
	o.Host, o.Https = "blog.example.com", true
	if res := Run(t.Context(), tg, o, values(), false); !res.Made {
		t.Fatalf("Run = %+v", res)
	}
	if host != "blog.example.com" || proto != "https" {
		t.Errorf("the request said Host %q, X-Forwarded-Proto %q", host, proto)
	}
}

// A request goes to the container alone, through no proxy the agent's environment
// names, which would see the owner's password (the review's M3). Go proxies no
// loopback address, so the container here is on an address of this machine that
// isn't loopback.
func TestARequestGoesThroughNoProxy(t *testing.T) {
	if tr, ok := direct.Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Fatal("a step's requests can go through a proxy")
	}
	var ip net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			ip = n.IP
			break
		}
	}
	if ip == nil {
		t.Skip("this machine has no address that isn't loopback")
	}
	proxied := false
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { proxied = true; w.WriteHeader(http.StatusTeapot) }))
	t.Cleanup(proxy.Close)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	l, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Skipf("can't listen on %s: %v", ip, err)
	}
	var body string
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { b, _ := io.ReadAll(r.Body); body = string(b) }))
	s.Listener = l
	s.Start()
	t.Cleanup(s.Close)
	tg := &target{addr: l.Addr().String()}
	if res := Run(t.Context(), tg, owner(post("/setup", `{"password":"${OWNER_PASSWORD}"}`, 80)), values(), false); !res.Made {
		t.Fatalf("Run = %+v", res)
	}
	if proxied || !strings.Contains(body, "Tr0ub4") {
		t.Errorf("the request went through the proxy %v, and the container got %q", proxied, body)
	}
}

// A failure says the error as well as what the app answered, scrubbed (the
// review's L4).
func TestAFailureSaysItsErrorAndTheAnswer(t *testing.T) {
	t.Parallel()
	tg := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("database is locked for " + password))
	})
	res := Run(t.Context(), tg, owner(post("/setup", `{}`, 80)), values(), false)
	if res.Made || !strings.Contains(res.Said, "it answered 500") || !strings.Contains(res.Said, "database is locked") || strings.Contains(res.Said, "Tr0ub4") {
		t.Errorf("a failure said %q", res.Said)
	}
}

// A command names no secret in its words in any form: "$NAME", a bare name as
// printenv takes it, the owner's Basic credential or a captured value's, in a step,
// a check that skips one, or a check of its own (the re-check's N3).
func TestNoCommandNamesASecretInItsWords(t *testing.T) {
	t.Parallel()
	command := func(words ...string) *resourcev1.OwnerCommand { return &resourcev1.OwnerCommand{Words: words} }
	secrets := []string{"OWNER_PASSWORD"}
	for name, c := range map[string]*resourcev1.OwnerCommand{
		"$NAME":    command("sh", "-c", `app --password "$OWNER_PASSWORD"`),
		"printenv": command("sh", "-c", `app --password "$(printenv OWNER_PASSWORD)"`),
		"basic":    command("sh", "-c", `curl -H "Authorization: Basic $DOCKWELL_OWNER_BASIC" localhost`),
		"captured": command("sh", "-c", `curl -H "Authorization: Basic $DOCKWELL_BASIC_MADE_UP" localhost`),
	} {
		if err := CheckSecretWords([]*resourcev1.OwnerStep{{Step: &resourcev1.OwnerStep_Command{Command: c}}}, secrets); err == nil {
			t.Errorf("%s: a step's command passes", name)
		}
		if err := CheckSecretCheck(&resourcev1.OwnerCheck{Command: c}, secrets); err == nil {
			t.Errorf("%s: a check's command passes", name)
		}
		unless := &resourcev1.OwnerStep{Step: &resourcev1.OwnerStep_Request{Request: &resourcev1.OwnerRequest{Port: 80}}, Unless: &resourcev1.OwnerCheck{Command: c}}
		if err := CheckSecretWords([]*resourcev1.OwnerStep{unless}, secrets); err == nil {
			t.Errorf("%s: a step's unless passes", name)
		}
	}
	if err := CheckSecretCheck(&resourcev1.OwnerCheck{Command: command("sh", "-c", `app user exists "$OWNER_USER"`)}, secrets); err != nil {
		t.Errorf("a check that names no secret: %v", err)
	}
}

// A step skipped by its unless lets a second try finish what a first stopped in, as
// Gitea's create when the user exists and its password's PATCH failed (the
// re-check's N6).
func TestASecondTryFinishesAfterAPartialRun(t *testing.T) {
	t.Parallel()
	var created, patched int
	exists, patchOK := false, false
	tg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/users/you" && exists:
		case r.URL.Path == "/users/you":
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/token":
			_, _ = w.Write([]byte(`{"token":"tok-0123456789"}`))
		case r.URL.Path == "/password" && patchOK:
			patched++
		case r.URL.Path == "/password":
			w.WriteHeader(http.StatusInternalServerError)
		case r.URL.Path == "/me":
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	tg.exec = func([]string, []string) ([]byte, int) {
		if exists {
			return []byte("user already exists"), 1
		}
		created++
		exists = true
		return []byte("created"), 0
	}
	create := &resourcev1.OwnerStep{
		Step:   &resourcev1.OwnerStep_Command{Command: &resourcev1.OwnerCommand{Words: []string{"make-user"}}},
		Unless: &resourcev1.OwnerCheck{Request: &resourcev1.OwnerRequest{Path: "/users/${OWNER_USER}", Port: 80}},
	}
	token := post("/token", `{}`, 80)
	token.GetRequest().Capture = map[string]string{"TOKEN": "token"}
	set := post("/password", `{"password":"${OWNER_PASSWORD}"}`, 80)
	o := owner(create, token, set)
	o.Done = &resourcev1.OwnerCheck{Request: &resourcev1.OwnerRequest{Path: "/me", Port: 80}}
	if res := Run(t.Context(), tg, o, values(), false); res.Made || res.Step != 3 {
		t.Fatalf("the first try = %+v, want step 3 failed", res)
	}
	patchOK = true
	if res := Run(t.Context(), tg, o, values(), true); !res.Made {
		t.Fatalf("the second try = %+v, want made", res)
	}
	if created != 1 || patched != 1 {
		t.Errorf("made %d users and set %d passwords, want 1 and 1", created, patched)
	}
}
