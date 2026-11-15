// SPDX-License-Identifier: AGPL-3.0-only

// Package githooks serves GitHookService, which turns deploy on push on and off for
// a service that builds from Git, and the receiver its hook's URL names, which
// GitHub, Gitea and Forgejo send pushes to. A push plans a build and deploy of what
// the branch names then, and applies it with an API token's rights, one deploy at
// a time for each service. See docs/adr/0046-builds-from-git.md.
package githooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/actors"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	serviceapi "github.com/dockwell/dockwell/internal/control/services"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

const (
	// Path is where a hook's URL points, its ID after it.
	Path = "/hooks/git/"
	// Action is the audit log's action for what a push came to, which the
	// timeline shows as a push (EVENT_KIND_PUSH).
	Action = "git_hook.push"
	// shown is how many of a hook's newest deliveries GetGitHook gives.
	shown = 20
)

// Canceller cancels a build, as BuildService does.
type Canceller interface {
	CancelBuild(context.Context, *connect.Request[dockwellv1.CancelBuildRequest]) (*connect.Response[dockwellv1.CancelBuildResponse], error)
}

// Config is what a Handler works with.
type Config struct {
	Auth     *auth.Service
	DB       *store.DB
	Changes  *changes.Handler
	Engine   *revision.Engine
	Services *serviceapi.Handler
	// Builds cancels a deploy's build that still waits, for a newer push. Nil
	// leaves it to finish.
	Builds Canceller
	// Sources reads what a branch names now.
	Sources revision.Sources
	Logger  *slog.Logger
}

// Handler implements GitHookService, and receives deliveries.
type Handler struct {
	cfg Config
	now func() time.Time
	// Wait is how long a deploy waits before it tries again to apply a plan that
	// another change to the service holds up; 5 seconds when zero.
	Wait time.Duration

	base   context.Context //nolint:containedctx // what runs after a delivery's answer, until Stop
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// readFor, inFlight and perSource bound what deliveries may hold
	// (SetLimits): how long one's body may take to arrive at most, and how many
	// may be read at once, and from one source. reading counts those being read,
	// and readingFrom those from each source, under readingMu.
	readFor     atomic.Int64
	inFlight    atomic.Int64
	perSource   atomic.Int64
	reading     atomic.Int64
	readingMu   sync.Mutex
	readingFrom map[string]int

	mu    sync.Mutex
	lanes map[string]*lane // by the service's ID
	// planned is the plan each service's last push left for a person, which a
	// newer one's plan takes the place of.
	planned map[string]string
}

var _ dockwellv1connect.GitHookServiceHandler = (*Handler)(nil)

// New returns a Handler.
func New(cfg Config) *Handler {
	base, cancel := context.WithCancel(context.Background())
	h := &Handler{cfg: cfg, now: time.Now, base: base, cancel: cancel, lanes: map[string]*lane{}, planned: map[string]string{}}
	h.SetLimits(ReadFor, InFlight, PerSource)
	return h
}

// ReadFor is the longest a delivery's body may take to arrive: GitHub gives up on
// a hook after 10 seconds, and a body has less the shorter it says it is
// (readDeadline). InFlight is how many deliveries are read at once, so bodies that
// trickle in from many addresses can't hold the control plane's memory, and
// PerSource how many of those one source may hold, an IPv6 /48 counting as one,
// so no one sender holds them all.
const (
	ReadFor   = 15 * time.Second
	InFlight  = 32
	PerSource = 4
)

// SetLimits sets how long a delivery's body may take at most, how many are read
// at once and how many from one source, as a test changes them. It may be called
// while deliveries come.
func (h *Handler) SetLimits(readFor time.Duration, inFlight, perSource int) {
	h.readFor.Store(int64(readFor))
	h.inFlight.Store(int64(inFlight))
	h.perSource.Store(int64(perSource))
}

// readDeadline is how long a body of length bytes may take to arrive: at least
// 2 seconds, and at 64 KiB a second past that, to readFor at most. A push arrives
// in one write, so only a sender that means to hold a place needs more. A body
// whose length isn't said gets readFor.
func readDeadline(length int64, readFor time.Duration) time.Duration {
	if length < 0 {
		return readFor
	}
	return min(readFor, max(2*time.Second, time.Duration(length)*time.Second/(64<<10)))
}

// startReading takes a place to read a delivery from source, and reports false
// when source holds as many as it may.
func (h *Handler) startReading(source string) bool {
	h.readingMu.Lock()
	defer h.readingMu.Unlock()
	if int64(h.readingFrom[source]) >= h.perSource.Load() {
		return false
	}
	if h.readingFrom == nil {
		h.readingFrom = map[string]int{}
	}
	h.readingFrom[source]++
	return true
}

func (h *Handler) doneReading(source string) {
	h.readingMu.Lock()
	defer h.readingMu.Unlock()
	h.readingFrom[source]--
	if h.readingFrom[source] <= 0 {
		delete(h.readingFrom, source)
	}
}

// sourceBlock is the block an address counts as for what deliveries it holds: an
// IPv4 address itself, and an IPv6 /48, as one sender holds many of its addresses.
func sourceBlock(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	if ip = ip.Unmap(); ip.Is4() {
		return ip.String()
	}
	return netip.PrefixFrom(ip, 48).Masked().String()
}

// ForgetOrphans deletes the hooks of apps that were deleted, each on record. Until
// it does, a delivery to one is taken and deploys nothing, as its app is gone.
func (h *Handler) ForgetOrphans(ctx context.Context) error {
	return h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		gone, err := tx.ForgetOrphanGitHooks(ctx)
		if err != nil {
			return err
		}
		for i := range gone {
			if err := tx.RecordEvent(ctx, store.Event{
				ID: id.New(id.Event), At: h.now(), Actor: revision.SystemActor, Surface: store.SurfaceAutomatic, Action: "git_hook.delete",
				Subject: gone[i].UserID, Target: gone[i].ResourceID, Detail: "hook " + gone[i].ID + " deleted, as its app was deleted",
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// Stop ends what deploys are under way waiting for, and waits for them: their
// changes go on, and a push that waited is forgotten, as the next push or a
// person's Deploy builds what the branch names then.
func (h *Handler) Stop() {
	// Under mu, so nothing joins wg once it's waited on.
	h.mu.Lock()
	h.cancel()
	h.mu.Unlock()
	h.wg.Wait()
}

// Recover says what became of the pushes that waited when the control plane last
// stopped, which nothing deploys now. It runs before any delivery can come.
func (h *Handler) Recover(ctx context.Context) {
	if err := h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		return tx.EndQueuedGitHookDeliveries(ctx, store.HookFailed,
			"The control plane restarted before it began, so it deploys nothing. Push again, or deploy from the panel")
	}); err != nil && ctx.Err() == nil {
		h.cfg.Logger.WarnContext(ctx, "can't end the pushes that waited when the control plane stopped", slog.String("error", err.Error()))
	}
}

// KeepForgetting forgets deliveries older than store.KeepHookDeliveries, every
// hour, until ctx is done.
func (h *Handler) KeepForgetting(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
			return tx.ForgetGitHookDeliveries(ctx, h.now().Add(-store.KeepHookDeliveries))
		}); err != nil && ctx.Err() == nil {
			h.cfg.Logger.WarnContext(ctx, "can't forget old deliveries to hooks", slog.String("error", err.Error()))
		}
		if err := h.ForgetOrphans(ctx); err != nil && ctx.Err() == nil {
			h.cfg.Logger.WarnContext(ctx, "can't delete the hooks of deleted apps", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// CreateGitHook turns deploy on push on for a service that builds from a branch.
func (h *Handler) CreateGitHook(ctx context.Context, req *connect.Request[dockwellv1.CreateGitHookRequest]) (*connect.Response[dockwellv1.CreateGitHookResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	m := req.Msg
	r, err := h.cfg.Services.Find(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	b, _, _, err := h.cfg.Services.BuildOf(ctx, r.ID, "", "")
	if err != nil {
		return nil, h.internal(ctx, "reading a service's build", err)
	}
	g := b.GetGit()
	switch {
	case g == nil:
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s runs an image rather than build one from Git, so no push deploys it. Have it build from a repository first", r.Name), "no_build")
	case isCommit(g.GetRef()):
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s builds commit %s, which no push moves, so deploy on push would do nothing. Have it build a branch first", r.Name, short(g.GetRef())), "no_branch")
	}
	client, _ := authapi.ClientFrom(ctx)
	made, err := h.cfg.Auth.CreateGitHook(ctx, client, p, r.ID, m.GetProof())
	if err != nil {
		return nil, authapi.AuthError(ctx, h.cfg.Logger, err)
	}
	hook, err := h.proto(ctx, made.Hook, nil)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.CreateGitHookResponse{Hook: hook, Secret: made.Secret}), nil
}

// GetGitHook returns a service's hook and its newest deliveries, or none.
func (h *Handler) GetGitHook(ctx context.Context, req *connect.Request[dockwellv1.GetGitHookRequest]) (*connect.Response[dockwellv1.GetGitHookResponse], error) {
	m := req.Msg
	r, err := h.cfg.Services.Find(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	var hook store.GitHook
	var deliveries []store.GitHookDelivery
	err = h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		if hook, err = tx.GitHookOf(ctx, r.ID); err != nil {
			return err
		}
		deliveries, err = tx.GitHookDeliveries(ctx, hook.ID, shown)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return connect.NewResponse(&dockwellv1.GetGitHookResponse{}), nil
	}
	if err != nil {
		return nil, h.internal(ctx, "reading a hook", err)
	}
	out, err := h.proto(ctx, hook, deliveries)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.GetGitHookResponse{Hook: out}), nil
}

// RotateGitHookSecret gives a service's hook a new secret.
func (h *Handler) RotateGitHookSecret(ctx context.Context, req *connect.Request[dockwellv1.RotateGitHookSecretRequest]) (*connect.Response[dockwellv1.RotateGitHookSecretResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	m := req.Msg
	r, err := h.cfg.Services.Find(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	client, _ := authapi.ClientFrom(ctx)
	made, err := h.cfg.Auth.RotateGitHookSecret(ctx, client, p, r.ID, m.GetProof())
	if err != nil {
		return nil, authapi.AuthError(ctx, h.cfg.Logger, err)
	}
	hook, err := h.proto(ctx, made.Hook, nil)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.RotateGitHookSecretResponse{Hook: hook, Secret: made.Secret}), nil
}

// DeleteGitHook turns deploy on push off for a service.
func (h *Handler) DeleteGitHook(ctx context.Context, req *connect.Request[dockwellv1.DeleteGitHookRequest]) (*connect.Response[dockwellv1.DeleteGitHookResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	m := req.Msg
	r, err := h.cfg.Services.Find(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	if err := h.cfg.Auth.DeleteGitHook(ctx, p, r.ID); err != nil {
		return nil, authapi.AuthError(ctx, h.cfg.Logger, err)
	}
	return connect.NewResponse(&dockwellv1.DeleteGitHookResponse{}), nil
}

// proto is a hook for the schema, its URL on the panel's address as the caller
// reached it, or on its domain when it has one.
func (h *Handler) proto(ctx context.Context, hook store.GitHook, deliveries []store.GitHookDelivery) (*dockwellv1.GitHook, error) {
	out := &dockwellv1.GitHook{
		Id: hook.ID, ServiceId: hook.ResourceID, Url: h.url(ctx, hook.ID),
		CreateTime: timestamppb.New(hook.CreatedAt), SecretTime: timestamppb.New(hook.SecretAt),
	}
	for i := range deliveries {
		out.Deliveries = append(out.Deliveries, deliveryProto(&deliveries[i]))
	}
	err := h.cfg.DB.View(ctx, func(tx *store.Tx) error {
		out.CreatedBy = actors.New().In(ctx, tx).Actor("user:" + hook.UserID)
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, "naming who made a hook", err)
	}
	return out, nil
}

func (h *Handler) url(ctx context.Context, hookID string) string {
	origin := ""
	if domain := h.cfg.Auth.Sites().Domain; domain != "" {
		origin = "https://" + domain
	} else if client, ok := authapi.ClientFrom(ctx); ok {
		origin = client.Site.Origin
	}
	return origin + Path + hookID
}

func deliveryProto(d *store.GitHookDelivery) *dockwellv1.GitHookDelivery {
	return &dockwellv1.GitHookDelivery{
		DeliveryId: d.DeliveryID, Time: timestamppb.New(d.At), Provider: d.Provider, Ref: d.Ref, Commit: d.Commit,
		Outcome: outcomes[d.Outcome], ChangeId: d.ChangeID, Detail: d.Detail,
	}
}

var outcomes = map[string]dockwellv1.GitHookOutcome{
	store.HookDeploying:   dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_DEPLOYING,
	store.HookNeedsPerson: dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_NEEDS_PERSON,
	store.HookQueued:      dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_QUEUED,
	store.HookNothingNew:  dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_NOTHING_NEW,
	store.HookIgnored:     dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_IGNORED,
	store.HookFailed:      dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_FAILED,
	store.HookSuperseded:  dockwellv1.GitHookOutcome_GIT_HOOK_OUTCOME_SUPERSEDED,
}

// told is what the audit log keeps of a push, in its detail: the delivery as it
// came to be, by the hook. Only this package writes Action.
type told struct {
	Hook     string `json:"hook"`
	Delivery string `json:"delivery"`
	Provider string `json:"provider"`
	Ref      string `json:"ref,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Outcome  string `json:"outcome"`
	Change   string `json:"change,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// ReadEvent is the push an audit event of Action tells of, for the timeline.
func ReadEvent(a *store.Event) (*dockwellv1.EventPush, bool) {
	var t told
	if a.Action != Action || json.Unmarshal([]byte(a.Detail), &t) != nil {
		return nil, false
	}
	d := store.GitHookDelivery{
		HookID: t.Hook, DeliveryID: t.Delivery, At: a.At, Provider: t.Provider, Ref: t.Ref, Commit: t.Commit,
		Outcome: t.Outcome, ChangeID: t.Change, Detail: t.Detail,
	}
	return &dockwellv1.EventPush{HookId: t.Hook, Delivery: deliveryProto(&d)}, true
}

// tell keeps what a delivery came to, with the hook, and on the timeline unless it
// did nothing at all.
func (h *Handler) tell(ctx context.Context, d store.GitHookDelivery, serviceID string) {
	ctx = context.WithoutCancel(ctx)
	err := h.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		if err := tx.SetGitHookDelivery(ctx, d); err != nil {
			return err
		}
		if d.Outcome == store.HookIgnored {
			return nil
		}
		detail, err := json.Marshal(told{
			Hook: d.HookID, Delivery: d.DeliveryID, Provider: d.Provider, Ref: d.Ref, Commit: d.Commit,
			Outcome: d.Outcome, Change: d.ChangeID, Detail: d.Detail,
		})
		if err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: h.now(), Actor: "hook:" + d.HookID, Surface: store.SurfacePush, Action: Action,
			Subject: serviceID, Target: d.HookID, Detail: string(detail),
		})
	})
	if err != nil {
		h.cfg.Logger.WarnContext(ctx, "can't keep what a push came to", slog.String("hook", d.HookID), slog.String("delivery", d.DeliveryID), slog.String("error", err.Error()))
	}
}

func (h *Handler) internal(ctx context.Context, what string, err error) error {
	if connect.CodeOf(err) != connect.CodeUnknown {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	h.cfg.Logger.ErrorContext(ctx, "can't serve hooks", slog.String("doing", what), slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("something went wrong with deploy on push. Try again, and check the control server's log if it keeps happening"))
}

// isCommit reports a ref that is a commit's full ID.
func isCommit(ref string) bool {
	if len(ref) != 40 && len(ref) != 64 {
		return false
	}
	for _, c := range ref {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// short is a commit's first 7 digits, as people read one.
func short(c string) string {
	if len(c) > 7 {
		return c[:7]
	}
	return c
}
