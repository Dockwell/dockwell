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
	"strings"

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
	name, event, delivery, signature, prefix string
}

var providers = []provider{
	{"forgejo", "X-Forgejo-Event", "X-Forgejo-Delivery", "X-Forgejo-Signature", ""},
	{"gitea", "X-Gitea-Event", "X-Gitea-Delivery", "X-Gitea-Signature", ""},
	{"github", "X-GitHub-Event", "X-GitHub-Delivery", "X-Hub-Signature-256", "sha256="},
}

// push is what Dockwell reads of a push's payload, which GitHub, Gitea and Forgejo
// each send in this shape.
type push struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		CloneURL      string `json:"clone_url"`
		HTMLURL       string `json:"html_url"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
}

// ServeHTTP receives a delivery at Path plus a hook's ID. It faces the internet as
// sign-in does: it spends from the sender's address's bucket before anything else,
// refuses a body by its length before reading it and cuts it off past MaxBody,
// and checks the signature before anything reads the body, in constant time, with
// one answer for a wrong signature and a hook there isn't. A delivery it takes it
// answers at once, 202, and deploys after.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
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
	if t, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || t != "application/json" {
		answer(w, http.StatusUnsupportedMediaType, "The delivery isn't JSON. Set the hook's content type to application/json.")
		return
	}
	var from *provider
	for i := range providers {
		if r.Header.Get(providers[i].signature) != "" {
			from = &providers[i]
			break
		}
	}
	if from == nil {
		answer(w, http.StatusUnauthorized, "The delivery isn't signed. Give the hook its secret, as Dockwell showed it.")
		return
	}
	delivery := r.Header.Get(from.delivery)
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
	sig, err := hex.DecodeString(strings.TrimPrefix(r.Header.Get(from.signature), from.prefix))
	if err != nil {
		sig = nil
	}
	hookID := strings.TrimPrefix(r.URL.Path, Path)
	p, err := h.cfg.Auth.GitHookPrincipal(r.Context(), hookID, body, sig)
	switch {
	case errors.Is(err, auth.ErrTooManyRequests):
		w.Header().Set("Retry-After", "1")
		answer(w, http.StatusTooManyRequests, "Too many deliveries to this hook. Wait a moment, then send again.")
		return
	case errors.Is(err, auth.ErrHookRejected):
		answer(w, http.StatusUnauthorized, "The delivery's signature doesn't match. Give the hook the secret Dockwell showed, or a new one from the panel.")
		return
	case err != nil:
		h.cfg.Logger.ErrorContext(r.Context(), "can't check a delivery to a hook", slog.String("error", err.Error()))
		answer(w, http.StatusServiceUnavailable, "Dockwell can't check deliveries right now. Send it again in a moment.")
		return
	}
	hook := *p.Hook
	d := store.GitHookDelivery{HookID: hook.ID, DeliveryID: delivery, At: h.now(), Provider: from.name, Outcome: store.HookIgnored}
	event := r.Header.Get(from.event)
	j, why := h.read(r.Context(), client, p, event, body, &d)
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
	case j == nil && event == "ping":
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

// read reads a delivery whose signature matched, and gives the deploy it asks for,
// or why it asks for none. It fills in d's ref.
func (h *Handler) read(ctx context.Context, client auth.Client, p auth.Principal, event string, body []byte, d *store.GitHookDelivery) (*job, string) {
	switch event {
	case "push":
	case "ping":
		return nil, "it's a ping, which says the hook works"
	default:
		return nil, "it's a " + strings.ReplaceAll(event, "_", " ") + " event, not a push"
	}
	var in push
	if err := json.Unmarshal(body, &in); err != nil || in.Ref == "" {
		return nil, "its payload isn't a push Dockwell can read"
	}
	d.Ref = in.Ref
	if in.Deleted || strings.Trim(in.After, "0") == "" {
		return nil, "it deletes " + refName(in.Ref)
	}
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
	case !sameRepository(g.GetUrl(), in.Repository.CloneURL, in.Repository.HTMLURL):
		return nil, "it's a push to " + shownRepository(in.Repository.HTMLURL, in.Repository.CloneURL) + ", and " + name + " builds from " + shownRepository(g.GetUrl())
	case !sameRef(g.GetRef(), in.Ref, in.Repository.DefaultBranch):
		return nil, "it's a push to " + refName(in.Ref) + ", and " + name + " builds " + refName(builtRef(g.GetRef()))
	}
	return &job{hook: *p.Hook, p: p, client: client}, ""
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
