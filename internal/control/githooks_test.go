// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func (g *gitBench) hooks() dockwellv1connect.GitHookServiceClient {
	return dockwellv1connect.NewGitHookServiceClient(g.b, g.b.origin, connect.WithProtoJSON())
}

// hook turns deploy on push on for a service, with a passkey, and gives its URL
// and secret.
func (g *gitBench) hook(t *testing.T, serviceID string) (url, secret string) {
	t.Helper()
	res, err := g.hooks().CreateGitHook(t.Context(), connect.NewRequest(&dockwellv1.CreateGitHookRequest{
		Id: serviceID, Proof: stepUpFor(t, g.b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_GIT_HOOK, serviceID),
	}))
	if err != nil {
		t.Fatalf("turning deploy on push on: %v", err)
	}
	return res.Msg.GetHook().GetUrl(), res.Msg.GetSecret()
}

// delivery is what a provider sends a hook.
type delivery struct {
	id, event, secret string
	body              []byte
	// headers replace what delivery sends by default, as GitHub does; an empty
	// value leaves one out.
	headers map[string]string
}

// pushBody is a push's payload, as GitHub sends it, of ref to after in repo.
func pushBody(repo, ref, after string) []byte {
	b, _ := json.Marshal(map[string]any{
		"ref": ref, "after": after, "before": strings.Repeat("0", 40), "deleted": false,
		"repository": map[string]any{"clone_url": repo, "html_url": strings.TrimSuffix(repo, ".git"), "default_branch": "main"},
		"commits":    []map[string]any{{"id": after, "message": "Ship it"}},
	})
	return b
}

// send sends d to url, signed as GitHub signs, and gives the answer's status and
// text.
func send(t *testing.T, url string, d delivery) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(d.body))
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte(d.secret))
	mac.Write(d.body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", d.event)
	req.Header.Set("X-GitHub-Delivery", d.id)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("User-Agent", "GitHub-Hookshot/0a1b2c3")
	for k, v := range d.headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	text, _ := io.ReadAll(res.Body)
	return res.StatusCode, strings.TrimSpace(string(text)), res.Header
}

// outcome waits for what a delivery came to, until it is want.
func (g *gitBench) outcome(t *testing.T, serviceID, deliveryID string, want dockwellv1.GitHookOutcome) *dockwellv1.GitHookDelivery {
	t.Helper()
	var last *dockwellv1.GitHookDelivery
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		res, err := g.hooks().GetGitHook(t.Context(), connect.NewRequest(&dockwellv1.GetGitHookRequest{Id: serviceID}))
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range res.Msg.GetHook().GetDeliveries() {
			if d.GetDeliveryId() == deliveryID {
				last = d
			}
		}
		if last.GetOutcome() == want {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("delivery %s came to %v, not %v", deliveryID, last, want)
	return nil
}

// ended waits for a change to end.
func (g *gitBench) ended(t *testing.T, changeID string) *dockwellv1.Change {
	t.Helper()
	return waitWatch(t, watch(t, g.c, changeID)).last()
}

// A push to the branch an app builds deploys what the branch names, as the hook,
// on the push surface, and leaves that on the timeline. A delivery sent again does
// nothing, and so does one that names a commit the branch doesn't: Dockwell reads
// the branch itself. Pushes to another branch and GitHub's ping are taken and do
// nothing.
func TestAPushDeploysWhatTheBranchNames(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile, "app.js": "1"})
		g.done(t, g.propose(t, g.buildEdit("main", nil)))
		web := g.service(t, "", "web")

		if _, err := g.hooks().CreateGitHook(t.Context(), connect.NewRequest(&dockwellv1.CreateGitHookRequest{Id: web.GetId()})); reasonOf(err) != authapi.ReasonNeedsStepUp {
			t.Fatalf("turning deploy on push on without a passkey: %v", err)
		}
		url, secret := g.hook(t, web.GetId())
		if !strings.HasPrefix(url, g.b.origin+"/hooks/git/ghk_") || len(secret) != 64 {
			t.Fatalf("the hook is at %s with secret %q", url, secret)
		}
		if _, err := g.hooks().CreateGitHook(t.Context(), connect.NewRequest(&dockwellv1.CreateGitHookRequest{
			Id: web.GetId(), Proof: stepUpFor(t, g.b, dockwellv1.StepUpAction_STEP_UP_ACTION_CREATE_GIT_HOOK, web.GetId()),
		})); connect.CodeOf(err) != connect.CodeAlreadyExists {
			t.Errorf("a second hook: %v", err)
		}

		second := g.repo.Commit("main", "Second", map[string]string{"Dockerfile": webDockerfile, "app.js": "2"})
		code, text, _ := send(t, url, delivery{id: "d-1", event: "push", secret: secret, body: pushBody(g.repo.URL, "refs/heads/main", second)})
		if code != http.StatusAccepted || !strings.Contains(text, "deploys what it names") {
			t.Fatalf("a push: %d %q", code, text)
		}
		d := g.outcome(t, web.GetId(), "d-1", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_DEPLOYING)
		if d.GetCommit() != second || d.GetChangeId() == "" || d.GetProvider() != "github" || d.GetRef() != "refs/heads/main" {
			t.Fatalf("the push came to %v", d)
		}
		c := g.ended(t, d.GetChangeId())
		if c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("the push's deploy ended %v: %s", c.GetState(), c.GetError())
		}
		if c.GetAppliedVia() != dockwellv1.Surface_SURFACE_PUSH || c.GetAppliedBy().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_HOOK ||
			c.GetAppliedBy().GetName() != "Deploy on push for web" || !strings.Contains(c.GetSummary(), "pushed to main") {
			t.Errorf("the deploy was applied by %v via %v, as %q", c.GetAppliedBy(), c.GetAppliedVia(), c.GetSummary())
		}
		if got := g.service(t, "", "web").GetSpec().GetBuild().GetGit().GetCommit(); got != second {
			t.Errorf("web builds %s, not the commit pushed, %s", got, second)
		}

		// The same delivery again does nothing.
		code, text, _ = send(t, url, delivery{id: "d-1", event: "push", secret: secret, body: pushBody(g.repo.URL, "refs/heads/main", second)})
		if code != http.StatusAccepted || !strings.Contains(text, "already") {
			t.Errorf("the delivery again: %d %q", code, text)
		}
		// A new one that names a commit the branch doesn't, as one forged with a
		// stolen secret could, builds what the branch names, which web runs.
		forged := strings.Repeat("ab", 20)
		send(t, url, delivery{id: "d-2", event: "push", secret: secret, body: pushBody(g.repo.URL, "refs/heads/main", forged)})
		if d := g.outcome(t, web.GetId(), "d-2", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_NOTHING_NEW); d.GetCommit() != second || d.GetChangeId() != "" {
			t.Errorf("a push naming a commit main doesn't: %v", d)
		}

		g.repo.Commit("dev", "Elsewhere", map[string]string{"Dockerfile": webDockerfile, "app.js": "dev"})
		send(t, url, delivery{id: "d-3", event: "push", secret: secret, body: pushBody(g.repo.URL, "refs/heads/dev", second)})
		if d := g.outcome(t, web.GetId(), "d-3", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_IGNORED); !strings.Contains(d.GetDetail(), "refs/heads/dev") && !strings.Contains(d.GetDetail(), "dev,") {
			t.Errorf("a push to another branch: %v", d)
		}
		send(t, url, delivery{id: "d-4", event: "push", secret: secret, body: pushBody("https://git.example.com/acme/other.git", "refs/heads/main", second)})
		g.outcome(t, web.GetId(), "d-4", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_IGNORED)
		code, text, _ = send(t, url, delivery{id: "d-5", event: "ping", secret: secret, body: []byte(`{"zen":"Keep it logically awesome.","hook_id":1}`)})
		if code != http.StatusAccepted || !strings.Contains(text, "The hook works") {
			t.Errorf("a ping: %d %q", code, text)
		}
		g.outcome(t, web.GetId(), "d-5", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_IGNORED)

		// The timeline has the pushes that did something, and the change.
		pushes := listEvents(t, dockwellv1connect.NewEventServiceClient(g.b, g.b.origin), &dockwellv1.ListEventsRequest{
			Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_PUSH}, ResourceId: web.GetId(),
		})
		var outcomes []string
		for _, e := range pushes {
			if e.GetSubject().GetName() != "web" || e.GetActor().GetKind() != dockwellv1.ActorKind_ACTOR_KIND_HOOK || e.GetSurface() != dockwellv1.Surface_SURFACE_PUSH {
				t.Errorf("a push's event is %v", e)
			}
			outcomes = append(outcomes, e.GetPush().GetDelivery().GetDeliveryId()+" "+e.GetPush().GetDelivery().GetOutcome().String())
		}
		if want := []string{"d-2 GIT_HOOK_OUTCOME_NOTHING_NEW", "d-1 GIT_HOOK_OUTCOME_DEPLOYING"}; strings.Join(outcomes, ", ") != strings.Join(want, ", ") {
			t.Errorf("the timeline's pushes are %v, want %v", outcomes, want)
		}
	})
}

// The receiver faces the internet: a delivery that isn't signed with the hook's
// secret is refused, and one for a hook there isn't gets the same answer; one too
// large, or not JSON, or with no ID is refused before its signature is checked;
// and one address may send only so many. A hook turned off answers as one there
// never was.
func TestAHookTakesOnlyWhatItsSecretSigned(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		head := g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile})
		g.done(t, g.propose(t, g.buildEdit("main", nil)))
		web := g.service(t, "", "web")
		url, secret := g.hook(t, web.GetId())
		body := pushBody(g.repo.URL, "refs/heads/main", head)

		code, wrong, _ := send(t, url, delivery{id: "r-1", event: "push", secret: "not-the-secret", body: body})
		if code != http.StatusUnauthorized {
			t.Errorf("a wrong signature: %d %q", code, wrong)
		}
		none := url[:strings.LastIndex(url, "/")+1] + "ghk_01JD6Z3R8M9QF2K7T4XW5BNC0V"
		if code, text, _ := send(t, none, delivery{id: "r-2", event: "push", secret: secret, body: body}); code != http.StatusUnauthorized || text != wrong {
			t.Errorf("a hook there isn't: %d %q, want the answer a wrong signature gets, %q", code, text, wrong)
		}
		if code, text, _ := send(t, url, delivery{id: "r-3", event: "push", secret: secret, body: body, headers: map[string]string{"X-Hub-Signature-256": ""}}); code != http.StatusUnauthorized {
			t.Errorf("no signature: %d %q", code, text)
		}
		if code, text, _ := send(t, url, delivery{id: "r-4", event: "push", secret: secret, body: body, headers: map[string]string{"Content-Type": "text/plain"}}); code != http.StatusUnsupportedMediaType || !strings.Contains(text, "application/json") {
			t.Errorf("text: %d %q", code, text)
		}
		// GitHub's hooks send a form unless told JSON, and sign the form.
		form := []byte("payload=" + neturl.QueryEscape(string(body)))
		if code, text, _ := send(t, url, delivery{id: "r-8", event: "push", secret: secret, body: form, headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}}); code != http.StatusAccepted {
			t.Errorf("GitHub's form: %d %q", code, text)
		}
		if d := g.outcome(t, web.GetId(), "r-8", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_NOTHING_NEW); d.GetProvider() != "github" || d.GetCommit() != head {
			t.Errorf("GitHub's form came to %v", d)
		}
		// Gitea's isn't taken as a form, as it doesn't sign one.
		if code, text, _ := send(t, url, delivery{id: "r-9", event: "push", secret: secret, body: form, headers: map[string]string{
			"Content-Type": "application/x-www-form-urlencoded", "X-Gitea-Signature": "00", "X-Gitea-Event": "push", "X-Gitea-Delivery": "r-9",
		}}); code != http.StatusUnsupportedMediaType {
			t.Errorf("Gitea's form: %d %q", code, text)
		}
		large := append(bytes.Repeat([]byte(" "), 1<<20), body...)
		if code, text, _ := send(t, url, delivery{id: "r-5", event: "push", secret: secret, body: large}); code != http.StatusRequestEntityTooLarge {
			t.Errorf("more than 1 MiB: %d %q", code, text)
		}
		if code, text, _ := send(t, url, delivery{event: "push", secret: secret, body: body}); code != http.StatusBadRequest || !strings.Contains(text, "X-GitHub-Delivery") {
			t.Errorf("no delivery ID: %d %q", code, text)
		}
		// Gitea signs with its own header, and sends GitHub's too.
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		if code, text, _ := send(t, url, delivery{id: "r-6", event: "push", secret: "not-the-secret", body: body, headers: map[string]string{
			"X-Gitea-Signature": hex.EncodeToString(mac.Sum(nil)), "X-Gitea-Event": "push", "X-Gitea-Delivery": "r-6",
		}}); code != http.StatusAccepted {
			t.Errorf("Gitea's delivery: %d %q", code, text)
		}
		if d := g.outcome(t, web.GetId(), "r-6", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_NOTHING_NEW); d.GetProvider() != "gitea" {
			t.Errorf("Gitea's delivery came to %v", d)
		}

		// A refused signature is in the audit log once, however many come.
		var refused int
		if err := g.f.store.View(t.Context(), func(tx *store.Tx) error {
			list, err := tx.ListEvents(t.Context(), 1000)
			for _, e := range list {
				if e.Action == "git_hook.refused" {
					refused++
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if refused != 1 {
			t.Errorf("the audit log has %d refused deliveries, want 1", refused)
		}

		if _, err := g.hooks().DeleteGitHook(t.Context(), connect.NewRequest(&dockwellv1.DeleteGitHookRequest{Id: web.GetId()})); err != nil {
			t.Fatal(err)
		}
		if code, text, _ := send(t, url, delivery{id: "r-7", event: "push", secret: secret, body: body}); code != http.StatusUnauthorized || text != wrong {
			t.Errorf("a hook turned off: %d %q", code, text)
		}
		got, err := g.hooks().GetGitHook(t.Context(), connect.NewRequest(&dockwellv1.GetGitHookRequest{Id: web.GetId()}))
		if err != nil || got.Msg.GetHook() != nil {
			t.Errorf("a service with deploy on push off has %v, %v", got.Msg.GetHook(), err)
		}
		// What was done to the hook is the app's, by its name, once the hook is
		// gone too; and what its pushes did is by "Deploy on push".
		eventsAPI := dockwellv1connect.NewEventServiceClient(g.b, g.b.origin)
		var done []string
		for _, e := range listEvents(t, eventsAPI, &dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_ACCESS}}) {
			if strings.HasPrefix(e.GetAction(), "git_hook.") && e.GetSubject().GetId() == web.GetId() {
				done = append(done, e.GetAction()+" "+e.GetSubject().GetName())
			}
		}
		if want := "git_hook.delete web, git_hook.create web"; strings.Join(done, ", ") != want {
			t.Errorf("the app's access events are %q, want %q", done, want)
		}
		for _, e := range listEvents(t, eventsAPI, &dockwellv1.ListEventsRequest{Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_PUSH}, ResourceId: web.GetId()}) {
			if e.GetActor().GetName() != "Deploy on push" {
				t.Errorf("a push by a hook that's gone is by %q", e.GetActor().GetName())
			}
		}

		// One address sends 30 a minute, at once at most.
		var limited http.Header
		for i := range 40 {
			code, _, header := send(t, none, delivery{id: fmt.Sprintf("flood-%d", i), event: "push", secret: secret, body: body})
			if code == http.StatusTooManyRequests {
				limited = header
				break
			}
		}
		if limited == nil || limited.Get("Retry-After") == "" {
			t.Errorf("40 deliveries at once from one address were all taken: %v", limited)
		}
	})
}

// twoWriters is an app that builds from the bench's repository, has one volume,
// and asks for its new container to start before the old one stops, so a deploy
// lets two containers write to one place, which a person approves.
func (g *gitBench) twoWriters() *dockwellv1.Edit {
	e := g.buildEdit("main", nil)
	app := e.GetSpec().GetService().GetApp()
	app.Ports = nil
	app.Mounts = []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data"}}
	app.Rollout = &resourcev1.Rollout{Order: resourcev1.RolloutOrder_ROLLOUT_ORDER_START_FIRST}
	return e
}

// A push has an API token's rights: a plan no token may apply waits for a person,
// and a newer push's plan takes its place.
func TestAPushLeavesWhatNeedsAPersonForOne(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile})
		g.done(t, g.propose(t, g.twoWriters(), &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "volume", Name: "data"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Volume{Volume: &resourcev1.Volume{}}},
		}))
		web := g.service(t, "", "web")
		url, secret := g.hook(t, web.GetId())

		second := g.repo.Commit("main", "Second", map[string]string{"Dockerfile": webDockerfile, "app.js": "2"})
		send(t, url, delivery{id: "p-1", event: "push", secret: secret, body: pushBody(g.repo.URL, "refs/heads/main", second)})
		first := g.outcome(t, web.GetId(), "p-1", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_NEEDS_PERSON)
		if !strings.Contains(first.GetDetail(), "write to one place") {
			t.Errorf("why it waits: %q", first.GetDetail())
		}
		got, err := g.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: first.GetChangeId()}))
		if err != nil || got.Msg.GetChange().GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
			t.Fatalf("the plan that needs a person is %v, %v", got.Msg.GetChange().GetState(), err)
		}

		third := g.repo.Commit("main", "Third", map[string]string{"Dockerfile": webDockerfile, "app.js": "3"})
		send(t, url, delivery{id: "p-2", event: "push", secret: secret, body: pushBody(g.repo.URL, "refs/heads/main", third)})
		next := g.outcome(t, web.GetId(), "p-2", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_NEEDS_PERSON)
		if next.GetCommit() != third {
			t.Errorf("the newer plan builds %s, not %s", next.GetCommit(), third)
		}
		got, err = g.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: first.GetChangeId()}))
		if err != nil || got.Msg.GetChange().GetState() != dockwellv1.ChangeState_CHANGE_STATE_DISCARDED {
			t.Errorf("the older plan is %v, %v, not discarded", got.Msg.GetChange().GetState(), err)
		}
		// A person applies it, from the panel.
		plan, err := g.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: next.GetChangeId()}))
		if err != nil {
			t.Fatal(err)
		}
		if c := applied(t, g.c, plan.Msg.GetChange()); c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("a person applying the push's plan: %v %s", c.GetState(), c.GetError())
		}
	})
}

// One deploy at a time for each app: a push while a deploy from a push is under way
// waits, a newer push takes the place of one waiting, and a deploy whose build
// still waits for its server is cancelled for the newer push.
func TestPushesToOneAppDeployOneAtATime(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile})
		g.done(t, g.propose(t, g.buildEdit("main", nil)))
		web := g.service(t, "", "web")
		url, secret := g.hook(t, web.GetId())
		push := func(id, commit string) {
			t.Helper()
			if code, text, _ := send(t, url, delivery{id: id, event: "push", secret: secret, body: pushBody(g.repo.URL, "refs/heads/main", commit)}); code != http.StatusAccepted {
				t.Fatalf("push %s: %d %q", id, code, text)
			}
		}
		building := func(changeID string, state dockwellv1.BuildState) *dockwellv1.Build {
			t.Helper()
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				list, err := g.builds.ListBuilds(t.Context(), connect.NewRequest(&dockwellv1.ListBuildsRequest{Id: web.GetId()}))
				if err != nil {
					t.Fatal(err)
				}
				for _, b := range list.Msg.GetBuilds() {
					if b.GetChangeId() == changeID && b.GetState() == state {
						return b
					}
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatalf("the build of %s never got to %v", changeID, state)
			return nil
		}

		// A slow build under way: the next push waits, and the one after takes its
		// place.
		slow := g.repo.Commit("main", "Slow", map[string]string{"Dockerfile": "FROM alpine:3.23\nRUN echo working && sleep 30\n"})
		push("q-1", slow)
		d1 := g.outcome(t, web.GetId(), "q-1", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_DEPLOYING)
		b1 := building(d1.GetChangeId(), dockwellv1.BuildState_BUILD_STATE_BUILDING)
		push("q-2", g.repo.Commit("main", "Next", map[string]string{"Dockerfile": webDockerfile, "app.js": "next"}))
		g.outcome(t, web.GetId(), "q-2", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_QUEUED)
		last := g.repo.Commit("main", "Last", map[string]string{"Dockerfile": webDockerfile, "app.js": "last"})
		push("q-3", last)
		g.outcome(t, web.GetId(), "q-3", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_QUEUED)
		g.outcome(t, web.GetId(), "q-2", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_SUPERSEDED)

		// A build under way isn't cancelled for a newer push; a person may.
		if _, err := g.builds.CancelBuild(t.Context(), connect.NewRequest(&dockwellv1.CancelBuildRequest{Id: b1.GetId()})); err != nil {
			t.Fatal(err)
		}
		d3 := g.outcome(t, web.GetId(), "q-3", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_DEPLOYING)
		if d3.GetCommit() != last {
			t.Errorf("the push that waited builds %s, not what main names, %s", d3.GetCommit(), last)
		}
		if c := g.ended(t, d3.GetChangeId()); c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("the push that waited ended %v: %s", c.GetState(), c.GetError())
		}

		// Another app's slow build holds the server's builder, so web's next build
		// waits for it, and a newer push cancels that.
		g.repo.Commit("slow", "Slow", map[string]string{"Dockerfile": "FROM alpine:3.23\nRUN echo working && sleep 30\n"})
		api := g.buildEdit("slow", nil)
		api.Resource.Name = "api"
		api.GetSpec().GetService().GetApp().Ports = nil
		other := g.propose(t, api)
		ow := watch(t, g.c, other.GetId())
		if _, err := g.c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: other.GetId(), Fingerprint: other.GetPlan().GetFingerprint()})); err != nil {
			t.Fatal(err)
		}
		var held *dockwellv1.Build
		deadline := time.Now().Add(15 * time.Second)
		for held == nil && time.Now().Before(deadline) {
			list, err := g.builds.ListBuilds(t.Context(), connect.NewRequest(&dockwellv1.ListBuildsRequest{Id: "api"}))
			if err != nil {
				t.Fatal(err)
			}
			if bs := list.Msg.GetBuilds(); len(bs) > 0 && bs[0].GetState() == dockwellv1.BuildState_BUILD_STATE_BUILDING {
				held = bs[0]
			}
			time.Sleep(50 * time.Millisecond)
		}
		if held == nil {
			t.Fatal("the other app's build never got under way")
		}
		push("q-4", g.repo.Commit("main", "Waits", map[string]string{"Dockerfile": webDockerfile, "app.js": "waits"}))
		d4 := g.outcome(t, web.GetId(), "q-4", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_DEPLOYING)
		building(d4.GetChangeId(), dockwellv1.BuildState_BUILD_STATE_QUEUED)
		newest := g.repo.Commit("main", "Newest", map[string]string{"Dockerfile": webDockerfile, "app.js": "newest"})
		push("q-5", newest)
		g.outcome(t, web.GetId(), "q-4", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_SUPERSEDED)
		cancelled := building(d4.GetChangeId(), dockwellv1.BuildState_BUILD_STATE_CANCELLED)
		if cancelled.GetCancelledBy() != "a newer push" {
			t.Errorf("the waiting build was cancelled by %q", cancelled.GetCancelledBy())
		}
		if _, err := g.builds.CancelBuild(t.Context(), connect.NewRequest(&dockwellv1.CancelBuildRequest{Id: held.GetId()})); err != nil {
			t.Fatal(err)
		}
		waitWatch(t, ow)
		d5 := g.outcome(t, web.GetId(), "q-5", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_DEPLOYING)
		if c := g.ended(t, d5.GetChangeId()); c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED || d5.GetCommit() != newest {
			t.Fatalf("the newest push ended %v building %s: %s", c.GetState(), d5.GetCommit(), c.GetError())
		}
	})
}

// stored waits for what a delivery came to, read from the store rather than the
// API, for a maker who may not read the hook any more.
func (g *gitBench) stored(t *testing.T, hookURL, deliveryID, want string) store.GitHookDelivery {
	t.Helper()
	hookID := hookURL[strings.LastIndex(hookURL, "/")+1:]
	var last store.GitHookDelivery
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := g.f.store.View(t.Context(), func(tx *store.Tx) error {
			list, err := tx.GitHookDeliveries(t.Context(), hookID, 50)
			for i := range list {
				if list[i].DeliveryID == deliveryID {
					last = list[i]
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if last.Outcome == want {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("delivery %s came to %+v, not %s", deliveryID, last, want)
	return last
}

// A push deploys with its hook's maker's rights as they are when it comes, as
// an API call would (ADR 0049): a developer's hook deploys where the environment
// is open, cancels no build where a developer may not, and leaves a plan for a
// person where the environment is protected. A maker who can't deploy the app
// any more, or who has left, deploys nothing.
func TestAHookDeploysOnlyWhatItsMakerMayNow(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile})
		made := g.done(t, g.propose(t, g.buildEdit("main", nil)))
		production := made.GetPlan().GetResources()[0].GetResource().GetScope()
		web := g.service(t, "", "web")
		url, secret := g.hook(t, web.GetId())
		n := 0
		push := func(id string) {
			t.Helper()
			n++
			commit := g.repo.Commit("main", fmt.Sprintf("Push %d", n), map[string]string{"Dockerfile": webDockerfile, "app.js": strconv.Itoa(n)})
			if code, text, _ := send(t, url, delivery{id: id, event: "push", secret: secret, body: pushBody(g.repo.URL, "refs/heads/main", commit)}); code != http.StatusAccepted {
				t.Fatalf("push %s: %d %q", id, code, text)
			}
		}
		buildOf := func(changeID string) *dockwellv1.Build {
			t.Helper()
			list, err := g.builds.ListBuilds(t.Context(), connect.NewRequest(&dockwellv1.ListBuildsRequest{Id: web.GetId()}))
			if err != nil {
				t.Fatal(err)
			}
			for _, b := range list.Msg.GetBuilds() {
				if b.GetChangeId() == changeID {
					return b
				}
			}
			return nil
		}

		// A developer where the environment is open deploys.
		protectEnv(t, g.c, g.f, production, resourcev1.Protection_PROTECTION_OPEN)
		hold := aMember(t, g.f)
		hold(onInstall(store.RoleDeveloper))
		push("o-1")
		d := g.stored(t, url, "o-1", store.HookDeploying)
		if c := g.ended(t, d.ChangeID); c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("a developer's push where it's open ended %v: %s", c.GetState(), c.GetError())
		}

		// Where it's protected, a build waiting for its server is an admin's to
		// cancel: a newer push from a developer's hook leaves it, and waits.
		hold(onInstall(store.RoleAdmin))
		protectEnv(t, g.c, g.f, production, resourcev1.Protection_PROTECTION_PROTECTED)
		g.repo.Commit("slow", "Slow", map[string]string{"Dockerfile": "FROM alpine:3.23\nRUN echo working && sleep 30\n"})
		api := g.buildEdit("slow", nil)
		api.Resource.Name = "api"
		api.GetSpec().GetService().GetApp().Ports = nil
		other := g.propose(t, api)
		ow := watch(t, g.c, other.GetId())
		if _, err := g.c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: other.GetId(), Fingerprint: other.GetPlan().GetFingerprint()})); err != nil {
			t.Fatal(err)
		}
		var held *dockwellv1.Build
		for deadline := time.Now().Add(15 * time.Second); held == nil && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			list, err := g.builds.ListBuilds(t.Context(), connect.NewRequest(&dockwellv1.ListBuildsRequest{Id: "api"}))
			if err != nil {
				t.Fatal(err)
			}
			if bs := list.Msg.GetBuilds(); len(bs) > 0 && bs[0].GetState() == dockwellv1.BuildState_BUILD_STATE_BUILDING {
				held = bs[0]
			}
		}
		if held == nil {
			t.Fatal("the other app's build never got under way")
		}
		push("c-1")
		c1 := g.stored(t, url, "c-1", store.HookDeploying)
		hold(onInstall(store.RoleDeveloper))
		push("c-2")
		g.stored(t, url, "c-2", store.HookQueued)
		for until := time.Now().Add(2 * time.Second); time.Now().Before(until); time.Sleep(100 * time.Millisecond) {
			if b := buildOf(c1.ChangeID); b.GetState() != dockwellv1.BuildState_BUILD_STATE_QUEUED {
				t.Fatalf("a developer's newer push left the waiting build %v, by %q", b.GetState(), b.GetCancelledBy())
			}
		}
		hold(onInstall(store.RoleAdmin))
		if _, err := g.builds.CancelBuild(t.Context(), connect.NewRequest(&dockwellv1.CancelBuildRequest{Id: held.GetId()})); err != nil {
			t.Fatal(err)
		}
		waitWatch(t, ow)
		if c := g.ended(t, c1.ChangeID); c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("the push whose build waited ended %v: %s", c.GetState(), c.GetError())
		}
		c2 := g.stored(t, url, "c-2", store.HookDeploying)
		g.ended(t, c2.ChangeID)

		// A developer's push to a protected environment plans, and waits for a
		// person who may apply it. Giving its hook a new secret there is an
		// admin's, whether the app is named by its ID or its name.
		hold(onInstall(store.RoleDeveloper))
		for _, id := range []string{web.GetId(), "web"} {
			_, err := g.hooks().RotateGitHookSecret(t.Context(), connect.NewRequest(&dockwellv1.RotateGitHookSecretRequest{Id: id}))
			needsRole(t, "a developer giving a protected app's hook a new secret, by "+id, err)
		}
		push("p-1")
		p1 := g.stored(t, url, "p-1", store.HookNeedsPerson)
		if !strings.Contains(p1.Detail, "may apply it there") {
			t.Errorf("why a developer's push waits: %q", p1.Detail)
		}
		got, err := g.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: p1.ChangeID}))
		if err != nil || got.Msg.GetChange().GetState() != dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
			t.Fatalf("a developer's push's plan is %v, %v", got.Msg.GetChange().GetState(), err)
		}

		// A viewer's hook deploys nothing, and plans nothing, even where the
		// environment is open.
		hold(onInstall(store.RoleAdmin))
		protectEnv(t, g.c, g.f, production, resourcev1.Protection_PROTECTION_OPEN)
		hold(onInstall(store.RoleViewer))
		push("v-1")
		if v := g.stored(t, url, "v-1", store.HookFailed); !strings.Contains(v.Detail, "may not deploy it now") || v.ChangeID != "" {
			t.Errorf("a viewer's push came to %+v", v)
		}
		// Nor does the hook of someone with no role left.
		hold()
		push("x-1")
		if x := g.stored(t, url, "x-1", store.HookFailed); !strings.Contains(x.Detail, "may not deploy it now") || x.ChangeID != "" {
			t.Errorf("a push from someone with no role left came to %+v", x)
		}
	})
}

// raw sends body to url with exactly these headers, and gives the answer's status
// and text.
func raw(t *testing.T, url string, headers map[string]string, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	text, _ := io.ReadAll(res.Body)
	return res.StatusCode, strings.TrimSpace(string(text))
}

// GitLab sends the hook's secret itself, which is compared in constant time, and
// Bitbucket Cloud signs as GitHub does, in another header. Bitbucket's push may
// move several branches: it deploys the app if one is the branch it builds, which
// its plan named in full though the app was made to build the default branch.
func TestGitLabAndBitbucketPushesDeploy(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		g := newGitBench(t, backend)
		g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile})
		g.done(t, g.propose(t, g.buildEdit("", nil)))
		web := g.service(t, "", "web")
		url, secret := g.hook(t, web.GetId())
		_, wrong, _ := send(t, url, delivery{id: "w-1", event: "push", secret: "not-the-secret", body: pushBody(g.repo.URL, "refs/heads/main", strings.Repeat("ab", 20))})
		page := strings.TrimSuffix(g.repo.URL, ".git")

		gitlab := func(id, event, token string, body map[string]any) (int, string) {
			b, _ := json.Marshal(body)
			return raw(t, url, map[string]string{
				"Content-Type": "application/json", "User-Agent": "GitLab/18.5.0",
				"X-Gitlab-Event": event, "X-Gitlab-Event-UUID": id, "X-Gitlab-Token": token,
			}, b)
		}
		gitlabPush := func(ref, after string) map[string]any {
			return map[string]any{
				"object_kind": "push", "ref": ref, "before": strings.Repeat("0", 40), "after": after,
				"project": map[string]any{"git_http_url": g.repo.URL, "web_url": page, "default_branch": "main"},
			}
		}
		second := g.repo.Commit("main", "Second", map[string]string{"Dockerfile": webDockerfile, "app.js": "2"})
		if code, text := gitlab("6f1e0c2a-2b47-4c55-9a3e-0d3f6b1a2c11", "Push Hook", "not-the-secret", gitlabPush("refs/heads/main", second)); code != http.StatusUnauthorized || text != wrong {
			t.Errorf("GitLab with a wrong token: %d %q, want the answer a wrong signature gets, %q", code, text, wrong)
		}
		if code, text := gitlab("6f1e0c2a-2b47-4c55-9a3e-0d3f6b1a2c12", "Push Hook", secret+"0", gitlabPush("refs/heads/main", second)); code != http.StatusUnauthorized {
			t.Errorf("GitLab with the secret and more: %d %q", code, text)
		}
		if code, text := gitlab("6f1e0c2a-2b47-4c55-9a3e-0d3f6b1a2c13", "Push Hook", secret, gitlabPush("refs/heads/main", second)); code != http.StatusAccepted {
			t.Fatalf("GitLab's push: %d %q", code, text)
		}
		d := g.outcome(t, web.GetId(), "6f1e0c2a-2b47-4c55-9a3e-0d3f6b1a2c13", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_DEPLOYING)
		if d.GetProvider() != "gitlab" || d.GetCommit() != second || d.GetRef() != "refs/heads/main" {
			t.Errorf("GitLab's push came to %v", d)
		}
		if c := g.ended(t, d.GetChangeId()); c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("GitLab's push ended %v: %s", c.GetState(), c.GetError())
		}
		tag := gitlabPush("refs/tags/v1", second)
		tag["object_kind"] = "tag_push"
		gitlab("6f1e0c2a-2b47-4c55-9a3e-0d3f6b1a2c14", "Tag Push Hook", secret, tag)
		g.outcome(t, web.GetId(), "6f1e0c2a-2b47-4c55-9a3e-0d3f6b1a2c14", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_IGNORED)
		gitlab("6f1e0c2a-2b47-4c55-9a3e-0d3f6b1a2c15", "Merge Request Hook", secret, map[string]any{"object_kind": "merge_request"})
		if d := g.outcome(t, web.GetId(), "6f1e0c2a-2b47-4c55-9a3e-0d3f6b1a2c15", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_IGNORED); !strings.Contains(d.GetDetail(), "not a push") {
			t.Errorf("GitLab's merge request: %v", d)
		}

		bitbucket := func(id, signedWith string, branches ...string) (int, string) {
			var changes []map[string]any
			for _, b := range branches {
				changes = append(changes, map[string]any{"new": map[string]any{"type": "branch", "name": b}, "old": map[string]any{"type": "branch", "name": b}})
			}
			body, _ := json.Marshal(map[string]any{
				"push":       map[string]any{"changes": changes},
				"repository": map[string]any{"full_name": "acme/web", "links": map[string]any{"html": map[string]any{"href": page}}},
			})
			mac := hmac.New(sha256.New, []byte(signedWith))
			mac.Write(body)
			return raw(t, url, map[string]string{
				"Content-Type": "application/json", "User-Agent": "Bitbucket-Webhooks/2.0",
				"X-Event-Key": "repo:push", "X-Request-UUID": "{" + id + "}",
				"X-Hub-Signature": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
			}, body)
		}
		if code, text := bitbucket("b0c1d2e3-0000-4000-8000-000000000001", "not-the-secret", "main"); code != http.StatusUnauthorized || text != wrong {
			t.Errorf("Bitbucket with a wrong signature: %d %q", code, text)
		}
		// A push to dev alone isn't to the branch web builds.
		g.repo.Commit("dev", "Elsewhere", map[string]string{"Dockerfile": webDockerfile, "app.js": "dev"})
		if code, text := bitbucket("b0c1d2e3-0000-4000-8000-000000000002", secret, "dev"); code != http.StatusAccepted {
			t.Fatalf("Bitbucket's push to dev: %d %q", code, text)
		}
		if d := g.outcome(t, web.GetId(), "b0c1d2e3-0000-4000-8000-000000000002", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_IGNORED); d.GetProvider() != "bitbucket" || !strings.Contains(d.GetDetail(), "web builds main") || d.GetChangeId() != "" {
			t.Errorf("Bitbucket's push to dev came to %v", d)
		}
		// One that moves dev and main deploys main.
		third := g.repo.Commit("main", "Third", map[string]string{"Dockerfile": webDockerfile, "app.js": "3"})
		bitbucket("b0c1d2e3-0000-4000-8000-000000000003", secret, "dev", "main")
		d = g.outcome(t, web.GetId(), "b0c1d2e3-0000-4000-8000-000000000003", dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_DEPLOYING)
		if d.GetCommit() != third || d.GetRef() != "refs/heads/main" {
			t.Errorf("Bitbucket's push to dev and main came to %v", d)
		}
		if c := g.ended(t, d.GetChangeId()); c.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("Bitbucket's push ended %v: %s", c.GetState(), c.GetError())
		}
	})
}

// slowDelivery starts a delivery to url whose body comes a byte at a time, every
// 100 ms, as one meant to hold the receiver would, and gives what it's answered,
// once it is, or "" if the connection ends with no answer.
func slowDelivery(t *testing.T, url string) <-chan string {
	t.Helper()
	u, err := neturl.Parse(url)
	if err != nil {
		t.Fatal(err)
	}
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n"+
		"X-GitHub-Event: push\r\nX-GitHub-Delivery: slow-%d\r\nX-Hub-Signature-256: sha256=00\r\n\r\n", u.Path, u.Host, time.Now().UnixNano())
	go func() {
		for range 1000 {
			if _, err := conn.Write([]byte(" ")); err != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	answered := make(chan string, 1)
	go func() {
		res, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			answered <- ""
			return
		}
		_ = res.Body.Close()
		answered <- res.Status + " " + res.Header.Get("Retry-After")
	}()
	return answered
}

// A delivery's body has a deadline, so one that trickles in holds nothing for
// long, and only so many are read at once: past that, a delivery is told to come
// again, and nothing of it is read.
func TestADeliveryThatTricklesInHoldsNothingForLong(t *testing.T) {
	t.Parallel()
	g := newGitBench(t, store.SQLite)
	head := g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile})
	g.done(t, g.propose(t, g.buildEdit("main", nil)))
	web := g.service(t, "", "web")
	url, secret := g.hook(t, web.GetId())
	hooks := g.f.server.hooks

	// Two read at once at most: two that trickle hold both, and a third waits. It's
	// sent to a hook there isn't, so it spends nothing of web's.
	hooks.SetLimits(5*time.Second, 2)
	first, second := slowDelivery(t, url), slowDelivery(t, url)
	none := url[:strings.LastIndex(url, "/")+1] + "ghk_01JD6Z3R8M9QF2K7T4XW5BNC0V"
	var code int
	var header http.Header
	for range 20 {
		if code, _, header = send(t, none, delivery{id: "busy", event: "push", secret: secret, body: pushBody(g.repo.URL, "refs/heads/main", head)}); code == http.StatusServiceUnavailable {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code != http.StatusServiceUnavailable || header.Get("Retry-After") == "" {
		t.Errorf("a delivery while two trickle in: %d, Retry-After %q; want 503 and when to come again", code, header.Get("Retry-After"))
	}

	// One that trickles past its deadline is cut off, and frees its place.
	hooks.SetLimits(300*time.Millisecond, 2)
	for what, answered := range map[string]<-chan string{"the first": first, "the second": second, "a third": slowDelivery(t, url)} {
		select {
		case got := <-answered:
			if strings.HasPrefix(got, "2") {
				t.Errorf("%s delivery that trickled in was answered %q", what, got)
			}
		case <-time.After(15 * time.Second):
			t.Errorf("%s delivery that trickled in held its place past its deadline", what)
		}
	}
	if code, text, _ := send(t, url, delivery{id: "after", event: "push", secret: secret, body: pushBody(g.repo.URL, "refs/heads/main", head)}); code != http.StatusAccepted {
		t.Errorf("a delivery once those ended: %d %q", code, text)
	}
}

// devHookInProduction is web, with a hook that acts for a developer, usr_dev,
// in a protected environment, and the bench signed in as an admin.
func devHookInProduction(t *testing.T) (g *gitBench, url, secret string, push func(id, n string) store.GitHookDelivery) {
	t.Helper()
	g = newGitBench(t, store.SQLite)
	g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile})
	made := g.done(t, g.propose(t, g.buildEdit("main", nil)))
	production := made.GetPlan().GetResources()[0].GetResource().GetScope()
	web := g.service(t, "", "web")
	url, secret = g.hook(t, web.GetId())
	hold := aMember(t, g.f)
	hold(onInstall(store.RoleAdmin))
	err := g.f.store.Update(t.Context(), func(tx *store.Tx) error {
		if err := tx.CreateUser(t.Context(), store.User{ID: "usr_dev", Email: "dev@example.com", Name: "Dev", WebAuthnHandle: []byte("dev"), CreatedAt: time.Now()}); err != nil {
			return err
		}
		if err := tx.SetGrant(t.Context(), store.Grant{UserID: "usr_dev", ProjectID: store.Install, Role: store.RoleDeveloper, CreatedAt: time.Now()}); err != nil {
			return err
		}
		h, err := tx.GitHookOf(t.Context(), web.GetId())
		if err != nil {
			return err
		}
		if err := tx.DeleteGitHook(t.Context(), h.ID); err != nil {
			return err
		}
		h.UserID = "usr_dev"
		return tx.CreateGitHook(t.Context(), h)
	})
	if err != nil {
		t.Fatal(err)
	}
	protectEnv(t, g.c, g.f, production, resourcev1.Protection_PROTECTION_PROTECTED)
	push = func(id, n string) store.GitHookDelivery {
		t.Helper()
		commit := g.repo.Commit("main", "Push "+n, map[string]string{"Dockerfile": webDockerfile, "app.js": n})
		if code, text, _ := send(t, url, delivery{id: id, event: "push", secret: secret, body: pushBody(g.repo.URL, "refs/heads/main", commit)}); code != http.StatusAccepted {
			t.Fatalf("push: %d %q", code, text)
		}
		return g.stored(t, url, id, store.HookNeedsPerson)
	}
	return g, url, secret, push
}

// A developer's push to a protected environment leaves a plan that is someone
// else's to an admin: applying it approves it, with the fingerprint and a passkey
// (from #253's review, H1).
func TestADevelopersPushPlanIsTheirsToAnAdmin(t *testing.T) {
	t.Parallel()
	g, _, _, push := devHookInProduction(t)
	d := push("d-1", "2")
	got, err := g.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: d.ChangeID}))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Msg.GetChange().GetApproves() {
		t.Errorf("an admin reading a developer's push plan in production: approves false, want true")
	}
	_, err = g.c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: d.ChangeID}))
	if reasonOf(err) != "needs_confirmation" {
		t.Errorf("an admin applying a developer's push plan in production with no fingerprint or passkey: %v, want needs_confirmation", err)
	}
	// Nor does an admin's token approve it, as a token approves no one else's plan.
	token := bearer{token: createToken(t, g.b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: g.b.client.Transport}
	_, err = changeClient(token, g.b.origin).ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{
		Id: d.ChangeID, Fingerprint: got.Msg.GetChange().GetPlan().GetFingerprint(),
	}))
	if reasonOf(err) != authapi.ReasonNeedsSession {
		t.Errorf("an admin's token applying a developer's push plan in production: %v, want %s", err, authapi.ReasonNeedsSession)
	}
}

// A newer push's plan takes the place of the one an earlier push left, where the
// environment is protected too, as the hook discards its own maker's plan (from
// #253's review, H1).
func TestANewerPushDiscardsThePlanTheLastOneLeft(t *testing.T) {
	t.Parallel()
	g, _, _, push := devHookInProduction(t)
	d1 := push("d-1", "2")
	d2 := push("d-2", "3")
	if d1.ChangeID == d2.ChangeID {
		t.Fatal("one change for two pushes")
	}
	var state dockwellv1.ChangeState
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		got, err := g.c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: d1.ChangeID}))
		if err != nil {
			t.Fatal(err)
		}
		if state = got.Msg.GetChange().GetState(); state == dockwellv1.ChangeState_CHANGE_STATE_DISCARDED {
			return
		}
	}
	t.Errorf("the first push's plan is %v after a newer push planned, want discarded", state)
}

// A push that waits for another change to its app reads its maker's role again
// at each try, as an API call would: a maker lowered while it waited applies
// nothing, and the plan waits for a person (from #253's review, M4).
func TestAPushThatWaitsReadsItsMakersRoleAgain(t *testing.T) {
	t.Parallel()
	g := newGitBench(t, store.SQLite)
	g.repo.Commit("main", "First", map[string]string{"Dockerfile": webDockerfile})
	made := g.done(t, g.propose(t, g.buildEdit("main", nil)))
	protectEnv(t, g.c, g.f, made.GetPlan().GetResources()[0].GetResource().GetScope(), resourcev1.Protection_PROTECTION_OPEN)
	web := g.service(t, "", "web")
	url, secret := g.hook(t, web.GetId())
	// The hook is a developer's, and the bench's owner deploys by hand.
	if err := g.f.store.Update(t.Context(), func(tx *store.Tx) error {
		if err := tx.CreateUser(t.Context(), store.User{ID: "usr_dev", Email: "dev@example.com", Name: "Dev", WebAuthnHandle: []byte("dev"), CreatedAt: time.Now()}); err != nil {
			return err
		}
		if err := tx.SetGrant(t.Context(), store.Grant{UserID: "usr_dev", ProjectID: store.Install, Role: store.RoleDeveloper, CreatedAt: time.Now()}); err != nil {
			return err
		}
		h, err := tx.GitHookOf(t.Context(), web.GetId())
		if err != nil {
			return err
		}
		if err := tx.DeleteGitHook(t.Context(), h.ID); err != nil {
			return err
		}
		h.UserID = "usr_dev"
		return tx.CreateGitHook(t.Context(), h)
	}); err != nil {
		t.Fatal(err)
	}

	// A slow deploy by hand holds web.
	g.repo.Commit("main", "Slow", map[string]string{"Dockerfile": "FROM alpine:3.23\nRUN echo working && sleep 30\n"})
	slow := g.propose(t, g.buildEdit("main", nil))
	if _, err := g.c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: slow.GetId(), Fingerprint: slow.GetPlan().GetFingerprint()})); err != nil {
		t.Fatal(err)
	}
	var building *dockwellv1.Build
	for deadline := time.Now().Add(15 * time.Second); building == nil && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		list, err := g.builds.ListBuilds(t.Context(), connect.NewRequest(&dockwellv1.ListBuildsRequest{Id: web.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range list.Msg.GetBuilds() {
			if b.GetChangeId() == slow.GetId() && b.GetState() == dockwellv1.BuildState_BUILD_STATE_BUILDING {
				building = b
			}
		}
	}
	if building == nil {
		t.Fatal("the deploy by hand never got to building")
	}

	// The developer pushes; their deploy plans, and waits for web.
	pushed := g.repo.Commit("main", "Pushed", map[string]string{"Dockerfile": webDockerfile, "app.js": "pushed"})
	if code, text, _ := send(t, url, delivery{id: "w-1", event: "push", secret: secret, body: pushBody(g.repo.URL, "refs/heads/main", pushed)}); code != http.StatusAccepted {
		t.Fatalf("the push: %d %q", code, text)
	}
	var planned string
	for deadline := time.Now().Add(15 * time.Second); planned == "" && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		list, err := g.c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range list.Msg.GetChanges() {
			if c.GetProposedBy().GetKind() == dockwellv1.ActorKind_ACTOR_KIND_HOOK && c.GetState() == dockwellv1.ChangeState_CHANGE_STATE_PLANNED {
				planned = c.GetId()
			}
		}
	}
	if planned == "" {
		t.Fatal("the push never planned its deploy")
	}

	// While it waits, the developer becomes a viewer, and then web is free.
	if err := g.f.store.Update(t.Context(), func(tx *store.Tx) error {
		return tx.SetGrant(t.Context(), store.Grant{UserID: "usr_dev", ProjectID: store.Install, Role: store.RoleViewer, CreatedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.builds.CancelBuild(t.Context(), connect.NewRequest(&dockwellv1.CancelBuildRequest{Id: building.GetId()})); err != nil {
		t.Fatal(err)
	}
	g.ended(t, slow.GetId())
	d := g.stored(t, url, "w-1", store.HookNeedsPerson)
	if !strings.Contains(d.Detail, "may apply it there") {
		t.Errorf("why the push waits: %q", d.Detail)
	}
	if got := g.service(t, "", "web").GetSpec().GetBuild().GetGit().GetCommit(); got == pushed {
		t.Errorf("a viewer's push deployed %s", got)
	}
}
