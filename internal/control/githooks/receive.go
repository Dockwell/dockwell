// SPDX-License-Identifier: AGPL-3.0-only

package githooks

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/store"
)

// MaxBody is the most a delivery may hold. A push's payload lists its commits, 20
// at most on GitHub, and is far less.
const MaxBody = 1 << 20

// provider is what a sender's headers say it is, with where it puts each thing.
// Gitea sends GitHub's headers too, and Forgejo Gitea's, so the order they are
// tried in matters.
type provider struct {
	name string
	// event and delivery are the headers that say what a delivery is, and which
	// one, which a retry sends again.
	event, delivery string
	// signature is the header with the body's HMAC-SHA-256 in hex, after prefix.
	// token is the header with the secret itself, as GitLab sends it instead.
	signature, prefix, token string
	// pushes and pings are the events that are a push, and that test the hook.
	pushes, pings []string
	// form says it may send its payload as a form's payload field, signing the
	// form, as GitHub does unless its hook says JSON.
	form bool
	// read reads a push's payload.
	read func([]byte) (pushed, error)
}

var providers = []provider{
	{name: "forgejo", event: "X-Forgejo-Event", delivery: "X-Forgejo-Delivery", signature: "X-Forgejo-Signature", pushes: []string{"push"}, read: readGitHub},
	{name: "gitea", event: "X-Gitea-Event", delivery: "X-Gitea-Delivery", signature: "X-Gitea-Signature", pushes: []string{"push"}, read: readGitHub},
	{name: "github", event: "X-GitHub-Event", delivery: "X-GitHub-Delivery", signature: "X-Hub-Signature-256", prefix: "sha256=", pushes: []string{"push"}, pings: []string{"ping"}, form: true, read: readGitHub},
	{name: "gitlab", event: "X-Gitlab-Event", delivery: "X-Gitlab-Event-UUID", token: "X-Gitlab-Token", pushes: []string{"Push Hook", "Tag Push Hook"}, read: readGitLab}, //nolint:gosec // a header's name, not a credential
	// Bitbucket Cloud. GitHub sends X-Hub-Signature too, of SHA-1, but is found
	// by its own header first.
	{name: "bitbucket", event: "X-Event-Key", delivery: "X-Request-UUID", signature: "X-Hub-Signature", prefix: "sha256=", pushes: []string{"repo:push"}, read: readBitbucket},
}

// pushed is what Dockwell reads of a push's payload, whichever provider sent it.
// Nothing in it is trusted for what to deploy: a push says only that the
// repository moved, and Dockwell reads the branch itself.
type pushed struct {
	// refs are what it moved, and gone what it deleted, by their full names.
	refs, gone []string
	// repos are the repository's URLs.
	repos []string
	// defaultBranch is the repository's default branch, when the payload says.
	defaultBranch string
}

// readGitHub reads a push as GitHub, Gitea and Forgejo send it.
func readGitHub(body []byte) (pushed, error) {
	var in struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Deleted    bool   `json:"deleted"`
		Repository struct {
			CloneURL      string `json:"clone_url"`
			HTMLURL       string `json:"html_url"`
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return pushed{}, err
	}
	out := pushed{repos: []string{in.Repository.CloneURL, in.Repository.HTMLURL}, defaultBranch: in.Repository.DefaultBranch}
	out.add(in.Ref, in.Deleted || strings.Trim(in.After, "0") == "")
	return out, nil
}

// readGitLab reads a push, or a tag's, as GitLab sends it.
func readGitLab(body []byte) (pushed, error) {
	var in struct {
		Ref     string `json:"ref"`
		After   string `json:"after"`
		Project struct {
			GitHTTPURL    string `json:"git_http_url"`
			WebURL        string `json:"web_url"`
			DefaultBranch string `json:"default_branch"`
		} `json:"project"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return pushed{}, err
	}
	out := pushed{repos: []string{in.Project.GitHTTPURL, in.Project.WebURL}, defaultBranch: in.Project.DefaultBranch}
	out.add(in.Ref, strings.Trim(in.After, "0") == "")
	return out, nil
}

// readBitbucket reads a push as Bitbucket Cloud sends it: each branch or tag it
// moved, which may be several, and no default branch.
func readBitbucket(body []byte) (pushed, error) {
	type end struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	var in struct {
		Push struct {
			Changes []struct {
				New *end `json:"new"`
				Old *end `json:"old"`
			} `json:"changes"`
		} `json:"push"`
		Repository struct {
			Links struct {
				HTML struct {
					Href string `json:"href"`
				} `json:"html"`
			} `json:"links"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return pushed{}, err
	}
	out := pushed{repos: []string{in.Repository.Links.HTML.Href}}
	full := func(e *end) string {
		switch {
		case e == nil || e.Name == "":
			return ""
		case e.Type == "branch":
			return "refs/heads/" + e.Name
		case e.Type == "tag" || e.Type == "annotated_tag":
			return "refs/tags/" + e.Name
		}
		return ""
	}
	for _, c := range in.Push.Changes {
		if ref := full(c.New); ref != "" {
			out.add(ref, false)
		} else if ref := full(c.Old); ref != "" && c.New == nil {
			out.add(ref, true)
		}
	}
	return out, nil
}

func (p *pushed) add(ref string, deleted bool) {
	switch {
	case ref == "":
	case deleted:
		p.gone = append(p.gone, ref)
	default:
		p.refs = append(p.refs, ref)
	}
}

// ServeHTTP receives a delivery at Path plus a hook's ID. It faces the internet as
// sign-in does: it spends from the sender's address's bucket before anything else,
// refuses a body by its length before reading it and cuts it off past MaxBody,
// and checks the signature before anything reads the body, in constant time, with
// one answer for a wrong signature and a hook there isn't. A delivery it takes it
// answers at once, 202, and deploys after.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// The body has a deadline from the start, so one that trickles in holds
	// nothing for long, whatever is answered before it's read.
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(time.Duration(h.readFor.Load()))); err != nil {
		h.cfg.Logger.WarnContext(r.Context(), "can't give a delivery's body a deadline", slog.String("error", err.Error()))
	}
	client, _ := authapi.ClientFrom(r.Context())
	if !h.cfg.Auth.TakeHookDelivery(client) {
		w.Header().Set("Retry-After", "2")
		answer(w, http.StatusTooManyRequests, "Too many deliveries from this address. Wait a moment, then send again.")
		return
	}
	if r.ContentLength > MaxBody {
		answer(w, http.StatusRequestEntityTooLarge, "The delivery is larger than 1 MiB, which no push is.")
		return
	}
	var from *provider
	for i := range providers {
		if r.Header.Get(providers[i].signature) != "" || r.Header.Get(providers[i].token) != "" {
			from = &providers[i]
			break
		}
	}
	if from == nil {
		answer(w, http.StatusUnauthorized, "The delivery isn't signed. Give the hook its secret, as Dockwell showed it.")
		return
	}
	form := false
	switch t, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); {
	case err == nil && t == "application/json":
	case err == nil && t == "application/x-www-form-urlencoded" && from.form:
		form = true
	default:
		answer(w, http.StatusUnsupportedMediaType, "The delivery isn't JSON. Set the hook's content type to application/json.")
		return
	}
	// Only so many are read at once. One refused here closes its connection, as
	// its body is never read.
	if h.reading.Add(1) > h.inFlight.Load() {
		h.reading.Add(-1)
		w.Header().Set("Retry-After", "2")
		w.Header().Set("Connection", "close")
		answer(w, http.StatusServiceUnavailable, "Dockwell has many deliveries under way. Send it again in a moment.")
		return
	}
	defer h.reading.Add(-1)
	// Bitbucket's IDs may come in braces.
	delivery := strings.Trim(r.Header.Get(from.delivery), "{}")
	if !deliveryID(delivery) {
		answer(w, http.StatusBadRequest, "The delivery has no ID in "+from.delivery+", which Dockwell needs to tell it from one sent again.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			answer(w, http.StatusRequestEntityTooLarge, "The delivery is larger than 1 MiB, which no push is.")
			return
		}
		answer(w, http.StatusBadRequest, "The delivery ended before its body did. Send it again.")
		return
	}
	var proof auth.HookProof
	if from.token != "" {
		proof.Token = r.Header.Get(from.token)
	} else if sig, err := hex.DecodeString(strings.TrimPrefix(r.Header.Get(from.signature), from.prefix)); err == nil {
		proof.Signature = sig
	}
	hookID := strings.TrimPrefix(r.URL.Path, Path)
	p, err := h.cfg.Auth.GitHookPrincipal(r.Context(), hookID, body, proof)
	switch {
	case errors.Is(err, auth.ErrTooManyRequests):
		w.Header().Set("Retry-After", "1")
		answer(w, http.StatusTooManyRequests, "Too many deliveries to this hook. Wait a moment, then send again.")
		return
	case errors.Is(err, auth.ErrHookRejected):
		answer(w, http.StatusUnauthorized, "The delivery's signature or token doesn't match the hook's secret. Give the hook the secret Dockwell showed, or a new one from the panel.")
		return
	case err != nil:
		h.cfg.Logger.ErrorContext(r.Context(), "can't check a delivery to a hook", slog.String("error", err.Error()))
		answer(w, http.StatusServiceUnavailable, "Dockwell can't check deliveries right now. Send it again in a moment.")
		return
	}
	// A form is read once its signature matched, which is of the form.
	payload := body
	if form {
		payload = nil
		if v, err := url.ParseQuery(string(body)); err == nil {
			payload = []byte(v.Get("payload"))
		}
	}
	// A delivery that proved it knows the secret costs its address nothing.
	h.cfg.Auth.ReturnHookDelivery(client)
	hook := *p.Hook
	d := store.GitHookDelivery{HookID: hook.ID, DeliveryID: delivery, At: h.now(), Provider: from.name, Outcome: store.HookIgnored}
	event := r.Header.Get(from.event)
	ping := slices.Contains(from.pings, event)
	j, why := h.read(r.Context(), client, p, from, event, payload, &d)
	if j == nil {
		d.Detail = strings.ToUpper(why[:1]) + why[1:]
	} else {
		d.Outcome, d.Detail = store.HookQueued, "Reads "+refName(d.Ref)+" and deploys what it names"
	}
	err = h.cfg.DB.Update(r.Context(), func(tx *store.Tx) error { return tx.RecordGitHookDelivery(r.Context(), d) })
	switch {
	case errors.Is(err, store.ErrConflict):
		answer(w, http.StatusAccepted, "Dockwell has had this delivery already, so it does nothing.")
		return
	case err != nil:
		h.cfg.Logger.ErrorContext(r.Context(), "can't keep a delivery to a hook", slog.String("hook", hook.ID), slog.String("error", err.Error()))
		answer(w, http.StatusServiceUnavailable, "Dockwell can't keep deliveries right now. Send it again in a moment.")
		return
	}
	switch {
	case j == nil && ping:
		answer(w, http.StatusAccepted, "Taken. The hook works: a push to the branch the service builds deploys it.")
		return
	case j == nil:
		// What it is to the service stays in the panel.
		answer(w, http.StatusAccepted, "Taken. It isn't a push the service builds, so it does nothing.")
		return
	}
	j.delivery = d
	h.enqueue(j)
	answer(w, http.StatusAccepted, "Taken. Dockwell reads "+refName(d.Ref)+" and deploys what it names.")
}

// read reads a delivery whose proof matched, and gives the deploy it asks for, or
// why it asks for none. It fills in d's ref.
func (h *Handler) read(ctx context.Context, client auth.Client, p auth.Principal, from *provider, event string, body []byte, d *store.GitHookDelivery) (*job, string) {
	switch {
	case slices.Contains(from.pushes, event):
	case slices.Contains(from.pings, event):
		return nil, "it's a ping, which says the hook works"
	default:
		return nil, "it's a " + strings.ReplaceAll(event, "_", " ") + " event, not a push"
	}
	in, err := from.read(body)
	if err != nil || len(in.refs)+len(in.gone) == 0 {
		return nil, "its payload isn't a push Dockwell can read"
	}
	if len(in.refs) == 0 {
		d.Ref = in.gone[0]
		return nil, "it deletes " + refName(in.gone[0])
	}
	d.Ref = in.refs[0]
	ctx = authapi.As(ctx, client, p)
	b, name, _, err := h.cfg.Services.BuildOf(ctx, p.Hook.ResourceID, "", "")
	if err != nil {
		return nil, "the service it deploys is gone, or can't be read now"
	}
	g := b.GetGit()
	switch {
	case g == nil:
		return nil, name + " doesn't build from Git now"
	case isCommit(g.GetRef()):
		return nil, name + " builds commit " + short(g.GetRef()) + ", which no push moves"
	case !sameRepository(g.GetUrl(), in.repos...):
		return nil, "it's a push to " + shownRepository(in.repos...) + ", and " + name + " builds from " + shownRepository(g.GetUrl())
	}
	// A push may move several refs, as Bitbucket's may: it's the build's if it
	// moved the one the build follows, which a plan names in full.
	for _, ref := range in.refs {
		if sameRef(g.GetRef(), ref, in.defaultBranch) {
			d.Ref = ref
			return &job{hook: *p.Hook, p: p, client: client}, ""
		}
	}
	return nil, "it's a push to " + refName(d.Ref) + ", and " + name + " builds " + refName(builtRef(g.GetRef()))
}

// answer says what came of a delivery, in a line, as text.
func answer(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(msg + "\n"))
}

// deliveryID reports a provider's ID for a delivery, as a GUID is: short, and
// letters, digits and dashes.
func deliveryID(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

// sameRef reports a push to the ref a build follows: by its full name, a short
// name being a branch or a tag, and none being the repository's default branch.
func sameRef(built, pushed, defaultBranch string) bool {
	switch {
	case built == "":
		return defaultBranch != "" && pushed == "refs/heads/"+defaultBranch
	case strings.HasPrefix(built, "refs/"):
		return pushed == built
	}
	return pushed == "refs/heads/"+built || pushed == "refs/tags/"+built
}

// builtRef is the ref a build follows, as a person reads it.
func builtRef(ref string) string {
	if ref == "" {
		return "its default branch"
	}
	return ref
}

// refName is a ref as people say it: a branch or tag by its short name.
func refName(ref string) string {
	for _, p := range []string{"refs/heads/", "refs/tags/"} {
		if s, ok := strings.CutPrefix(ref, p); ok {
			return s
		}
	}
	return ref
}

// sameRepository reports whether a push's repository, by any of its URLs, is the
// one a build fetches: the same host and path, whatever their case, with no .git
// and no trailing slash, as GitHub, Gitea and Forgejo serve a repository by any of
// those.
func sameRepository(built string, pushed ...string) bool {
	want := repoKey(built)
	if want == "" {
		return false
	}
	for _, u := range pushed {
		if k := repoKey(u); k != "" && strings.EqualFold(k, want) {
			return true
		}
	}
	return false
}

func repoKey(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" && (u.Scheme != "https" || port != "443") && (u.Scheme != "http" || port != "80") {
		host += ":" + port
	}
	path := strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), ".git")
	return host + path
}

// shownRepository is a repository as a person reads it, by the first URL given.
func shownRepository(urls ...string) string {
	for _, u := range urls {
		if k := repoKey(u); k != "" {
			return k
		}
	}
	return "another repository"
}
