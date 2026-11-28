// SPDX-License-Identifier: AGPL-3.0-only

// Package cistatus delivers durable CI deploy status transitions from
// ci_status_outbox back to GitHub (Check Runs) and GitLab (Commit Statuses).
// Delivery runs asynchronously outside the revision engine's write lock,
// preserves monotonic transition order per change so an older status never
// regresses a newer one, enforces the deploy token's bound repository before
// touching any provider, isolates slow provider hosts, retries transient
// failures with exponential backoff, and gives up after a bounded window with
// structured reason codes on the change and audit log (ADR 0111).
package cistatus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/github"
	"github.com/dockwell/dockwell/internal/control/gitlab"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// Audit actions recorded for CI deploy status reporting.
const (
	ActionFailed  = "ci_status.failed"
	ActionRetried = "ci_status.retried"
)

// Default delivery bounds.
const (
	DefaultBaseBackoff    = 5 * time.Second
	DefaultMaxBackoff     = 5 * time.Minute
	DefaultGiveUpAfter    = 30 * time.Minute
	DefaultMaxAttempts    = 12
	DefaultPollInterval   = 15 * time.Second
	DefaultMaxHostWorkers = 4
)

// GitHubTarget is a resolved GitHub App installation and repository.
type GitHubTarget struct {
	App            *github.App
	AppID          string
	AppName        string
	InstallationID int64
	RepositoryID   int64
	FullName       string
	CredentialRef  string
}

// GitLabTarget is a resolved GitLab connection and project path.
type GitLabTarget struct {
	ConnectionID  string
	Revision      int
	Name          string
	BaseURL       string
	Token         string
	ProjectPath   string
	CredentialRef string
}

// Targets resolves provider credentials for CI status delivery without coupling
// this package to gitproviders directly.
type Targets interface {
	FindGitHubStatusTarget(ctx context.Context, repoURL, serviceID string) (*GitHubTarget, string, error)
	VerifyChecksPermission(ctx context.Context, appID string, installationID int64) bool
	FindGitLabStatusTarget(ctx context.Context, repoURL, serviceID string) (*GitLabTarget, string, error)
	NoteGitLabStatus(connID string, rev int, state dockwellv1.GitLabConnectionState, detail string)
	NoteGitLabStatusScopeMissing(connID string, rev int, detail string)
}

// Config configures a Worker.
type Config struct {
	DB             *store.DB
	Targets        Targets
	GitLab         *gitlab.Client
	Logger         *slog.Logger
	PanelURL       func() string
	Now            func() time.Time
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
	GiveUpAfter    time.Duration
	MaxAttempts    int
	PollInterval   time.Duration
	MaxHostWorkers int
}

type changeLock struct {
	mu   sync.Mutex
	refs int
}

// Worker drains ci_status_outbox in the background.
type Worker struct {
	cfg  Config
	wake chan struct{}

	mu       sync.Mutex
	inFlight map[string]*changeLock
}

// New constructs a Worker with sensible defaults.
func New(cfg Config) *Worker {
	if cfg.GitLab == nil {
		cfg.GitLab = &gitlab.Client{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = DefaultBaseBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}
	if cfg.GiveUpAfter <= 0 {
		cfg.GiveUpAfter = DefaultGiveUpAfter
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.MaxHostWorkers <= 0 {
		cfg.MaxHostWorkers = DefaultMaxHostWorkers
	}
	return &Worker{
		cfg:      cfg,
		wake:     make(chan struct{}, 1),
		inFlight: map[string]*changeLock{},
	}
}

func (w *Worker) now() time.Time {
	if w.cfg.Now != nil {
		return w.cfg.Now()
	}
	return time.Now()
}

// InFlightLen returns the number of per-change locks currently held in memory.
func (w *Worker) InFlightLen() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.inFlight)
}

// Wake nudges the background loop to check for due transitions immediately.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run drains due outbox entries until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	for {
		if err := w.DeliverOnce(ctx); err != nil && ctx.Err() == nil {
			w.cfg.Logger.WarnContext(ctx, "CI status delivery pass failed", slog.String("error", err.Error()))
		}
		wait := w.nextWait(ctx)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-w.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (w *Worker) nextWait(ctx context.Context) time.Duration {
	wait := w.cfg.PollInterval
	var (
		next time.Time
		ok   bool
	)
	if err := w.cfg.DB.View(ctx, func(tx *store.Tx) error {
		var err error
		next, ok, err = tx.CIStatusNextDue(ctx)
		return err
	}); err == nil && ok {
		d := next.Sub(w.now())
		if d <= 0 {
			return 50 * time.Millisecond
		}
		if d < wait {
			wait = d
		}
	}
	return wait
}

// DeliverOnce delivers every currently due change's pending transitions in
// order, running distinct provider hosts concurrently so one slow or down host
// cannot stall deliveries to other hosts.
func (w *Worker) DeliverOnce(ctx context.Context) error {
	if w.cfg.DB == nil {
		return nil
	}
	unreachableHosts := map[string]bool{}
	for {
		var due []store.CIStatusDue
		err := w.cfg.DB.View(ctx, func(tx *store.Tx) error {
			var err error
			due, err = tx.CIStatusDueChanges(ctx, w.now(), 64)
			return err
		})
		if err != nil {
			return err
		}
		groups := map[string][]string{}
		var hostOrder []string
		for _, d := range due {
			hk := hostKey(d.Repository, d.Provider)
			if unreachableHosts[hk] {
				continue
			}
			if _, ok := groups[hk]; !ok {
				hostOrder = append(hostOrder, hk)
			}
			groups[hk] = append(groups[hk], d.ChangeID)
		}
		if len(hostOrder) == 0 {
			return nil
		}

		var (
			progressed atomic.Bool
			hostMu     sync.Mutex
			wg         sync.WaitGroup
			sem        = make(chan struct{}, w.cfg.MaxHostWorkers)
		)
		for _, hk := range hostOrder {
			if ctx.Err() != nil {
				break
			}
			sem <- struct{}{}
			wg.Add(1)
			go func(host string, changes []string) {
				defer wg.Done()
				defer func() { <-sem }()
				for _, changeID := range changes {
					if ctx.Err() != nil {
						return
					}
					did, unreachable, err := w.deliverChange(ctx, changeID)
					if err != nil && ctx.Err() == nil {
						w.cfg.Logger.WarnContext(ctx, "CI status delivery failed", slog.String("change", changeID), slog.String("error", err.Error()))
					}
					if did {
						progressed.Store(true)
					}
					if unreachable {
						hostMu.Lock()
						unreachableHosts[host] = true
						hostMu.Unlock()
						return
					}
				}
			}(hk, groups[hk])
		}
		wg.Wait()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !progressed.Load() {
			return nil
		}
	}
}

func hostKey(repoURL string, provider int32) string {
	if u, err := url.Parse(strings.TrimSpace(repoURL)); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return fmt.Sprintf("provider:%d", provider)
}

// Retry requeues the latest recorded status transition for changeID, records an
// audit event, and wakes the delivery loop.
func (w *Worker) Retry(ctx context.Context, changeID, actor, via string) error {
	panelChangeURL := w.changeURL(changeID)
	now := w.now()
	err := w.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		entry, err := tx.CIStatusRequeueLatest(ctx, changeID, panelChangeURL, now)
		if err != nil {
			return err
		}
		if actor == "" {
			actor = revision.SystemActor
		}
		if via == "" {
			via = store.SurfaceAPI
		}
		return tx.RecordEvent(ctx, store.Event{
			ID:      id.New(id.Event),
			At:      now,
			Actor:   actor,
			Surface: via,
			Action:  ActionRetried,
			Subject: changeID,
			Target:  entry.ServiceID,
			Detail:  "retried CI deploy status delivery",
		})
	})
	if err != nil {
		return err
	}
	w.Wake()
	return nil
}

func (w *Worker) changeURL(changeID string) string {
	if w.cfg.PanelURL == nil {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(w.cfg.PanelURL()), "/")
	if base == "" {
		return ""
	}
	return base + "/changes/" + changeID
}

func (w *Worker) lockChange(changeID string) func() {
	w.mu.Lock()
	cl, ok := w.inFlight[changeID]
	if !ok {
		cl = &changeLock{}
		w.inFlight[changeID] = cl
	}
	cl.refs++
	w.mu.Unlock()
	cl.mu.Lock()
	return func() {
		cl.mu.Unlock()
		w.mu.Lock()
		cl.refs--
		if cl.refs == 0 {
			delete(w.inFlight, changeID)
		}
		w.mu.Unlock()
	}
}

func (w *Worker) deliverChange(ctx context.Context, changeID string) (progressed, hostUnreachable bool, err error) {
	unlock := w.lockChange(changeID)
	defer unlock()

	for {
		var (
			pending      []store.CIStatusEntry
			maxDelivered int
			remoteID     string
			remoteURL    string
			credRef      string
		)
		err = w.cfg.DB.View(ctx, func(tx *store.Tx) error {
			var err error
			pending, maxDelivered, err = tx.CIStatusPendingForChange(ctx, changeID)
			if err != nil {
				return err
			}
			remoteID, remoteURL, credRef, err = tx.CIStatusLatestRemote(ctx, changeID)
			return err
		})
		if err != nil {
			return progressed, false, err
		}
		if len(pending) == 0 {
			return progressed, false, nil
		}

		now := w.now()
		// Never regress: supersede any pending transition older than what was
		// already delivered.
		var staleIDs []string
		var live []store.CIStatusEntry
		for i := range pending {
			if pending[i].Transition <= maxDelivered {
				staleIDs = append(staleIDs, pending[i].ID)
			} else {
				live = append(live, pending[i])
			}
		}
		if len(staleIDs) > 0 {
			if err := w.supersede(ctx, staleIDs, now); err != nil {
				return progressed, false, err
			}
			progressed = true
			if len(live) == 0 {
				return progressed, false, nil
			}
		}

		// Pick the earliest entry that is due now. If an earlier entry is still
		// waiting in backoff while a newer transition is already due, supersede
		// the stale earlier entry so the newer status isn't blocked or regressed.
		pickIdx := -1
		for i := range live {
			if !live[i].NextAttemptAt.After(now) {
				pickIdx = i
				break
			}
		}
		if pickIdx < 0 {
			return progressed, false, nil
		}
		if pickIdx > 0 {
			var drop []string
			for i := range pickIdx {
				drop = append(drop, live[i].ID)
			}
			if err := w.supersede(ctx, drop, now); err != nil {
				return progressed, false, err
			}
			progressed = true
		}

		entry := live[pickIdx]
		if entry.RemoteID == "" && remoteID != "" {
			entry.RemoteID = remoteID
		}
		if entry.RemoteURL == "" && remoteURL != "" {
			entry.RemoteURL = remoteURL
		}
		if entry.CredentialRef == "" && credRef != "" {
			entry.CredentialRef = credRef
		}
		if entry.ChangeURL == "" {
			entry.ChangeURL = w.changeURL(entry.ChangeID)
		}

		done, unreachable, err := w.deliverEntry(ctx, entry)
		if err != nil {
			return progressed, unreachable, err
		}
		progressed = true
		if !done {
			// Entry went into retry backoff or stopped; do not spin in this pass.
			return progressed, unreachable, nil
		}
	}
}

func (w *Worker) supersede(ctx context.Context, ids []string, at time.Time) error {
	return w.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		for _, entryID := range ids {
			if err := tx.CIStatusMarkSuperseded(ctx, entryID, at); err != nil {
				return err
			}
		}
		return nil
	})
}

type deliveryOutcome struct {
	delivered       bool
	skipped         bool
	permanentFail   bool
	hostUnreachable bool
	remoteID        string
	remoteURL       string
	credentialRef   string
	reasonCode      string
	reason          string
	retryAfter      time.Duration
}

// deliverEntry attempts one provider HTTP call (outside any DB transaction or
// revision lock) and persists the outcome. Returns done=true if the entry
// reached a terminal state and the caller may advance to the next queued transition.
func (w *Worker) deliverEntry(ctx context.Context, entry store.CIStatusEntry) (done, hostUnreachable bool, err error) {
	// Enforce the deploy token's Dockwell-owned repository binding BEFORE
	// touching any provider or checking installation reach.
	if entry.AllowedRepository == "" {
		return true, false, w.markFinal(ctx, entry, store.CIStatusDeliverySkipped, "", store.CIStatusReasonNoTokenRepository, "", false)
	}
	if !store.SameDeployRepository(entry.Repository, entry.AllowedRepository) {
		return true, false, w.markFinal(ctx, entry, store.CIStatusDeliverySkipped, "", store.CIStatusReasonRepositoryMismatch, "", false)
	}

	if w.cfg.Targets == nil {
		code := store.CIStatusReasonProviderUnsupported
		switch dockwellv1.DeployProvider(entry.Provider) {
		case dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS:
			code = store.CIStatusReasonNoGitHubApp
		case dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITLAB_CI:
			code = store.CIStatusReasonNoGitLabConnection
		case dockwellv1.DeployProvider_DEPLOY_PROVIDER_UNSPECIFIED, dockwellv1.DeployProvider_DEPLOY_PROVIDER_OTHER:
		}
		return true, false, w.markFinal(ctx, entry, store.CIStatusDeliverySkipped, "", code, "", false)
	}
	var out deliveryOutcome
	switch dockwellv1.DeployProvider(entry.Provider) {
	case dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS:
		out = w.deliverGitHub(ctx, entry)
	case dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITLAB_CI:
		// N10: GitLab keys commit statuses by (sha, name) without external_id.
		// Never post an older change's transition (such as a retried discard)
		// once a newer change for the same commit and status name has already
		// delivered a status.
		var newerDelivered bool
		if err := w.cfg.DB.View(ctx, func(tx *store.Tx) error {
			var err error
			newerDelivered, err = tx.CIStatusHasNewerDeliveredForName(ctx, entry)
			return err
		}); err != nil {
			return false, false, err
		}
		if newerDelivered {
			err := w.cfg.DB.Update(ctx, func(tx *store.Tx) error {
				return tx.CIStatusMarkSuperseded(ctx, entry.ID, w.now())
			})
			return true, false, err
		}
		out = w.deliverGitLab(ctx, entry)
	default:
		out = deliveryOutcome{skipped: true, reasonCode: store.CIStatusReasonProviderUnsupported}
	}

	now := w.now()
	switch {
	case out.delivered:
		err := w.cfg.DB.Update(ctx, func(tx *store.Tx) error {
			return tx.CIStatusMarkDelivered(ctx, entry.ID, entry.ChangeID, entry.Transition, out.remoteID, out.remoteURL, out.credentialRef, now)
		})
		return true, false, err
	case out.skipped:
		return true, false, w.markFinal(ctx, entry, store.CIStatusDeliverySkipped, out.credentialRef, out.reasonCode, out.reason, false)
	case out.permanentFail:
		return true, false, w.markFinal(ctx, entry, store.CIStatusDeliveryFailed, out.credentialRef, out.reasonCode, out.reason, true)
	default:
		// Transient failure: retry with bounded backoff measured from the first
		// attempt until GiveUpAfter or MaxAttempts.
		attempts := entry.Attempts + 1
		retryStart := entry.RetrySince
		if retryStart.IsZero() {
			retryStart = now
		}
		if attempts >= w.cfg.MaxAttempts || now.Sub(retryStart) >= w.cfg.GiveUpAfter {
			return true, out.hostUnreachable, w.markFinal(ctx, entry, store.CIStatusDeliveryFailed, out.credentialRef, store.CIStatusReasonGaveUp, out.reason, true)
		}
		delay := w.backoff(attempts, out.retryAfter)
		nextAt := now.Add(delay)
		err := w.cfg.DB.Update(ctx, func(tx *store.Tx) error {
			return tx.CIStatusMarkRetry(ctx, entry.ID, out.credentialRef, out.reasonCode, out.reason, nextAt, now)
		})
		return false, out.hostUnreachable, err
	}
}

func (w *Worker) backoff(attempt int, retryAfter time.Duration) time.Duration {
	shift := max(0, min(attempt-1, 10))
	d := w.cfg.BaseBackoff << shift
	if d > w.cfg.MaxBackoff {
		d = w.cfg.MaxBackoff
	}
	if retryAfter > d {
		d = min(retryAfter, w.cfg.MaxBackoff)
	}
	return d
}

func (w *Worker) markFinal(ctx context.Context, entry store.CIStatusEntry, deliveryState, credRef, reasonCode, reason string, countAttempt bool) error {
	now := w.now()
	return w.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		if err := tx.CIStatusMarkFinal(ctx, entry.ID, deliveryState, credRef, reasonCode, reason, countAttempt, now); err != nil {
			return err
		}
		if deliveryState == store.CIStatusDeliveryFailed {
			detail := reason
			if detail == "" {
				detail = reasonCode
			}
			return tx.RecordEvent(ctx, store.Event{
				ID:      id.New(id.Event),
				At:      now,
				Actor:   revision.SystemActor,
				Surface: store.SurfaceAutomatic,
				Action:  ActionFailed,
				Subject: entry.ChangeID,
				Target:  entry.ServiceID,
				Detail:  detail,
			})
		}
		return nil
	})
}

func (w *Worker) deliverGitHub(ctx context.Context, entry store.CIStatusEntry) deliveryOutcome {
	target, reasonCode, err := w.cfg.Targets.FindGitHubStatusTarget(ctx, entry.Repository, entry.ServiceID)
	if err != nil {
		return deliveryOutcome{reasonCode: store.CIStatusReasonGitHubNotReached, reason: err.Error()}
	}
	if target == nil {
		return deliveryOutcome{skipped: true, reasonCode: reasonCode}
	}

	req := buildGitHubCheckRun(entry, w.now())
	var checkRunID int64
	if entry.RemoteID != "" {
		checkRunID, _ = strconv.ParseInt(entry.RemoteID, 10, 64)
	}
	// Always look up an existing check run by external_id before creating one,
	// so even a process restart or lost response on attempt 0 updates in place
	// instead of creating a duplicate check run.
	if checkRunID == 0 {
		if existing, _, ferr := target.App.FindCheckRun(ctx, target.InstallationID, target.RepositoryID, target.FullName, entry.Commit, req.Name, entry.ChangeID); ferr == nil && existing != nil {
			checkRunID = existing.ID
		}
	}

	var (
		got  *github.CheckRun
		rate github.Rate
	)
	if checkRunID > 0 {
		got, rate, err = target.App.UpdateCheckRun(ctx, target.InstallationID, target.RepositoryID, target.FullName, checkRunID, req)
	} else {
		got, rate, err = target.App.CreateCheckRun(ctx, target.InstallationID, target.RepositoryID, target.FullName, req)
	}
	if err == nil {
		return deliveryOutcome{
			delivered:     true,
			remoteID:      strconv.FormatInt(got.ID, 10),
			remoteURL:     got.HTMLURL,
			credentialRef: target.CredentialRef,
		}
	}

	var ge *github.Error
	if errors.As(err, &ge) {
		switch {
		case ge.Status == 403 && !rate.Limited,
			ge.Status == 422 && strings.Contains(strings.ToLower(ge.Message), "permission"):
			if w.cfg.Targets.VerifyChecksPermission(ctx, target.AppID, target.InstallationID) {
				return deliveryOutcome{
					permanentFail: true,
					credentialRef: target.CredentialRef,
					reasonCode:    store.CIStatusReasonMissingChecksPermission,
					reason:        ge.Error(),
				}
			}
			return deliveryOutcome{
				permanentFail: true,
				credentialRef: target.CredentialRef,
				reasonCode:    store.CIStatusReasonGitHubAuthFailed,
				reason:        ge.Error(),
			}
		case ge.Status == 401:
			return deliveryOutcome{
				permanentFail: true,
				credentialRef: target.CredentialRef,
				reasonCode:    store.CIStatusReasonGitHubAuthFailed,
				reason:        ge.Error(),
			}
		case ge.Status == 404 || ge.Status == 422:
			return deliveryOutcome{
				permanentFail: true,
				credentialRef: target.CredentialRef,
				reasonCode:    store.CIStatusReasonGitHubNotReached,
				reason:        ge.Error(),
			}
		case ge.Status == 429 || rate.Limited || ge.Status >= 500:
			return deliveryOutcome{
				credentialRef: target.CredentialRef,
				reasonCode:    store.CIStatusReasonGitHubNotReached,
				reason:        ge.Error(),
				retryAfter:    rate.RetryAfter,
			}
		default:
			return deliveryOutcome{
				permanentFail: true,
				credentialRef: target.CredentialRef,
				reasonCode:    store.CIStatusReasonGitHubNotReached,
				reason:        ge.Error(),
			}
		}
	}
	return deliveryOutcome{
		hostUnreachable: true,
		credentialRef:   target.CredentialRef,
		reasonCode:      store.CIStatusReasonGitHubNotReached,
		reason:          err.Error(),
		retryAfter:      rate.RetryAfter,
	}
}

func (w *Worker) deliverGitLab(ctx context.Context, entry store.CIStatusEntry) deliveryOutcome {
	target, reasonCode, err := w.cfg.Targets.FindGitLabStatusTarget(ctx, entry.Repository, entry.ServiceID)
	if err != nil {
		return deliveryOutcome{reasonCode: store.CIStatusReasonGitLabNotReached, reason: err.Error()}
	}
	if target == nil {
		return deliveryOutcome{skipped: true, reasonCode: reasonCode}
	}

	req := buildGitLabCommitStatus(entry)
	got, err := w.cfg.GitLab.PostCommitStatus(ctx, target.BaseURL, target.Token, target.ProjectPath, entry.Commit, req)
	if err == nil {
		w.cfg.Targets.NoteGitLabStatus(target.ConnectionID, target.Revision, dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED, "")
		remoteURL := gitlab.CommitStatusURL(target.BaseURL, target.ProjectPath, entry.Commit, got.PipelineID)
		return deliveryOutcome{
			delivered:     true,
			remoteID:      strconv.FormatInt(got.ID, 10),
			remoteURL:     remoteURL,
			credentialRef: target.CredentialRef,
		}
	}

	var ge *gitlab.Error
	switch {
	case errors.Is(err, netguard.ErrForbidden):
		w.cfg.Targets.NoteGitLabStatus(target.ConnectionID, target.Revision, dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_UNREACHABLE, err.Error())
		return deliveryOutcome{permanentFail: true, credentialRef: target.CredentialRef, reasonCode: store.CIStatusReasonNetguardBlocked, reason: err.Error()}
	case errors.As(err, &ge) && ge.Status == 401:
		w.cfg.Targets.NoteGitLabStatus(target.ConnectionID, target.Revision, dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_AUTH_FAILED, ge.Error())
		return deliveryOutcome{permanentFail: true, credentialRef: target.CredentialRef, reasonCode: store.CIStatusReasonGitLabAuthFailed, reason: ge.Error()}
	case errors.As(err, &ge) && ge.Status == 403:
		w.cfg.Targets.NoteGitLabStatusScopeMissing(target.ConnectionID, target.Revision, ge.Error())
		return deliveryOutcome{permanentFail: true, credentialRef: target.CredentialRef, reasonCode: store.CIStatusReasonGitLabMissingScope, reason: ge.Error()}
	case errors.As(err, &ge) && !ge.Transient():
		return deliveryOutcome{permanentFail: true, credentialRef: target.CredentialRef, reasonCode: store.CIStatusReasonGitLabNotReached, reason: ge.Error()}
	case errors.As(err, &ge):
		return deliveryOutcome{credentialRef: target.CredentialRef, reasonCode: store.CIStatusReasonGitLabNotReached, reason: ge.Error(), retryAfter: ge.RetryAfter}
	default:
		return deliveryOutcome{hostUnreachable: true, credentialRef: target.CredentialRef, reasonCode: store.CIStatusReasonGitLabNotReached, reason: err.Error()}
	}
}

func checkRunName(envName, serviceName string) string {
	switch {
	case envName != "" && serviceName != "":
		return "Dockwell / " + envName + " / " + serviceName
	case serviceName != "":
		return "Dockwell / " + serviceName
	default:
		return "Dockwell"
	}
}

func buildGitHubCheckRun(entry store.CIStatusEntry, now time.Time) github.CheckRunRequest {
	name := checkRunName(entry.EnvironmentName, entry.ServiceName)
	svc := entry.ServiceName
	if svc == "" {
		svc = "service"
	}
	req := github.CheckRunRequest{
		Name:       name,
		HeadSHA:    entry.Commit,
		DetailsURL: entry.ChangeURL,
		ExternalID: entry.ChangeID,
	}
	ts := now.UTC()
	switch entry.State {
	case store.CIStatusQueued:
		req.Status = "queued"
		req.Output = &github.CheckRunOutput{
			Title:   "Deploy queued",
			Summary: "Waiting to deploy " + svc + ".",
		}
	case store.CIStatusApplying:
		req.Status = "in_progress"
		req.StartedAt = &ts
		req.Output = &github.CheckRunOutput{
			Title:   "Deploying",
			Summary: "Deploying " + svc + ".",
		}
	case store.CIStatusHealthy:
		req.Status = "completed"
		req.Conclusion = "success"
		req.CompletedAt = &ts
		summary := svc + " is healthy."
		if entry.Digest != "" {
			summary = fmt.Sprintf("%s is healthy (%s).", svc, shortDigest(entry.Digest))
		}
		req.Output = &github.CheckRunOutput{Title: "Deployed", Summary: summary}
	case store.CIStatusFailed:
		req.Status = "completed"
		req.Conclusion = "failure"
		req.CompletedAt = &ts
		summary := svc + " failed to deploy."
		if entry.Summary != "" {
			summary = fmt.Sprintf("%s failed to deploy: %s", svc, entry.Summary)
		}
		req.Output = &github.CheckRunOutput{Title: "Deploy failed", Summary: summary}
	case store.CIStatusRolledBack:
		req.Status = "completed"
		req.Conclusion = "failure"
		req.CompletedAt = &ts
		summary := svc + " was rolled back."
		if entry.Summary != "" {
			summary = fmt.Sprintf("%s was rolled back: %s", svc, entry.Summary)
		}
		req.Output = &github.CheckRunOutput{Title: "Rolled back", Summary: summary}
	case store.CIStatusDiscarded:
		req.Status = "completed"
		req.Conclusion = "cancelled"
		req.CompletedAt = &ts
		summary := svc + " deploy was discarded."
		if entry.Summary != "" {
			summary = entry.Summary
		}
		req.Output = &github.CheckRunOutput{Title: "Deploy discarded", Summary: summary}
	}
	return req
}

func buildGitLabCommitStatus(entry store.CIStatusEntry) gitlab.CommitStatusRequest {
	svc := entry.ServiceName
	if svc == "" {
		svc = "service"
	}
	req := gitlab.CommitStatusRequest{
		Name:      checkRunName(entry.EnvironmentName, entry.ServiceName),
		TargetURL: entry.ChangeURL,
		Ref:       shortRef(entry.Ref),
	}
	switch entry.State {
	case store.CIStatusQueued:
		req.State = "pending"
		req.Description = "Deploy queued for " + svc
	case store.CIStatusApplying:
		req.State = "running"
		req.Description = "Deploying " + svc
	case store.CIStatusHealthy:
		req.State = "success"
		req.Description = "Deployed " + svc
	case store.CIStatusFailed:
		req.State = "failed"
		if entry.Summary != "" {
			req.Description = fmt.Sprintf("Deploy failed for %s: %s", svc, entry.Summary)
		} else {
			req.Description = "Deploy failed for " + svc
		}
	case store.CIStatusRolledBack:
		req.State = "failed"
		req.Description = "Rolled back " + svc
	case store.CIStatusDiscarded:
		req.State = "canceled"
		if entry.Summary != "" {
			req.Description = entry.Summary
		} else {
			req.Description = "Deploy discarded for " + svc
		}
	}
	return req
}

func shortDigest(d string) string {
	if strings.HasPrefix(d, "sha256:") && len(d) > 19 {
		return d[:19]
	}
	return d
}

func shortRef(ref string) string {
	ref = strings.TrimPrefix(ref, "refs/heads/")
	ref = strings.TrimPrefix(ref, "refs/tags/")
	return ref
}
