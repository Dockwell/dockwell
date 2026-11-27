// SPDX-License-Identifier: AGPL-3.0-only

// Package cistatus delivers durable CI deploy status transitions from
// ci_status_outbox back to GitHub (Check Runs) and GitLab (Commit Statuses).
// Delivery runs asynchronously outside the revision engine's write lock,
// preserves monotonic transition order per change so an older status never
// regresses a newer one, retries transient failures with exponential backoff,
// and gives up after a bounded window with the reason on the change and audit
// log (ADR 0111).
package cistatus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/github"
	"github.com/dockwell/dockwell/internal/control/gitlab"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// ActionFailed is recorded on the audit log and Activity when a CI deploy
// status delivery permanently fails or gives up after retries.
const ActionFailed = "ci_status.failed"

// Default delivery bounds.
const (
	DefaultBaseBackoff  = 5 * time.Second
	DefaultMaxBackoff   = 5 * time.Minute
	DefaultGiveUpAfter  = 30 * time.Minute
	DefaultMaxAttempts  = 12
	DefaultPollInterval = 15 * time.Second
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
	MarkChecksPermissionMissing(ctx context.Context, appID string, installationID int64)
	FindGitLabStatusTarget(ctx context.Context, repoURL, serviceID string) (*GitLabTarget, string, error)
	NoteGitLabStatus(connID string, rev int, state dockwellv1.GitLabConnectionState, detail string)
}

// Config configures a Worker.
type Config struct {
	DB           *store.DB
	Targets      Targets
	GitLab       *gitlab.Client
	Logger       *slog.Logger
	PanelURL     func() string
	Now          func() time.Time
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
	GiveUpAfter  time.Duration
	MaxAttempts  int
	PollInterval time.Duration
}

// Worker drains ci_status_outbox in the background.
type Worker struct {
	cfg  Config
	wake chan struct{}

	mu       sync.Mutex
	inFlight map[string]*sync.Mutex
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
	return &Worker{
		cfg:      cfg,
		wake:     make(chan struct{}, 1),
		inFlight: map[string]*sync.Mutex{},
	}
}

func (w *Worker) now() time.Time {
	if w.cfg.Now != nil {
		return w.cfg.Now()
	}
	return time.Now()
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
// order. It is safe to call directly from tests.
func (w *Worker) DeliverOnce(ctx context.Context) error {
	if w.cfg.DB == nil {
		return nil
	}
	for {
		var due []string
		err := w.cfg.DB.View(ctx, func(tx *store.Tx) error {
			var err error
			due, err = tx.CIStatusDueChanges(ctx, w.now(), 32)
			return err
		})
		if err != nil {
			return err
		}
		if len(due) == 0 {
			return nil
		}
		progressed := false
		for _, changeID := range due {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			did, err := w.deliverChange(ctx, changeID)
			if err != nil && ctx.Err() == nil {
				w.cfg.Logger.WarnContext(ctx, "CI status delivery failed", slog.String("change", changeID), slog.String("error", err.Error()))
			}
			progressed = progressed || did
		}
		if !progressed {
			return nil
		}
	}
}

// Retry requeues the latest recorded status transition for changeID and wakes
// the delivery loop.
func (w *Worker) Retry(ctx context.Context, changeID string) error {
	panelChangeURL := w.changeURL(changeID)
	err := w.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.CIStatusRequeueLatest(ctx, changeID, panelChangeURL, w.now())
		return err
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
	cm, ok := w.inFlight[changeID]
	if !ok {
		cm = &sync.Mutex{}
		w.inFlight[changeID] = cm
	}
	w.mu.Unlock()
	cm.Lock()
	return cm.Unlock
}

func (w *Worker) deliverChange(ctx context.Context, changeID string) (bool, error) {
	unlock := w.lockChange(changeID)
	defer unlock()

	progressed := false
	for {
		var (
			pending      []store.CIStatusEntry
			maxDelivered int
			remoteID     string
			remoteURL    string
			credRef      string
		)
		err := w.cfg.DB.View(ctx, func(tx *store.Tx) error {
			var err error
			pending, maxDelivered, err = tx.CIStatusPendingForChange(ctx, changeID)
			if err != nil {
				return err
			}
			remoteID, remoteURL, credRef, err = tx.CIStatusLatestRemote(ctx, changeID)
			return err
		})
		if err != nil {
			return progressed, err
		}
		if len(pending) == 0 {
			return progressed, nil
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
				return progressed, err
			}
			progressed = true
			if len(live) == 0 {
				return progressed, nil
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
			return progressed, nil
		}
		if pickIdx > 0 {
			var drop []string
			for i := range pickIdx {
				drop = append(drop, live[i].ID)
			}
			if err := w.supersede(ctx, drop, now); err != nil {
				return progressed, err
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

		done, err := w.deliverEntry(ctx, entry)
		if err != nil {
			return progressed, err
		}
		progressed = true
		if !done {
			// Entry went into retry backoff or stopped; do not spin in this pass.
			return progressed, nil
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
	delivered     bool
	skipped       bool
	permanentFail bool
	remoteID      string
	remoteURL     string
	credentialRef string
	reason        string
	retryAfter    time.Duration
}

// deliverEntry attempts one provider HTTP call (outside any DB transaction or
// revision lock) and persists the outcome. Returns true if the entry reached a
// terminal state and the caller may advance to the next queued transition.
func (w *Worker) deliverEntry(ctx context.Context, entry store.CIStatusEntry) (bool, error) {
	if w.cfg.Targets == nil {
		return true, w.markFinal(ctx, entry, store.CIStatusDeliverySkipped, "", "No Git provider is configured.", false)
	}
	var out deliveryOutcome
	switch dockwellv1.DeployProvider(entry.Provider) {
	case dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITHUB_ACTIONS:
		out = w.deliverGitHub(ctx, entry)
	case dockwellv1.DeployProvider_DEPLOY_PROVIDER_GITLAB_CI:
		out = w.deliverGitLab(ctx, entry)
	default:
		out = deliveryOutcome{skipped: true, reason: "Status reporting is supported for GitHub and GitLab deploys."}
	}

	now := w.now()
	switch {
	case out.delivered:
		err := w.cfg.DB.Update(ctx, func(tx *store.Tx) error {
			return tx.CIStatusMarkDelivered(ctx, entry.ID, entry.ChangeID, entry.Transition, out.remoteID, out.remoteURL, out.credentialRef, now)
		})
		return true, err
	case out.skipped:
		return true, w.markFinal(ctx, entry, store.CIStatusDeliverySkipped, out.credentialRef, out.reason, false)
	case out.permanentFail:
		return true, w.markFinal(ctx, entry, store.CIStatusDeliveryFailed, out.credentialRef, out.reason, true)
	default:
		// Transient failure: retry with bounded backoff until GiveUpAfter or MaxAttempts.
		attempts := entry.Attempts + 1
		if attempts >= w.cfg.MaxAttempts || now.Sub(entry.RetrySince) >= w.cfg.GiveUpAfter {
			reason := out.reason
			if reason == "" {
				reason = "Provider could not be reached after retries."
			}
			return true, w.markFinal(ctx, entry, store.CIStatusDeliveryFailed, out.credentialRef, reason, true)
		}
		delay := w.backoff(attempts, out.retryAfter)
		nextAt := now.Add(delay)
		err := w.cfg.DB.Update(ctx, func(tx *store.Tx) error {
			return tx.CIStatusMarkRetry(ctx, entry.ID, out.credentialRef, out.reason, nextAt, now)
		})
		return false, err
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

func (w *Worker) markFinal(ctx context.Context, entry store.CIStatusEntry, deliveryState, credRef, reason string, countAttempt bool) error {
	now := w.now()
	return w.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		if err := tx.CIStatusMarkFinal(ctx, entry.ID, deliveryState, credRef, reason, countAttempt, now); err != nil {
			return err
		}
		if deliveryState == store.CIStatusDeliveryFailed {
			return tx.RecordEvent(ctx, store.Event{
				ID:      id.New(id.Event),
				At:      now,
				Actor:   revision.SystemActor,
				Surface: store.SurfaceAutomatic,
				Action:  ActionFailed,
				Subject: entry.ChangeID,
				Target:  entry.ServiceID,
				Detail:  reason,
			})
		}
		return nil
	})
}

func (w *Worker) deliverGitHub(ctx context.Context, entry store.CIStatusEntry) deliveryOutcome {
	target, skipReason, err := w.cfg.Targets.FindGitHubStatusTarget(ctx, entry.Repository, entry.ServiceID)
	if err != nil {
		return deliveryOutcome{reason: fmt.Sprintf("Couldn't read GitHub App settings: %v", err)}
	}
	if target == nil {
		return deliveryOutcome{skipped: true, reason: skipReason}
	}

	req := buildGitHubCheckRun(entry, w.now())
	var checkRunID int64
	if entry.RemoteID != "" {
		checkRunID, _ = strconv.ParseInt(entry.RemoteID, 10, 64)
	}
	// If we retried after a lost response before recording remote_id, look up
	// an existing check run by external_id so we update in place instead of
	// creating duplicates.
	if checkRunID == 0 && entry.Attempts > 0 {
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
			w.cfg.Targets.MarkChecksPermissionMissing(ctx, target.AppID, target.InstallationID)
			return deliveryOutcome{
				permanentFail: true,
				credentialRef: target.CredentialRef,
				reason:        "GitHub App lacks Checks permission. Grant Checks (read and write) in Settings, Integrations, then retry.",
			}
		case ge.Status == 401:
			return deliveryOutcome{
				permanentFail: true,
				credentialRef: target.CredentialRef,
				reason:        "GitHub refused the App's credentials. Check the GitHub App in Settings, Integrations.",
			}
		case ge.Status == 404 || ge.Status == 422:
			return deliveryOutcome{
				permanentFail: true,
				credentialRef: target.CredentialRef,
				reason:        ge.Error(),
			}
		case ge.Status == 429 || rate.Limited || ge.Status >= 500:
			return deliveryOutcome{
				credentialRef: target.CredentialRef,
				reason:        ge.Error(),
				retryAfter:    rate.RetryAfter,
			}
		default:
			return deliveryOutcome{
				permanentFail: true,
				credentialRef: target.CredentialRef,
				reason:        ge.Error(),
			}
		}
	}
	return deliveryOutcome{
		credentialRef: target.CredentialRef,
		reason:        fmt.Sprintf("Couldn't reach GitHub: %v", err),
		retryAfter:    rate.RetryAfter,
	}
}

func (w *Worker) deliverGitLab(ctx context.Context, entry store.CIStatusEntry) deliveryOutcome {
	target, skipReason, err := w.cfg.Targets.FindGitLabStatusTarget(ctx, entry.Repository, entry.ServiceID)
	if err != nil {
		return deliveryOutcome{reason: fmt.Sprintf("Couldn't read GitLab connection settings: %v", err)}
	}
	if target == nil {
		return deliveryOutcome{skipped: true, reason: skipReason}
	}

	req := buildGitLabCommitStatus(entry)
	got, err := w.cfg.GitLab.PostCommitStatus(ctx, target.BaseURL, target.Token, target.ProjectPath, entry.Commit, req)
	if err == nil {
		w.cfg.Targets.NoteGitLabStatus(target.ConnectionID, target.Revision, dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_CONNECTED, "")
		remoteURL := strings.TrimRight(target.BaseURL, "/") + "/" + target.ProjectPath + "/-/commit/" + entry.Commit
		if got.PipelineID > 0 {
			remoteURL = strings.TrimRight(target.BaseURL, "/") + "/" + target.ProjectPath + "/-/pipelines/" + strconv.FormatInt(got.PipelineID, 10)
		}
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
		return deliveryOutcome{permanentFail: true, credentialRef: target.CredentialRef, reason: err.Error()}
	case errors.As(err, &ge) && ge.Status == 401:
		msg := "GitLab refused the connection's token. Update the token in Settings, Integrations, then retry."
		w.cfg.Targets.NoteGitLabStatus(target.ConnectionID, target.Revision, dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_AUTH_FAILED, msg)
		return deliveryOutcome{permanentFail: true, credentialRef: target.CredentialRef, reason: msg}
	case errors.As(err, &ge) && ge.Status == 403:
		msg := "GitLab token lacks permission to report commit statuses. Use a token with the api scope in Settings, Integrations, then retry."
		w.cfg.Targets.NoteGitLabStatus(target.ConnectionID, target.Revision, dockwellv1.GitLabConnectionState_GIT_LAB_CONNECTION_STATE_MISSING_SCOPE, msg)
		return deliveryOutcome{permanentFail: true, credentialRef: target.CredentialRef, reason: msg}
	case errors.As(err, &ge) && !ge.Transient():
		return deliveryOutcome{permanentFail: true, credentialRef: target.CredentialRef, reason: ge.Error()}
	case errors.As(err, &ge):
		return deliveryOutcome{credentialRef: target.CredentialRef, reason: ge.Error(), retryAfter: ge.RetryAfter}
	default:
		return deliveryOutcome{credentialRef: target.CredentialRef, reason: err.Error()}
	}
}

func checkRunName(serviceName string) string {
	if serviceName == "" {
		return "Dockwell"
	}
	return "Dockwell / " + serviceName
}

func buildGitHubCheckRun(entry store.CIStatusEntry, now time.Time) github.CheckRunRequest {
	name := checkRunName(entry.ServiceName)
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
	}
	return req
}

func buildGitLabCommitStatus(entry store.CIStatusEntry) gitlab.CommitStatusRequest {
	svc := entry.ServiceName
	if svc == "" {
		svc = "service"
	}
	req := gitlab.CommitStatusRequest{
		Name:      checkRunName(entry.ServiceName),
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
