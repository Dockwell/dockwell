// SPDX-License-Identifier: AGPL-3.0-only

package githooks

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/store"
)

// AppPath is where a GitHub App's webhook points, the ID its manifest named after
// it (ADR 0098). One webhook carries every installation's pushes.
const AppPath = "/hooks/github/"

// Apps is what the App's receiver asks of the GitHub Apps Dockwell made.
type Apps interface {
	// Webhook is the App whose webhook ID is hookID, with the secrets a delivery
	// may be signed with now. ok is false where there's none.
	Webhook(ctx context.Context, hookID string) (app App, ok bool, err error)
	// Installed reads an installation again from GitHub, as an event of it says it
	// changed, or keeps that it's removed.
	Installed(ctx context.Context, appID string, installation int64, removed bool) error
	// Reaches says whether installation of the App appID reaches the project of
	// the service serviceID, and is neither suspended nor removed.
	Reaches(ctx context.Context, appID string, installation int64, serviceID string) (bool, error)
	// Serves names the App whose installation reaches the service serviceID's
	// project and lists its repository repoURL, or "" for none.
	Serves(ctx context.Context, repoURL, serviceID string) (string, error)
	// Delivered says one of the App's deliveries reached the panel, so Dockwell
	// needn't check for pushes itself.
	Delivered(appID string)
}

// App is a GitHub App as its receiver needs it.
type App struct {
	// ID is its resource's.
	ID string
	// Secrets are what a delivery may be signed with: its webhook secret, and the
	// one before for 10 minutes after a new one.
	Secrets [][]byte
}

// decoySecret is checked against a delivery to a webhook there isn't, so its answer
// takes as long as one to an App there is.
var decoySecret = func() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}()

// signed says sig is body's HMAC-SHA-256 under one of secrets, compared in
// constant time; each is compared, so the time says nothing of which matched.
func signed(secrets [][]byte, body, sig []byte) bool {
	ok := false
	for _, s := range secrets {
		mac := hmac.New(sha256.New, s)
		_, _ = mac.Write(body)
		if hmac.Equal(mac.Sum(nil), sig) {
			ok = true
		}
	}
	return ok
}

// appEvent is what an App's delivery says beside a push: its action, and its
// installation.
type appEvent struct {
	Action       string `json:"action"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
}

// ServeGitHubApp receives a GitHub App's deliveries: held to what a hook's are,
// with the App's webhook secret as the proof. A push deploys each app that builds
// the branch it moved, through one of the App's installations that reaches the
// app's project, with deploy on push on, as that app's hook would; an installation
// event has the App read the installation again.
func (h *Handler) ServeGitHubApp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(readDeadline(r.ContentLength, time.Duration(h.readFor.Load())))); err != nil {
		h.cfg.Logger.WarnContext(r.Context(), "can't give a delivery's body a deadline", slog.String("error", err.Error()))
	}
	client, _ := authapi.ClientFrom(r.Context())
	hookID := strings.TrimPrefix(r.URL.Path, AppPath)
	if h.cfg.Apps == nil || !h.cfg.Auth.TakeHookDelivery(client, "app:"+hookID) {
		w.Header().Set("Retry-After", "2")
		answer(w, http.StatusTooManyRequests, "Too many deliveries from this address. Wait a moment, then send again.")
		return
	}
	if r.ContentLength > MaxBody {
		answer(w, http.StatusRequestEntityTooLarge, "The delivery is larger than 1 MiB, which no push is.")
		return
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256="))
	if err != nil || len(sig) == 0 {
		answer(w, http.StatusUnauthorized, "The delivery isn't signed. Dockwell's GitHub App signs each, with the secret Dockwell gave it.")
		return
	}
	if t, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || t != "application/json" {
		answer(w, http.StatusUnsupportedMediaType, "The delivery isn't JSON, as Dockwell's GitHub App sends it.")
		return
	}
	source := sourceBlock(client.Addr)
	if !h.startReading(source) {
		w.Header().Set("Retry-After", "2")
		w.Header().Set("Connection", "close")
		answer(w, http.StatusTooManyRequests, "Too many deliveries under way from this address. Wait for them, then send again.")
		return
	}
	defer h.doneReading(source)
	if h.reading.Add(1) > h.inFlight.Load() {
		h.reading.Add(-1)
		w.Header().Set("Retry-After", "2")
		w.Header().Set("Connection", "close")
		answer(w, http.StatusServiceUnavailable, "Dockwell has many deliveries under way. Send it again in a moment.")
		return
	}
	defer h.reading.Add(-1)
	delivery := r.Header.Get("X-GitHub-Delivery")
	if !deliveryID(delivery) {
		answer(w, http.StatusBadRequest, "The delivery has no ID in X-GitHub-Delivery, which Dockwell needs to tell it from one sent again.")
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
	app, found, err := h.cfg.Apps.Webhook(r.Context(), hookID)
	if err != nil {
		h.cfg.Logger.ErrorContext(r.Context(), "can't read a GitHub App's webhook secret", slog.String("error", err.Error()))
		answer(w, http.StatusServiceUnavailable, "Dockwell can't check deliveries right now. Send it again in a moment.")
		return
	}
	secrets := app.Secrets
	if !found {
		secrets = [][]byte{decoySecret}
	}
	if !signed(secrets, body, sig) || !found {
		answer(w, http.StatusUnauthorized, "The delivery's signature doesn't match the App's webhook secret. Give it a new one from the panel.")
		return
	}
	h.cfg.Auth.ReturnHookDelivery(client)
	h.cfg.Apps.Delivered(app.ID)
	ctx := r.Context()
	event := r.Header.Get("X-GitHub-Event")
	d := store.GitHookDelivery{HookID: app.ID, DeliveryID: delivery, At: h.now(), Provider: "github-app", Outcome: store.HookIgnored}
	if err := h.cfg.DB.Update(ctx, func(tx *store.Tx) error { return tx.RecordGitHookDelivery(ctx, d) }); errors.Is(err, store.ErrConflict) {
		answer(w, http.StatusAccepted, "Dockwell has had this delivery already, so it does nothing.")
		return
	} else if err != nil {
		h.cfg.Logger.ErrorContext(ctx, "can't keep a GitHub App's delivery", slog.String("app", app.ID), slog.String("error", err.Error()))
		answer(w, http.StatusServiceUnavailable, "Dockwell can't keep deliveries right now. Send it again in a moment.")
		return
	}
	var ev appEvent
	_ = json.Unmarshal(body, &ev)
	switch event {
	case "ping":
		h.noteApp(ctx, d, "A ping: the App's webhook reaches the panel")
		answer(w, http.StatusAccepted, "Taken. The App's webhook works: a push to a branch an app builds deploys it.")
	case "installation", "installation_repositories":
		h.noteApp(ctx, d, "Installation "+strconv.FormatInt(ev.Installation.ID, 10)+" "+clip(strings.ReplaceAll(ev.Action, "_", " "), maxEvent))
		if ev.Installation.ID > 0 {
			h.installed(app.ID, ev.Installation.ID, event == "installation" && ev.Action == "deleted")
		}
		answer(w, http.StatusAccepted, "Taken. Dockwell reads the installation again.")
	case "push":
		in, err := readGitHub(body)
		if err != nil || len(in.refs)+len(in.gone) == 0 || !in.fits() || ev.Installation.ID <= 0 {
			h.noteApp(ctx, d, "Its payload isn't a push Dockwell can read")
			answer(w, http.StatusAccepted, "Taken. It isn't a push Dockwell can read, so it does nothing.")
			return
		}
		n := h.appPush(ctx, client, app, ev.Installation.ID, in, d)
		if n == 0 {
			h.noteApp(ctx, d, "No app with deploy on push on builds what it moved")
			answer(w, http.StatusAccepted, "Taken. No app with deploy on push on builds what it moved, so it does nothing.")
			return
		}
		h.noteApp(ctx, d, fmt.Sprintf("Deploys %d %s", n, plural(n, "app", "apps")))
		answer(w, http.StatusAccepted, fmt.Sprintf("Taken. Dockwell deploys %d %s from what it moved.", n, plural(n, "app", "apps")))
	default:
		h.noteApp(ctx, d, "A "+clip(strings.ReplaceAll(event, "_", " "), maxEvent)+" event, not a push")
		answer(w, http.StatusAccepted, "Taken. It isn't a push, so it does nothing.")
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// noteApp keeps what an App's delivery came to.
func (h *Handler) noteApp(ctx context.Context, d store.GitHookDelivery, detail string) {
	d.Detail = detail
	if err := h.cfg.DB.Update(ctx, func(tx *store.Tx) error { return tx.SetGitHookDelivery(ctx, d) }); err != nil {
		h.cfg.Logger.WarnContext(ctx, "can't keep what a GitHub App's delivery came to", slog.String("app", d.HookID), slog.String("error", err.Error()))
	}
}

// installed has the App read an installation again, after the delivery is
// answered, as GitHub gives a delivery 10 seconds.
func (h *Handler) installed(appID string, installation int64, removed bool) {
	if h.base.Err() != nil {
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		if err := h.cfg.Apps.Installed(h.base, appID, installation, removed); err != nil {
			h.cfg.Logger.WarnContext(h.base, "can't read a GitHub App's installation again", slog.String("app", appID),
				slog.Int64("installation", installation), slog.String("error", err.Error()))
		}
	}()
}

// appPush queues a deploy of each app the push moved the build of, through an
// installation that reaches it, with deploy on push on through the App, and says
// how many.
func (h *Handler) appPush(ctx context.Context, client auth.Client, app App, installation int64, in pushed, d store.GitHookDelivery) int {
	if len(in.refs) == 0 {
		return 0 // a deletion deploys nothing
	}
	var hooks []store.GitHook
	if err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		hooks, err = tx.GitHooksVia(ctx, store.ViaGitHub)
		return err
	}); err != nil {
		h.cfg.Logger.ErrorContext(ctx, "can't list the hooks a GitHub App delivers to", slog.String("error", err.Error()))
		return 0
	}
	n := 0
	for i := range hooks {
		hook := &hooks[i]
		if reach, err := h.cfg.Apps.Reaches(ctx, app.ID, installation, hook.ResourceID); err != nil || !reach {
			continue
		}
		p, err := h.cfg.Auth.AppHookPrincipal(ctx, hook.ID)
		if err != nil {
			continue
		}
		b, _, _, err := h.cfg.Services.BuildOf(authapi.As(ctx, client, p), hook.ResourceID, "", "")
		g := b.GetGit()
		if err != nil || g == nil || isCommit(g.GetRef()) || !sameRepository(g.GetUrl(), in.repos...) {
			continue
		}
		ref := ""
		for _, pushedRef := range in.refs {
			if sameRef(g.GetRef(), pushedRef, in.defaultBranch) {
				ref = pushedRef
			}
		}
		if ref == "" {
			continue
		}
		hd := store.GitHookDelivery{
			HookID: hook.ID, DeliveryID: d.DeliveryID, At: d.At, Provider: "github", Ref: ref,
			Outcome: store.HookQueued, Detail: "Reads " + refName(ref) + " and deploys what it names",
		}
		if err := h.cfg.DB.Update(ctx, func(tx *store.Tx) error { return tx.RecordGitHookDelivery(ctx, hd) }); err != nil {
			continue // had already, or can't be kept: either way, not deployed twice
		}
		h.enqueue(&job{hook: *hook, p: p, client: client, delivery: hd})
		n++
	}
	return n
}

// Polled deploys a push Dockwell found by checking a branch itself, for a hook
// whose pushes come through a GitHub App GitHub can't reach: as a delivery of it
// would, in its lane, as its maker (ADR 0098). The same commit found again does
// nothing more.
func (h *Handler) Polled(ctx context.Context, hookID, ref, commit string) error {
	p, err := h.cfg.Auth.AppHookPrincipal(ctx, hookID)
	if err != nil {
		return err
	}
	d := store.GitHookDelivery{
		HookID: hookID, DeliveryID: "poll-" + commit, At: h.now(), Provider: "github", Ref: ref, Commit: commit,
		Outcome: store.HookQueued, Detail: "Dockwell found " + refName(ref) + " moved, and deploys what it names",
	}
	err = h.cfg.DB.Update(ctx, func(tx *store.Tx) error { return tx.RecordGitHookDelivery(ctx, d) })
	if errors.Is(err, store.ErrConflict) {
		return nil
	} else if err != nil {
		return err
	}
	h.enqueue(&job{hook: *p.Hook, p: p, delivery: d})
	return nil
}
