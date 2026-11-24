// SPDX-License-Identifier: AGPL-3.0-only

// Package changes serves dockwell.v1.ChangeService: the API over the
// revision engine. Every surface changes managed state through it, so it
// adds no rules of its own beyond who may apply what: it fills in the
// hidden defaults a request leaves out, converts plans for the schema, and
// lets clients follow a change as it applies. See
// docs/adr/0017-the-change-api-and-the-agent-stream.md.
package changes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/control/actors"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/paging"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/control/settings"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/catalogsource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// Page sizes for ListChanges.
const (
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// Handler implements ChangeService.
type Handler struct {
	engine *revision.Engine
	auth   *auth.Service
	db     *store.DB
	logger *slog.Logger

	mu sync.Mutex
	// appliedIn is the session each change to who reaches a server was applied
	// from, as only that session may confirm it.
	appliedIn map[string]string
	// removalNotes say what a server's removal does beyond its resources, such as
	// to certificates by DNS (ADR 0090). Nil says nothing.
	removalNotes func(ctx context.Context, server, name string) ([]revision.Note, error)
	// gitLogins checks and fills what fetches a build's private repository, for
	// its project (ADR 0098). Nil leaves a build as it's sent.
	gitLogins GitLogins
	// Address has edits that route a catalog app's web to a domain, or stop, write
	// what follows the app's address too, and says so (ADR 0103). Nil writes nothing
	// more.
	Address func(ctx context.Context, edits []revision.Edit) ([]revision.Edit, []revision.Note, error)
	// DiscardDockerArchives releases staging for a discarded Docker archive action.
	DiscardDockerArchives func(context.Context, *dockwellv1.Change)
}

// GitLogins checks a build's git login, as a plan is made: one named must reach
// the project and list the repository, and one left out is the installation of a
// GitHub App that does, or "" where none does.
type GitLogins interface {
	PinGitLogin(ctx context.Context, tx *store.Tx, project, repoURL, login string) (string, error)
}

// SetGitLogins has plans check and fill builds' git logins with g (ADR 0098).
func (h *Handler) SetGitLogins(g GitLogins) { h.gitLogins = g }

// SetRemovalNotes says what a server's removal does beyond its resources, for its
// plan (ADR 0090).
func (h *Handler) SetRemovalNotes(fn func(ctx context.Context, server, name string) ([]revision.Note, error)) {
	h.removalNotes = fn
}

var _ dockwellv1connect.ChangeServiceHandler = (*Handler)(nil)

// New returns a handler over engine, which reads db. people checks the passkey a
// plan that's as good as root takes.
func New(engine *revision.Engine, people *auth.Service, db *store.DB, logger *slog.Logger) *Handler {
	return &Handler{engine: engine, auth: people, db: db, logger: logger, appliedIn: map[string]string{}}
}

// by is who is calling, and from where, as auth.Client.Surface says: a
// session is the panel, and an API token the API, or the CLI when its
// User-Agent says so. A change's audit events record the same.
func by(ctx context.Context) (revision.By, auth.Principal, error) {
	if dockwell, _ := ctx.Value(asDockwell{}).(bool); dockwell {
		return revision.By{Actor: revision.SystemActor, Via: revision.ViaAutomatic}, auth.Principal{}, nil
	}
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		// The interceptor lets no call here through without one.
		return revision.By{}, p, connect.NewError(connect.CodeUnauthenticated, auth.ErrUnauthenticated)
	}
	client, _ := authapi.ClientFrom(ctx)
	return revision.By{Actor: p.Actor(), Via: client.Surface(p)}, p, nil
}

type asDockwell struct{}

// AsDockwell is ctx for a change Dockwell proposes on its own, such as an
// automatic update (ADR 0080): a method that plans through this handler records
// Dockwell as who proposed it, from nowhere a person was. Only the control
// plane's own loops make one; no request carries it.
func AsDockwell(ctx context.Context) context.Context {
	return context.WithValue(ctx, asDockwell{}, true)
}

// IsDockwell says ctx is for a change Dockwell proposes on its own, as AsDockwell
// makes it: an automatic update's, when it plans an upgrade (ADR 0080).
func IsDockwell(ctx context.Context) bool {
	dockwell, _ := ctx.Value(asDockwell{}).(bool)
	return dockwell
}

type dockwellAt struct{}

// AsDockwellAt is AsDockwell at the time the loop that proposes judges by, as an
// automatic update's window is, so a plan's own checks of time, such as a catalog
// release's wait, judge by the same moment (ADR 0080).
func AsDockwellAt(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(AsDockwell(ctx), dockwellAt{}, at)
}

// DockwellAt is the time AsDockwellAt gave ctx, and false for none.
func DockwellAt(ctx context.Context) (time.Time, bool) {
	at, ok := ctx.Value(dockwellAt{}).(time.Time)
	return at, ok
}

// ownMethods are the kinds that change only through methods of their own, which do
// what a plan of one needs, with what to use instead. A catalog source's read its
// commit, and say in the plan what it holds and leaves out (ADR 0055).
var ownMethods = map[string]string{
	string(catalogsource.Kind): "a catalog source changes through dockwell catalog set-source and dockwell catalog remove-source, which read its commit and say in the plan what it holds. Use those",
}

// ProposeChange plans edits as a new change.
func (h *Handler) ProposeChange(ctx context.Context, req *connect.Request[dockwellv1.ProposeChangeRequest]) (*connect.Response[dockwellv1.ProposeChangeResponse], error) {
	who, _, err := by(ctx)
	if err != nil {
		return nil, err
	}
	summary := strings.TrimSpace(req.Msg.GetSummary())
	if summary == "" {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, errors.New("a change needs a summary that says what it is for, such as \"Add a staging environment\""), "invalid")
	}
	for _, e := range req.Msg.GetEdits() {
		if e.GetMustCreate() && (e.GetDelete() || e.GetBaseRevision() != 0 || len(e.GetServers()) != 1 ||
			(e.GetSpec().GetNetwork() == nil && e.GetSpec().GetVolume() == nil)) {
			return nil, authapi.Invalid(errors.New("create-only needs a new network or volume and one server"))
		}
		if how, ok := ownMethods[e.GetResource().GetKind()]; ok {
			return nil, authapi.Invalid(errors.New(how))
		}
	}
	c, err := h.proposeAlso(ctx, who, summary, req.Msg.GetEdits(), nil, Also{SkipBackup: req.Msg.GetSkipBackup()})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.ProposeChangeResponse{Change: c}), nil
}

// Also is what another method adds to the edits it plans: edits the API has no
// words for, as a project it brings to life with an ID its services take, and
// what the plan should say beside its steps.
type Also struct {
	Edits []revision.Edit
	Notes []revision.Note
	// IDs are what new resources the edits make are called, by what's edited, where
	// the method chose one up front (revision.Edit.ID).
	IDs map[resource.Ref]string
	// MustCreate names additions that must still be absent when the proposal loads them.
	MustCreate map[resource.Ref]bool
	// Base is the revision the request's edits were made from (revision.Edit).
	Base int
	// IfNothing refuses a plan that would do nothing (revision.Proposal).
	IfNothing error
	// Pull asks the server for the image of what's edited again (revision.Edit).
	Pull bool
	// Run says this asks what's edited to run (revision.Edit).
	Run bool
	// Adopts is what each edit takes on, by what's edited. A change can take on
	// several projects, whose services may share a name.
	Adopts map[resource.Ref]Adoption
	// Public are the ports each edit makes public on purpose, by what's edited, as a
	// catalog entry declares them (revision.Edit.Public, ADR 0101).
	Public map[resource.Ref][]string
	// SetsArchitectures are the edits that set where their service runs, as a
	// catalog install and upgrade do (revision.Edit.SetsArchitectures, ADR 0102).
	SetsArchitectures map[resource.Ref]bool
	// Intent is what the plan is for, when the method proposing it knows.
	Intent *revision.Intent
	// BackUp names resources to back up first (revision.Edit), and BackUpOne apps'
	// databases to dump alone first (revision.Proposal).
	BackUp    []resource.Ref
	BackUpOne []revision.OneBackup
	// Empties is the app the change starts on an empty database of another engine
	// (revision.Proposal.Empties).
	Empties string
	// SkipBackup leaves out the backup a database's move takes first
	// (revision.Proposal).
	SkipBackup bool
	// Unholds are the edits that may lift a catalog install's hold, as an
	// applied OpenCatalogInstall does (ADR 0105).
	Unholds []resource.Ref
	// Owners are the app owners the catalog writes, as an install makes one and
	// SetCatalogOwnerEmail gives one its email, which no other write may (ADR 0105).
	Owners []resource.Ref
	// SetsUp are the held routes whose setup link SetUpCatalogInstall writes, which no
	// other write may (ADR 0105).
	SetsUp []resource.Ref
	// Restores are backups the change restores into what it makes, by the resource
	// that restores each: a new database's service (ADR 0106).
	Restores map[resource.Ref]resource.RestoreSource
	// Records go on the activity log with the planned change, in its transaction
	// (revision.Proposal.Records).
	Records func(change string) []store.Event
}

// Adoption is a container a change takes on: its full ID, and what it is called.
type Adoption struct {
	ID, Name string
}

// Propose plans edits as a new change, as ProposeChange does, for another
// method that makes edits of its own, such as ImportCompose. field names
// the request's field that takes a choice the edits leave out, from the
// edit's field, such as "edits[0].project", which is empty for a choice
// of servers.
func (h *Handler) Propose(ctx context.Context, summary string, edits []*dockwellv1.Edit, field func(choice *scope.ChoiceError, edit string) string, also Also) (*dockwellv1.Change, error) {
	who, _, err := by(ctx)
	if err != nil {
		return nil, err
	}
	return h.proposeAlso(ctx, who, summary, edits, field, also)
}

func (h *Handler) proposeAlso(ctx context.Context, who revision.By, summary string, in []*dockwellv1.Edit, field func(*scope.ChoiceError, string) string, also Also) (*dockwellv1.Change, error) {
	fail := func(err error) error {
		var choice *scope.ChoiceError
		if field != nil && errors.As(err, &choice) {
			var at *choiceAt
			edit := ""
			if errors.As(err, &at) {
				edit = at.field
			}
			err = &choiceAt{ChoiceError: choice, field: field(choice, edit)}
		}
		return h.rpcError(ctx, err)
	}
	edits, err := h.edits(ctx, in)
	if err != nil {
		return nil, fail(err)
	}
	for i := range edits {
		// A method that read the spec says where it read it; an edit can say so itself.
		if also.Base != 0 {
			edits[i].Base = also.Base
		}
		edits[i].Pull, edits[i].Run = also.Pull, also.Run
		edits[i].MustCreate = also.MustCreate[edits[i].Ref]
		if edits[i].Ref.Kind == compose.TemplateKind && edits[i].Base == 0 {
			if edits[i].Spec == nil {
				return nil, authapi.Invalid(errors.New("name the current revision to delete a Compose template"))
			}
			edits[i].MustCreate = true
		}
		a := also.Adopts[edits[i].Ref]
		edits[i].Adopt, edits[i].AdoptName = a.ID, a.Name
		edits[i].Public = also.Public[edits[i].Ref]
		edits[i].SetsArchitectures = also.SetsArchitectures[edits[i].Ref]
		edits[i].Backup = slices.Contains(also.BackUp, edits[i].Ref)
		edits[i].Unholds = slices.Contains(also.Unholds, edits[i].Ref)
		edits[i].SetsUp = slices.Contains(also.SetsUp, edits[i].Ref)
		edits[i].Owner = slices.Contains(also.Owners, edits[i].Ref)
		if chosen := also.IDs[edits[i].Ref]; chosen != "" {
			edits[i].ID = chosen
		}
		if source, ok := also.Restores[edits[i].Ref]; ok {
			edits[i].Restore = &source
		}
	}
	// A route that moves a catalog app's address moves what follows it, in the same
	// change, and a service's value asked for as its entry writes it is rendered
	// (ADR 0103).
	if h.Address != nil && slices.ContainsFunc(edits, func(e revision.Edit) bool { return e.Ref.Kind == route.Kind || e.Ref.Kind == service.Kind }) {
		var notes []revision.Note
		if edits, notes, err = h.Address(ctx, edits); err != nil {
			// A refusal in its own words reaches the caller as it is.
			if cerr := new(connect.Error); errors.As(err, &cerr) {
				return nil, cerr
			}
			return nil, fail(err)
		}
		also.Notes = append(slices.Clip(also.Notes), notes...)
	}
	plan, err := h.engine.Propose(ctx, revision.Proposal{
		Summary: summary, Actor: who.Actor, Via: who.Via, Edits: slices.Concat(also.Edits, edits), Notes: also.Notes, IfNothing: also.IfNothing, Intent: also.Intent,
		SkipBackup: also.SkipBackup, BackUp: also.BackUp, BackUpOne: also.BackUpOne, Empties: also.Empties, Records: also.Records,
	})
	if err != nil {
		return nil, fail(err)
	}
	return h.change(ctx, plan.Change)
}

// Planned plans a change with plan, for another method that plans one its
// own way, such as RollBackService, and returns it for the schema.
func (h *Handler) Planned(ctx context.Context, plan func(who revision.By) (*revision.Plan, error)) (*dockwellv1.Change, error) {
	who, _, err := by(ctx)
	if err != nil {
		return nil, err
	}
	p, err := plan(who)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return h.change(ctx, p.Change)
}

// Error is err as the API says it, as ChangeService's own methods do.
func (h *Handler) Error(ctx context.Context, err error) error { return h.rpcError(ctx, err) }

// Protos are records for the schema, with their actors named in one read.
func (h *Handler) Protos(ctx context.Context, records ...*revision.Record) ([]*dockwellv1.Change, error) {
	return h.protos(ctx, records...)
}

// Environment finds the environment a request names by project and
// environment, each by name or ID, or left out where there is one. A
// choice it leaves out names the request's fields project and
// environment.
func (h *Handler) Environment(ctx context.Context, project, environment string) (scope.Project, scope.Environment, error) {
	return h.environment(ctx, project, environment, false)
}

// ReachedEnvironment finds an environment as Environment does, among the projects
// the caller holds a role in only, for a request that names a second place the
// API's first check doesn't read, such as where an app moves: a project elsewhere
// reads as one that isn't there, and no choice names one.
func (h *Handler) ReachedEnvironment(ctx context.Context, project, environment string) (scope.Project, scope.Environment, error) {
	return h.environment(ctx, project, environment, true)
}

func (h *Handler) environment(ctx context.Context, project, environment string, reached bool) (scope.Project, scope.Environment, error) {
	var p scope.Project
	var e scope.Environment
	err := h.db.View(ctx, func(tx *store.Tx) error {
		tree, err := scope.Load(ctx, tx)
		if err != nil {
			return err
		}
		if reached {
			tree = place.Reachable(ctx, tree)
		}
		p, e, err = tree.Resolve(project, environment)
		return err
	})
	var choice *scope.ChoiceError
	if errors.As(err, &choice) {
		field := "environment"
		if choice.Level == scope.ProjectKind {
			field = "project"
		}
		err = &choiceAt{ChoiceError: choice, field: field}
	}
	if err != nil {
		return p, e, h.rpcError(ctx, err)
	}
	return p, e, nil
}

// GetChange returns one change.
func (h *Handler) GetChange(ctx context.Context, req *connect.Request[dockwellv1.GetChangeRequest]) (*connect.Response[dockwellv1.GetChangeResponse], error) {
	c, err := h.change(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.GetChangeResponse{Change: c}), nil
}

// ListChanges returns a page of changes, newest first.
func (h *Handler) ListChanges(ctx context.Context, req *connect.Request[dockwellv1.ListChangesRequest]) (*connect.Response[dockwellv1.ListChangesResponse], error) {
	size, err := paging.Size(req.Msg.GetPageSize(), DefaultPageSize, MaxPageSize)
	if err != nil {
		return nil, err
	}
	after, _, err := paging.Read(req.Msg.GetPageToken(), "ListChanges")
	if err != nil {
		return nil, err
	}
	// One more than asked for says whether there is a next page.
	records, err := h.engine.Changes(ctx, store.ChangePage{CreatedAt: after.At, ID: after.ID}, size+1)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	res := &dockwellv1.ListChangesResponse{}
	if len(records) > size {
		records = records[:size]
		last := records[size-1]
		res.NextPageToken = paging.Token(paging.Cursor{At: last.CreatedAt, ID: last.ID})
	}
	if res.Changes, err = h.protos(ctx, records...); err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(res), nil
}

// ApplyChange starts applying a planned change, and returns it applying.
func (h *Handler) ApplyChange(ctx context.Context, req *connect.Request[dockwellv1.ApplyChangeRequest]) (*connect.Response[dockwellv1.ApplyChangeResponse], error) {
	who, p, err := by(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.engine.Change(ctx, req.Msg.GetId())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	fp := req.Msg.GetFingerprint()
	pushPermission := r.State == store.ChangePlanned && r.Plan.ChangesRegistryPushPermission()
	if pushPermission && (p.Token != nil || p.Session.ID == "") {
		return nil, authapi.ReasonError(connect.CodePermissionDenied, errors.New("sign in to change a registry login's push permission"), authapi.ReasonNeedsSession)
	}
	if r.State == store.ChangePlanned && r.Plan.HasDockerImageAction() && (p.Token != nil || p.Session.ID == "") {
		return nil, authapi.ReasonError(connect.CodePermissionDenied, errors.New("sign in with an install admin account to apply this image action"), authapi.ReasonNeedsSession)
	}
	// What the caller's role refuses is said first, before anything is asked of
	// them; applying checks it again (ADR 0048, check 2).
	var approve revision.Approval
	if r.State == store.ChangePlanned {
		if err := h.engine.CheckApply(ctx, r.ID); err != nil {
			return nil, h.rpcError(ctx, err)
		}
		// Someone else's change in a protected environment is approved: by an
		// admin or the owner, in a session, with the fingerprint of the plan they
		// reviewed and a passkey bound to it (ADR 0049).
		if approve, err = h.engine.ApprovalOf(ctx, r.ID); err != nil {
			return nil, h.rpcError(ctx, err)
		}
	}
	switch {
	case approve.Protected && approve.Theirs && p.Token != nil:
		return nil, authapi.ReasonError(connect.CodePermissionDenied,
			fmt.Errorf("%s is someone else's change in a protected environment, which an admin or the owner approves with a passkey, so no API token may apply it. %s", r.ID, signIn),
			authapi.ReasonNeedsSession)
	case approve.Protected && approve.Theirs && fp == "":
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s is someone else's change in a protected environment. Review the plan, then apply it with its fingerprint, %s, to approve it", r.ID, r.Fingerprint),
			"needs_confirmation")
	case fp != "" && fp != r.Fingerprint:
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s's plan has fingerprint %s, not %s, so it isn't the plan you reviewed. Review it again, then apply it", r.ID, r.Fingerprint, fp),
			"plan_changed")
	case pushPermission && fp == "":
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New("review the registry permission change, then apply it with its plan fingerprint"), "needs_confirmation")
	case r.State == store.ChangePlanned && r.Plan.HasDockerImageAction() && fp == "":
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			errors.New("review the image action, then apply it with its plan fingerprint"), "needs_confirmation")
	case r.State == store.ChangePlanned && r.Plan.Destructive() && p.Token != nil:
		return nil, authapi.ReasonError(connect.CodePermissionDenied,
			fmt.Errorf("%s destroys data, may cut off access to a server, or lets two containers write to one place, so no API token may apply it. %s", r.ID, signIn),
			authapi.ReasonNeedsSession)
	case r.State == store.ChangePlanned && r.Plan.HostAccess() && p.Token != nil:
		return nil, authapi.ReasonError(connect.CodePermissionDenied,
			fmt.Errorf("%s %s, which a person approves, so no API token may apply it. %s", r.ID, accessWords(r.Plan), signIn),
			authapi.ReasonNeedsSession)
	case r.State == store.ChangePlanned && h.engine.TouchesAccess(r.Plan) && p.Token != nil:
		return nil, authapi.ReasonError(connect.CodePermissionDenied,
			fmt.Errorf("%s changes who reaches a server, or who gets onto it, which a person approves with a passkey, so no API token may apply it. %s", r.ID, signIn),
			authapi.ReasonNeedsSession)
	case r.State == store.ChangePlanned && r.Plan.Destructive() && fp == "":
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s destroys data, may cut off access to a server, or lets two containers write to one place, and undo may not bring everything back. Review the plan, then apply it with its fingerprint, %s, to confirm", r.ID, r.Fingerprint),
			"needs_confirmation")
	case r.State == store.ChangePlanned && r.Plan.HostAccess() && fp == "":
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s %s. Review the plan, then apply it with its fingerprint, %s, to confirm", r.ID, accessWords(r.Plan), r.Fingerprint),
			"needs_confirmation")
	}
	// The proof is asked for once nothing else here refuses, and held until the
	// engine takes the change, as it can still refuse: busy, offline or out of date.
	giveBack := func() {}
	access := r.State == store.ChangePlanned && h.engine.TouchesAccess(r.Plan)
	client, _ := authapi.ClientFrom(ctx)
	switch {
	case r.State == store.ChangePlanned && (pushPermission || r.Plan.HasDockerMaintenance() || slices.ContainsFunc(r.Plan.Resources, func(p revision.ResourcePlan) bool { return p.DockerDaemon != nil })):
		target := r.ID + ":" + r.Fingerprint
		if req.Msg.GetProof() == "" {
			h.auth.AskedForStepUp(p, auth.StepUpApproveChange, target, func() { h.noteUnconfirmed(r.ID, who) })
			return nil, stepUpNeeded(auth.ErrNeedsStepUp, dockwellv1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, target)
		}
		back, err := h.auth.HoldStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpApproveChange, target)
		if err != nil {
			return nil, stepUpNeeded(err, dockwellv1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, target)
		}
		giveBack = back
	case r.State == store.ChangePlanned && slices.ContainsFunc(r.Plan.Resources, func(r revision.ResourcePlan) bool { return r.Ref.Kind == "ssh.config" }):
		target := r.ID + ":" + r.Fingerprint
		if req.Msg.GetProof() == "" {
			return nil, stepUpNeeded(auth.ErrNeedsStepUp, dockwellv1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, target)
		}
		back, err := h.auth.HoldStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpApproveChange, target)
		if err != nil {
			return nil, stepUpNeeded(err, dockwellv1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, target)
		}
		giveBack = back
	case approve.Protected && approve.Theirs:
		// One passkey approves the plan, root's included: it's bound to its fingerprint.
		target := r.ID + ":" + r.Fingerprint
		back, err := h.auth.HoldStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpApproveChange, target)
		if err != nil {
			h.auth.AskedForStepUp(p, auth.StepUpApproveChange, target, func() { h.noteUnconfirmed(r.ID, who) })
			return nil, stepUpNeeded(err, dockwellv1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, target)
		}
		giveBack = back
	case r.State == store.ChangePlanned && (r.Plan.AsGoodAsRoot() || access || r.Plan.SendsKeys() || r.Plan.Removes != nil || r.Plan.Unholds):
		// With no proof, the session's cover may stand in (ADR 0075).
		back, err := h.auth.HoldStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpApplyRootChange, r.ID)
		if err != nil && !errors.Is(err, auth.ErrNeedsStepUp) && !errors.Is(err, auth.ErrNeedsSession) {
			return nil, authapi.StepUpRefused(err)
		}
		if err != nil {
			h.auth.AskedForStepUp(p, auth.StepUpApplyRootChange, r.ID, func() { h.noteUnconfirmed(r.ID, who) })
			return nil, stepUpNeeded(err, dockwellv1.StepUpAction_STEP_UP_ACTION_APPLY_ROOT_CHANGE, r.ID)
		}
		giveBack = back
	}
	if access {
		h.remember(ctx, r.ID, p.Session.ID)
	}
	pairs := r.Plan.KeyPairs()
	if err := h.engine.Start(ctx, r.ID, who); err != nil {
		giveBack()
		if access {
			h.forget(r.ID)
		}
		return nil, h.rpcError(ctx, err)
	}
	// The admin's passkey let these servers have these certificates' keys by DNS
	// (ADR 0088), which the control plane sends them as it has them, once the
	// engine has taken the change.
	if r.State == store.ChangePlanned && len(pairs) > 0 {
		if err := h.db.Update(context.WithoutCancel(ctx), func(tx *store.Tx) error { return revision.ConfirmKeys(ctx, tx, pairs) }); err != nil {
			h.logger.ErrorContext(ctx, "can't keep which servers may have which certificates' keys; apply the change's plan again to send them", slog.String("change", r.ID), slog.String("error", err.Error()))
		}
	}
	c, err := h.change(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.ApplyChangeResponse{Change: c}), nil
}

// stepUpNeeded refuses for want of a step-up, saying which, so a client asks
// for that one: approving a change, or applying one as good as root.
func stepUpNeeded(err error, action dockwellv1.StepUpAction, target string) error {
	cerr := authapi.ReasonError(connect.CodePermissionDenied, err, authapi.ReasonNeedsStepUp)
	if d, derr := connect.NewErrorDetail(&dockwellv1.StepUpNeeded{Action: action, Target: target}); derr == nil {
		cerr.AddDetail(d)
	}
	return cerr
}

// ConfirmChange keeps a change to who reaches a server that waits for its
// confirmation there, for the person who applied it, in that session.
func (h *Handler) ConfirmChange(ctx context.Context, req *connect.Request[dockwellv1.ConfirmChangeRequest]) (*connect.Response[dockwellv1.ConfirmChangeResponse], error) {
	_, p, err := by(ctx)
	if err != nil {
		return nil, err
	}
	changeID, server := req.Msg.GetId(), req.Msg.GetServerId()
	if !h.AppliedFrom(changeID, p) {
		return nil, authapi.ReasonError(connect.CodePermissionDenied,
			fmt.Errorf("only the person who applied %s may confirm it, in the session they applied it from, as they check they can still reach the server", changeID), "not_yours")
	}
	if server == "" {
		return nil, authapi.Invalid(errors.New("a confirmation names its server, server_id, as a change to several servers waits on each in turn. Take it from the change's confirmation"))
	}
	r, err := h.engine.Change(ctx, changeID)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if r.State != store.ChangeApplying {
		// It's over, so nothing of it waits on any of its servers now.
		h.forget(changeID)
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s is %s, so there's nothing of it to confirm", changeID, r.State), "not_waiting")
	}
	var deadline time.Time
	if d := req.Msg.GetDeadline(); d != nil {
		deadline = d.AsTime()
	}
	verdict, err := h.engine.ConfirmAccess(ctx, changeID, server, deadline)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if err := verdictError(changeID, verdict); err != nil {
		return nil, err
	}
	c, err := h.change(ctx, changeID)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.ConfirmChangeResponse{Change: c}), nil
}

// AppliedFrom reports whether p is in the session that applied this change.
func (h *Handler) AppliedFrom(changeID string, p auth.Principal) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	session, known := h.appliedIn[changeID]
	return p.Token == nil && known && session == p.Session.ID
}

// verdictError is what a confirmation answers for what the server did with it:
// nothing when it kept the change.
func verdictError(changeID string, v resource.AccessVerdict) error {
	switch v {
	case resource.AccessKept:
		// A change to several servers waits on each in turn, so the session stays
		// the one to confirm it until it's over.
		return nil
	case resource.AccessNotProved:
		return authapi.ReasonError(connect.CodeFailedPrecondition, errors.New("open a fresh SSH connection on the new settings before keeping this change"), "ssh_not_proved")
	case resource.AccessRolledBack:
		return authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("the server's time for %s ran out before the confirmation reached it, so it put back its firewall as it was, and the change rolls back", changeID), "rolled_back")
	case resource.AccessNotKept:
		return authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("the server couldn't keep %s yet, as it couldn't write what puts its firewall back if the agent stops, so it still waits. Check the server's disk has room, then keep it again", changeID), "not_kept")
	case resource.AccessNotWaiting:
	}
	return authapi.ReasonError(connect.CodeFailedPrecondition,
		fmt.Errorf("nothing of %s waits for that confirmation on that server now. Watch the change to see what it waits for, or how it ended", changeID), "not_waiting")
}

// remember keeps the session a change to who reaches a server was applied
// from, and forgets those of changes that are over. It lives in memory: a
// control plane that starts again during a wait has nobody who may confirm
// it, and the server's timer puts back its firewall.
func (h *Handler) remember(ctx context.Context, changeID, session string) {
	h.mu.Lock()
	ids := slices.Collect(maps.Keys(h.appliedIn))
	h.appliedIn[changeID] = session
	h.mu.Unlock()
	for _, id := range ids {
		if id == changeID {
			continue
		}
		if r, err := h.engine.Change(ctx, id); err == nil && r.State != store.ChangeApplying {
			h.forget(id)
		}
	}
}

func (h *Handler) forget(changeID string) {
	h.mu.Lock()
	delete(h.appliedIn, changeID)
	h.mu.Unlock()
}

// noteUnconfirmed puts on record a plan as good as root that someone tried to apply,
// and no passkey followed. Stopping there changed nothing, and it's what a stolen
// session looks like.
func (h *Handler) noteUnconfirmed(changeID string, who revision.By) {
	ctx := context.Background()
	if err := h.db.Update(ctx, func(tx *store.Tx) error {
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: time.Now(), Actor: who.Actor, Action: "change.apply_unconfirmed", Subject: changeID, Target: changeID, Surface: who.Via,
			Detail: "asked for a passkey to apply " + changeID + ", and none came",
		})
	}); err != nil {
		h.logger.WarnContext(ctx, "can't record a plan that wasn't confirmed", slog.String("change", changeID), slog.String("error", err.Error()))
	}
}

// signIn says where a person applies what no token may. The CLI signs in
// with an API token, even after dockwell login, so the panel is the place.
const signIn = "Sign in to the panel and apply it there: the CLI uses an API token, even after dockwell login, so it can't apply it either"

// accessWords says what about a plan a person approves, each as what it is, such
// as "gives web access to its server: the server path /srv/app (read-only), and
// Docker's socket at /var/run/docker.sock, which controls Docker as root does". A
// domain isn't access to a server, and the panel's moving isn't given to anything.
func accessWords(p *revision.Plan) string {
	var parts []string
	for i := range p.Resources {
		r := &p.Resources[i]
		var reach []string
		for j := range r.HostAccess {
			a := r.HostAccess[j]
			switch a.Kind {
			case resource.AccessDomain:
				parts = append(parts, "claims "+resource.AccessWords(a))
			case resource.AccessPanelDomain:
				parts = append(parts, "moves the panel's domain, "+a.Path+", behind "+a.Detail)
			case resource.AccessTrustedProxy:
				parts = append(parts, "has "+r.Ref.Name+" take "+resource.AccessWords(a))
			case resource.AccessPlainForAnyone:
				parts = append(parts, "has "+r.Ref.Name+" serve "+resource.AccessWords(a))
			case resource.AccessHTTPSOff:
				parts = append(parts, "takes away "+resource.AccessWords(a))
			case resource.AccessOpened:
				parts = append(parts, "opens "+resource.AccessWords(a))
			default:
				reach = append(reach, resource.AccessWords(a))
			}
		}
		if len(reach) > 0 {
			parts = append(parts, "gives "+r.Ref.Name+" access to its server: "+human.List(reach))
		}
	}
	return strings.Join(parts, "; ")
}

// WatchChange streams a change until it is applied, failed or discarded.
func (h *Handler) WatchChange(ctx context.Context, req *connect.Request[dockwellv1.WatchChangeRequest], stream *connect.ServerStream[dockwellv1.WatchChangeResponse]) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Watch before reading, so nothing that happens in between is missed.
	updates := h.engine.Watch(ctx, req.Msg.GetId())
	c, err := h.change(ctx, req.Msg.GetId())
	if err != nil {
		return err
	}
	if err := stream.Send(&dockwellv1.WatchChangeResponse{Event: &dockwellv1.WatchChangeResponse_Change{Change: c}}); err != nil {
		return err
	}
	if final(c.GetState()) {
		return nil
	}
	partial := c.GetElsewhere() != nil
	for {
		select {
		case <-ctx.Done():
			return nil
		case u, ok := <-updates:
			if !ok {
				if ctx.Err() != nil {
					return nil
				}
				return authapi.ReasonError(connect.CodeUnavailable, errors.New("this watch fell behind the change, so Dockwell ended it. Watch it again"), "watch_behind")
			}
			msg := &dockwellv1.WatchChangeResponse{}
			switch {
			case u.Step != nil && partial:
				// A step's index is in the whole change, which this reader sees part of:
				// they get the change as they see it instead.
				if c, err = h.change(ctx, req.Msg.GetId()); err != nil {
					return err
				}
				msg.Event = &dockwellv1.WatchChangeResponse_Change{Change: c}
			case u.Step != nil:
				msg.Event = &dockwellv1.WatchChangeResponse_Step{Step: &dockwellv1.StepUpdate{Index: int32(u.Index), Step: stepProto(u.Step)}} //nolint:gosec // a change's steps number far fewer than 2³¹
			default:
				// The change as the event left it: reading it now may find it
				// further on, and the events after this one say how.
				if c, err = h.change(ctx, req.Msg.GetId()); err != nil {
					return err
				}
				c.State = states[u.State]
				if u.Progress != nil {
					c.Steps = c.Steps[:0]
					for i := range u.Progress.Steps {
						c.Steps = append(c.Steps, stepProto(&u.Progress.Steps[i]))
					}
					c.Confirmation = confirmationOf(c.Steps)
					if partial {
						if err := h.db.View(ctx, func(tx *store.Tx) error { return Narrow(ctx, tx, c) }); err != nil {
							return err
						}
					}
				}
				msg.Event = &dockwellv1.WatchChangeResponse_Change{Change: c}
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
			if u.Step == nil && final(states[u.State]) {
				return nil
			}
		}
	}
}

func final(s dockwellv1.ChangeState) bool {
	return s == dockwellv1.ChangeState_CHANGE_STATE_APPLIED || s == dockwellv1.ChangeState_CHANGE_STATE_FAILED ||
		s == dockwellv1.ChangeState_CHANGE_STATE_DISCARDED
}

// UndoChange plans undoing an applied change.
func (h *Handler) UndoChange(ctx context.Context, req *connect.Request[dockwellv1.UndoChangeRequest]) (*connect.Response[dockwellv1.UndoChangeResponse], error) {
	who, _, err := by(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := h.engine.Undo(ctx, req.Msg.GetId(), who)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	c, err := h.change(ctx, plan.Change)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.UndoChangeResponse{Change: c}), nil
}

// DiscardChange drops a planned change.
func (h *Handler) DiscardChange(ctx context.Context, req *connect.Request[dockwellv1.DiscardChangeRequest]) (*connect.Response[dockwellv1.DiscardChangeResponse], error) {
	who, _, err := by(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.engine.Discard(ctx, req.Msg.GetId(), who); err != nil {
		return nil, h.rpcError(ctx, err)
	}
	c, err := h.change(ctx, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	if h.DiscardDockerArchives != nil {
		h.DiscardDockerArchives(ctx, c)
	}
	return connect.NewResponse(&dockwellv1.DiscardChangeResponse{Change: c}), nil
}

// RestartService plans restarting a service or a managed Compose stack.
func (h *Handler) RestartService(ctx context.Context, req *connect.Request[dockwellv1.RestartServiceRequest]) (*connect.Response[dockwellv1.RestartServiceResponse], error) {
	who, _, err := by(ctx)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	if m.GetId() == "" {
		return nil, authapi.Invalid(errors.New("name the service to restart, by its ID or its name, such as web"))
	}
	if strings.HasPrefix(m.GetId(), id.Environment+"_") {
		if m.GetContainerId() != "" {
			return nil, authapi.Invalid(errors.New("a stack restart cannot target one container"))
		}
		project, env, err := h.Environment(ctx, "", m.GetId())
		if err != nil {
			return nil, err
		}
		summary := strings.TrimSpace(m.GetSummary())
		if summary == "" {
			summary = "Restart stack " + cmp.Or(project.DisplayName, project.Name)
		}
		plan, err := h.engine.RestartEnvironment(ctx, env.ID, m.GetServerId(), who, summary)
		if err != nil {
			return nil, h.rpcError(ctx, err)
		}
		change, err := h.change(ctx, plan.Change)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&dockwellv1.RestartServiceResponse{Change: change}), nil
	}
	var ref resource.Ref
	exact := m.GetServerId() != "" || m.GetContainerId() != ""
	if strings.HasPrefix(m.GetId(), id.Service+"_") || exact && strings.HasPrefix(m.GetId(), id.Resource+"_") {
		err = h.db.View(ctx, func(tx *store.Tx) error {
			r, err := tx.GetResourceByID(ctx, m.GetId())
			if errors.Is(err, store.ErrNotFound) || err == nil && (r.Spec == nil || r.Kind != string(service.Kind) && (!exact || r.Kind != "container")) {
				return fault.New(scope.ErrNotFound, "there's no service %s. Check its ID, or list the resources to find it", m.GetId())
			}
			if err != nil {
				return err
			}
			ref = resource.Ref{Kind: resource.Kind(r.Kind), Scope: r.Scope, Name: r.Name}
			return nil
		})
		if err != nil {
			return nil, h.rpcError(ctx, err)
		}
	} else {
		_, e, err := h.Environment(ctx, m.GetProject(), m.GetEnvironment())
		if err != nil {
			return nil, err
		}
		ref = resource.Ref{Kind: service.Kind, Scope: e.ID, Name: m.GetId()}
	}
	summary := strings.TrimSpace(m.GetSummary())
	var plan *revision.Plan
	if exact {
		plan, err = h.engine.RestartInstance(ctx, ref, who, summary, revision.RestartTarget{Server: m.GetServerId(), Container: m.GetContainerId()})
	} else {
		plan, err = h.engine.Restart(ctx, ref, who, summary)
	}
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	c, err := h.change(ctx, plan.Change)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.RestartServiceResponse{Change: c}), nil
}

// ListDrift checks servers for drift.
func (h *Handler) ListDrift(ctx context.Context, req *connect.Request[dockwellv1.ListDriftRequest]) (*connect.Response[dockwellv1.ListDriftResponse], error) {
	var servers []string
	names := map[string]string{}
	err := h.db.View(ctx, func(tx *store.Tx) error {
		if want := req.Msg.GetServerId(); want != "" {
			s, err := tx.GetServer(ctx, want)
			if errors.Is(err, store.ErrNotFound) || err == nil && s.RevokedAt != nil {
				return fault.New(scope.ErrNotFound, "there's no server %s in the fleet", want)
			}
			servers, names[want] = []string{want}, s.Name
			return err
		}
		all, err := tx.ListServers(ctx)
		for i := range all {
			servers = append(servers, all[i].ID)
			names[all[i].ID] = all[i].Name
		}
		return err
	})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	results, err := h.drift(ctx, servers)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	res := &dockwellv1.ListDriftResponse{}
	for i, s := range servers {
		if err := results[i].err; err != nil {
			reason, _ := serverReason(err)
			h.logUnreadable(ctx, err)
			res.UncheckedServers = append(res.UncheckedServers, &dockwellv1.UncheckedServer{
				ServerId: s, ServerName: names[s], Reason: reason, Message: err.Error(),
			})
		}
		for j := range results[i].drift {
			d := &results[i].drift[j]
			res.Drift = append(res.Drift, &dockwellv1.Drift{
				ServerId: s, ResourceId: d.ID, Resource: resourcepb.Ref(d.Ref), Revision: int64(d.Revision), Missing: d.Missing,
				Changes: resourcepb.Changes(d.Changes),
			})
		}
	}
	return connect.NewResponse(res), nil
}

// DriftChecks is how many servers ListDrift checks at once. Each agent may
// take up to a minute to answer, so one at a time would keep a fleet's
// request waiting for minutes.
const DriftChecks = 16

// checked is one server's drift, or why it couldn't be checked.
type checked struct {
	drift []revision.Drift
	err   error
}

// drift checks each server for drift, DriftChecks at a time, and returns
// what each has, in the servers' order. A server that can't be checked
// leaves the others to report; the request failing, as when its client
// goes away, or the store failing, fails it all, and stops the checks
// still under way.
func (h *Handler) drift(ctx context.Context, servers []string) ([]checked, error) {
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make([]checked, len(servers))
	var fatal error
	var once sync.Once
	next := make(chan int)
	var wg sync.WaitGroup
	for range min(DriftChecks, len(servers)) {
		wg.Go(func() {
			for i := range next {
				c := &out[i]
				c.drift, c.err = h.engine.Drift(ctx, servers[i])
				if _, theServers := serverReason(c.err); c.err != nil && !theServers {
					once.Do(func() { fatal = c.err; cancel() })
				}
			}
		})
	}
feed:
	for i := range servers {
		select {
		case next <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(next)
	wg.Wait()
	if err := parent.Err(); err != nil {
		return nil, err
	}
	return out, fatal
}

// RevertDrift plans putting a drifted resource back.
func (h *Handler) RevertDrift(ctx context.Context, req *connect.Request[dockwellv1.RevertDriftRequest]) (*connect.Response[dockwellv1.RevertDriftResponse], error) {
	c, err := h.onDrift(ctx, req.Msg.GetServerId(), req.Msg.GetResourceId(), h.engine.Revert)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.RevertDriftResponse{Change: c}), nil
}

// AdoptDrift plans keeping what a server has.
func (h *Handler) AdoptDrift(ctx context.Context, req *connect.Request[dockwellv1.AdoptDriftRequest]) (*connect.Response[dockwellv1.AdoptDriftResponse], error) {
	c, err := h.onDrift(ctx, req.Msg.GetServerId(), req.Msg.GetResourceId(), h.engine.Adopt)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.AdoptDriftResponse{Change: c}), nil
}

// onDrift checks a server for a resource's drift now, rather than trusting
// what the caller saw, and plans what to do about it.
func (h *Handler) onDrift(ctx context.Context, server, resourceID string,
	plan func(context.Context, revision.Drift, revision.By) (*revision.Plan, error),
) (*dockwellv1.Change, error) {
	who, _, err := by(ctx)
	if err != nil {
		return nil, err
	}
	if server == "" || resourceID == "" {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument, errors.New("name the server and the resource, by their IDs, as ListDrift gives them"), "invalid")
	}
	drift, err := h.engine.Drift(ctx, server)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	i := slices.IndexFunc(drift, func(d revision.Drift) bool { return d.ID == resourceID })
	if i < 0 {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s on %s matches its latest revision now, so there's no drift to act on", resourceID, server), "no_drift")
	}
	p, err := plan(ctx, drift[i], who)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return h.change(ctx, p.Change)
}

// change reads a change for the schema.
func (h *Handler) change(ctx context.Context, changeID string) (*dockwellv1.Change, error) {
	r, err := h.engine.Change(ctx, changeID)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	cs, err := h.protos(ctx, r)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return cs[0], nil
}

// edits reads a request's edits, filling in the kinds and scopes it leaves
// out (PRD principle 4).
func (h *Handler) edits(ctx context.Context, in []*dockwellv1.Edit) ([]revision.Edit, error) {
	if len(in) == 0 {
		return nil, fault.New(revision.ErrInvalid, "the change has no edits. Add one for each resource to change")
	}
	kinds := h.engine.Kinds()
	out := make([]revision.Edit, 0, len(in))
	err := h.db.View(ctx, func(tx *store.Tx) error {
		var tree *scope.Tree
		for i, e := range in {
			ref := resourcepb.FromRef(e.GetResource())
			var spec proto.Message
			if s := e.GetSpec(); s != nil {
				m := s.ProtoReflect()
				if fd := m.WhichOneof(m.Descriptor().Oneofs().ByName("kind")); fd != nil {
					spec = m.Get(fd).Message().Interface()
				}
			}
			switch {
			case e.GetDelete() && spec != nil:
				return fault.New(revision.ErrInvalid, "edit %d both sets a spec and deletes. Do one or the other", i+1)
			case !e.GetDelete() && spec == nil:
				return fault.New(revision.ErrInvalid, "edit %d has no spec. Set one, or set delete to delete the resource", i+1)
			}
			if spec != nil {
				kind, ok := kinds.KindOf(spec)
				switch {
				case !ok:
					return fault.New(revision.ErrInvalid, "edit %d's spec is of no kind Dockwell manages. Set one of the spec's kinds, such as service", i+1)
				case ref.Kind == "":
					ref.Kind = kind
				case ref.Kind != kind:
					return fault.New(revision.ErrInvalid, "edit %d is for a %s, but its spec is a %s. Make the two agree, or leave the kind out", i+1, ref.Kind, kind)
				}
			}
			t, err := kinds.Type(ref.Kind)
			if err != nil {
				if ref.Kind == "" {
					return fault.New(revision.ErrInvalid, "edit %d needs a kind, such as service", i+1)
				}
				return fault.New(revision.ErrInvalid, "%w", err)
			}
			if e.GetProject() != "" && t.Level() != resource.LevelEnvironment {
				return fault.New(revision.ErrInvalid, "edit %d names a project, which only a resource that lives in an environment, such as a service, takes. "+
					"Name what a %s lives in with resource.scope instead", i+1, ref.Kind)
			}
			if tree == nil && t.Level() != resource.LevelServer {
				loaded, err := scope.Load(ctx, tx)
				if err != nil {
					return err
				}
				loaded.Projects = inReach(ctx, loaded.Projects)
				tree = &loaded
			}
			if ref.Scope, err = resolve(ctx, tx, tree, t.Level(), e.GetProject(), ref.Scope); err != nil {
				var choice *scope.ChoiceError
				if errors.As(err, &choice) {
					// Which field takes the choice: an environment's project
					// is named apart, and every other choice is the scope.
					field := "resource.scope"
					if t.Level() == resource.LevelEnvironment && choice.Level == scope.ProjectKind {
						field = "project"
					}
					return &choiceAt{ChoiceError: choice, field: fmt.Sprintf("edits[%d].%s", i, field)}
				}
				return err
			}
			if state := runIntentOnly(spec); state != 0 {
				if e.GetBaseRevision() <= 0 || len(e.GetServers()) != 0 || len(e.GetKeepEnv()) != 0 ||
					len(e.GetEnvToSecrets()) != 0 || e.GetWithContents() || e.GetKeepSharedIn() != "" || e.GetDeleteShared() {
					return fault.New(revision.ErrInvalid, "a run-state edit needs its base revision and no other changes")
				}
				spec, err = h.engine.SpecWithRunIntent(ctx, tx, ref, int(e.GetBaseRevision()), state)
				if err != nil {
					return err
				}
				out = append(out, revision.Edit{Ref: ref, Spec: spec, Base: int(e.GetBaseRevision())})
				continue
			}
			if spec, err = withNewAppDefaults(ctx, tx, ref, spec); err != nil {
				return err
			}
			if spec, err = withInstallZone(ctx, tx, spec); err != nil {
				return err
			}
			if spec, err = h.withGitLogin(ctx, tx, ref, spec); err != nil {
				return err
			}
			// None keeps a service where it runs.
			var servers []string
			if servers, err = onServers(ctx, tx, e.GetServers()); err != nil {
				return err
			}
			switch {
			case e.GetBaseRevision() < 0:
				return fault.New(revision.ErrInvalid, "edit %d's base_revision is %d. Pass the revision the spec was read at, or 0 to check nothing", i+1, e.GetBaseRevision())
			case len(e.GetKeepEnv()) > 0 && spec == nil:
				return fault.New(revision.ErrInvalid, "edit %d deletes, and keeps environment variables too. Leave keep_env out", i+1)
			case len(e.GetKeepEnv()) > 0 && e.GetBaseRevision() == 0:
				// What it keeps comes from what's there now, and the rest from what it read: from
				// no revision, a change between the two would be undone with nothing to say so.
				return fault.New(revision.ErrInvalid, "edit %d keeps environment variables, but says no base_revision, so a change since the spec was read would be undone. "+
					"Pass the revision ListServices gave as base_revision", i+1)
			}
			out = append(out, revision.Edit{
				Ref: ref, Spec: spec, Servers: servers, Base: int(e.GetBaseRevision()), KeepEnv: e.GetKeepEnv(),
				EnvToSecrets: e.GetEnvToSecrets(), WithContents: e.GetWithContents(),
				KeepSharedIn: e.GetKeepSharedIn(), DeleteShared: e.GetDeleteShared(),
				MustCreate: e.GetMustCreate(), DockerStorageCreate: e.GetMustCreate(),
			})
		}
		return nil
	})
	return out, err
}

// runIntentOnly recognizes the narrow edit that changes run state without
// reading the rest of the workload's spec into the panel.
func runIntentOnly(spec proto.Message) resourcev1.DesiredRunState {
	state := resource.RunIntent(spec)
	if state == 0 {
		return 0
	}
	app := &resourcev1.Container{DesiredRunState: state}
	switch spec.(type) {
	case *resourcev1.Service:
		if proto.Equal(spec, &resourcev1.Service{Runs: &resourcev1.Service_App{App: app}}) {
			return state
		}
	case *resourcev1.Container:
		if proto.Equal(spec, app) {
			return state
		}
	}
	return 0
}

// withNewAppDefaults gives a service a change makes, with no automatic updates of
// its own, what the install's settings say new apps' start as (ADR 0080). One
// that's there already keeps its own, as does an app that builds or runs from a
// folder. A copy is changed, never the request.
func withNewAppDefaults(ctx context.Context, tx *store.Tx, ref resource.Ref, spec proto.Message) (proto.Message, error) {
	svc, ok := spec.(*resourcev1.Service)
	if !ok || svc.GetAutoUpdate() != nil || svc.GetBuild() != nil || svc.GetRuntime() != nil {
		return spec, nil
	}
	switch r, err := tx.GetResource(ctx, string(ref.Kind), ref.Scope, ref.Name); {
	case err == nil && r.Spec != nil:
		return spec, nil
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	a, err := settings.NewAppsAutoUpdate(ctx, tx)
	if err != nil || a == nil {
		return spec, err
	}
	svc = proto.CloneOf(svc)
	svc.AutoUpdate = a
	return svc, nil
}

// withGitLogin checks the git login of a service's build from Git, for the
// project of its environment, and gives one that names none the GitHub App's
// installation that reaches its repository there, if one does (ADR 0098). A copy
// is changed, never the request.
func (h *Handler) withGitLogin(ctx context.Context, tx *store.Tx, ref resource.Ref, spec proto.Message) (proto.Message, error) {
	svc, ok := spec.(*resourcev1.Service)
	g := svc.GetBuild().GetGit()
	if !ok || g == nil {
		return spec, nil
	}
	login := svc.GetBuild().GetGitLogin()
	if h.gitLogins == nil {
		if login != "" {
			return nil, fault.New(revision.ErrInvalid, "%s's build names the GitHub App installation %s, and this control plane has no GitHub App. Leave it out to fetch the repository as the public do", ref.Name, login)
		}
		return spec, nil
	}
	tree, err := scope.Load(ctx, tx)
	if err != nil {
		return nil, err
	}
	project := ""
	for _, p := range tree.Projects {
		for _, e := range p.Environments {
			if e.ID == ref.Scope {
				project = p.ID
			}
		}
	}
	pinned, err := h.gitLogins.PinGitLogin(ctx, tx, project, g.GetUrl(), login)
	if err != nil {
		return nil, fault.New(revision.ErrInvalid, "%s's build: %w", ref.Name, err)
	}
	if pinned == login {
		return spec, nil
	}
	svc = proto.CloneOf(svc)
	svc.Build.GitLogin = pinned
	return svc, nil
}

// withInstallZone gives a backup schedule that comes without a time zone the
// install's, from Settings, before it's planned, as Settings says new schedules
// start in it (ADR 0042). The kind's own default, UTC, is for an install that
// has none. A copy is changed, never the request.
func withInstallZone(ctx context.Context, tx *store.Tx, spec proto.Message) (proto.Message, error) {
	if t, ok := spec.(*resourcev1.Task); ok && t.GetTimeZone() == "" {
		// A scheduled task too (ADR 0053).
		zone, err := tx.GetSetting(ctx, store.SettingTimeZone)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return spec, nil
		case err != nil:
			return nil, fmt.Errorf("can't read the install's time zone for a scheduled task: %w", err)
		}
		t = proto.CloneOf(t)
		t.TimeZone = string(zone)
		return t, nil
	}
	if svc, ok := spec.(*resourcev1.Service); ok && svc.GetAutoUpdate() != nil && svc.GetAutoUpdate().GetTimeZone() == "" {
		// An automatic update's window too (ADR 0080).
		zone, err := tx.GetSetting(ctx, store.SettingTimeZone)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return spec, nil
		case err != nil:
			return nil, fmt.Errorf("can't read the install's time zone for automatic updates: %w", err)
		}
		svc = proto.CloneOf(svc)
		svc.AutoUpdate.TimeZone = string(zone)
		return svc, nil
	}
	p, ok := spec.(*resourcev1.BackupPolicy)
	if !ok || p.GetTimeZone() != "" {
		return spec, nil
	}
	zone, err := tx.GetSetting(ctx, store.SettingTimeZone)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return spec, nil
	case err != nil:
		return nil, fmt.Errorf("can't read the install's time zone for a backup schedule: %w", err)
	}
	p = proto.Clone(p).(*resourcev1.BackupPolicy) //nolint:forcetypeassert // a clone has its original's type
	p.TimeZone = string(zone)
	return p, nil
}

// inReach are the projects the caller in ctx holds a role in, below an admin,
// so an edit names a project or an environment by name among those alone: one
// out of reach reads as one that isn't there, and no answer lists another
// project or its environments (ADR 0049). An admin, the owner, and Dockwell's
// own changes find every project.
func inReach(ctx context.Context, projects []scope.Project) []scope.Project {
	caller, ok := place.CallerFrom(ctx)
	if !ok || store.RoleRank(place.Holds(caller.Grants, place.Install)) >= store.RoleRank(store.RoleAdmin) {
		return projects
	}
	return slices.DeleteFunc(projects, func(p scope.Project) bool {
		return place.Holds(caller.Grants, place.Place{Project: p.ID}) == ""
	})
}

// choiceAt is a choice a request left out, with the field of the request
// that takes it.
type choiceAt struct {
	*scope.ChoiceError
	field string
}

func (c *choiceAt) Unwrap() error { return c.ChoiceError }

// resolve finds the ID of what a resource lives in, from an ID, a name,
// or nothing, which means the only one there is. An environment's project
// is named apart, by name or ID, for when there is a choice of projects.
func resolve(ctx context.Context, tx *store.Tx, tree *scope.Tree, level resource.Level, project, given string) (string, error) {
	switch level {
	case resource.LevelOrganization:
		if given == "" || given == tree.Organization.ID || given == tree.Organization.Name {
			return tree.Organization.ID, nil
		}
		return "", fault.New(scope.ErrNotFound, "there's no organization %s", given)
	case resource.LevelProject:
		if strings.HasPrefix(given, id.Project+"_") {
			return given, nil // checked when the change is planned
		}
		p, err := tree.Project(given)
		return p.ID, err
	case resource.LevelEnvironment:
		if project == "" && strings.HasPrefix(given, id.Environment+"_") {
			return given, nil // checked when the change is planned
		}
		_, e, err := tree.Resolve(project, given)
		return e.ID, err
	case resource.LevelServer:
	}
	servers, err := tx.ListServers(ctx)
	if err != nil {
		return "", err
	}
	if given == "" {
		switch len(servers) {
		case 1:
			return servers[0].ID, nil
		case 0:
			return "", fault.New(scope.ErrNotFound, "there's no server yet. Add one first")
		}
		names := make([]string, len(servers))
		for i := range servers {
			names[i] = servers[i].Name
		}
		return "", &scope.ChoiceError{Level: scope.ServerLevel, Options: names}
	}
	for i := range servers {
		if servers[i].ID == given || servers[i].Name == given {
			return servers[i].ID, nil
		}
	}
	return "", noServerNamed(servers, given)
}

// onServers finds the servers an edit runs on, each named by its ID or by its
// name, as the CLI's --on and the hint that lists them name it (#841). An ID
// goes as it is, so planning says when its server was removed from the fleet.
// No two servers in the fleet share a name (servers_active_name), so a name
// is one server or none.
func onServers(ctx context.Context, tx *store.Tx, given []string) ([]string, error) {
	if len(given) == 0 {
		return nil, nil
	}
	var fleet []store.Server
	out := make([]string, 0, len(given))
	for _, g := range given {
		if strings.HasPrefix(g, id.Server+"_") {
			out = append(out, g)
			continue
		}
		if fleet == nil {
			var err error
			if fleet, err = tx.ListServers(ctx); err != nil {
				return nil, err
			}
		}
		i := slices.IndexFunc(fleet, func(s store.Server) bool { return s.Name == g })
		if i < 0 {
			return nil, noServerNamed(fleet, g)
		}
		out = append(out, fleet[i].ID)
	}
	return out, nil
}

// noServerNamed says there's no server by a name, with the names there are.
func noServerNamed(fleet []store.Server, given string) error {
	if len(fleet) == 0 {
		return fault.New(scope.ErrNotFound, "there's no server %s, as the fleet has none yet. Add one first", given)
	}
	names := make([]string, len(fleet))
	for i := range fleet {
		names[i] = fleet[i].Name
	}
	return fault.New(scope.ErrNotFound, "there's no server %s in the fleet. Choose %s, by name or ID", given, human.Either(names))
}

// protos are records for the schema, with their actors named in one read.
func (h *Handler) protos(ctx context.Context, records ...*revision.Record) ([]*dockwellv1.Change, error) {
	out := make([]*dockwellv1.Change, len(records))
	err := h.db.View(ctx, func(tx *store.Tx) error {
		names := actors.New().In(ctx, tx)
		placer := place.NewPlacer(tx)
		for i, r := range records {
			out[i] = Proto(r, names.Actor)
			// What reaches a project the reader holds no role in is narrowed to theirs.
			if err := Narrow(ctx, tx, out[i]); err != nil {
				return err
			}
			if r.State != store.ChangePlanned {
				continue
			}
			places, err := placer.Of(ctx, r.ID)
			if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, place.ErrUnknown) {
				return err
			}
			out[i].Protected = slices.ContainsFunc(places, func(p place.Place) bool { return p.Protected })
			if err := ForCaller(ctx, tx, h.engine, r.ID, out[i]); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// ForCaller says on a planned change what applying it asks of whoever reads
// it: whether it approves someone else's, and why it waits if their role
// doesn't cover it, as ApplyChange would answer them.
func ForCaller(ctx context.Context, tx *store.Tx, engine *revision.Engine, changeID string, out *dockwellv1.Change) error {
	applying, err := engine.ApplyingOf(ctx, tx, changeID)
	if err != nil {
		return err
	}
	out.Approves, out.Waits, out.Discards = applying.Approves, applying.Waits, applying.Discards
	return nil
}

// Surfaces maps where a change or an audit event came from, as the store
// records it, to the API's Surface.
var Surfaces = map[string]dockwellv1.Surface{
	store.SurfacePanel:     dockwellv1.Surface_SURFACE_PANEL,
	store.SurfaceAPI:       dockwellv1.Surface_SURFACE_API,
	store.SurfaceCLI:       dockwellv1.Surface_SURFACE_CLI,
	store.SurfaceAutomatic: dockwellv1.Surface_SURFACE_AUTOMATIC,
	store.SurfacePush:      dockwellv1.Surface_SURFACE_PUSH,
}

var states = map[string]dockwellv1.ChangeState{
	store.ChangePlanned:   dockwellv1.ChangeState_CHANGE_STATE_PLANNED,
	store.ChangeApplying:  dockwellv1.ChangeState_CHANGE_STATE_APPLYING,
	store.ChangeApplied:   dockwellv1.ChangeState_CHANGE_STATE_APPLIED,
	store.ChangeFailed:    dockwellv1.ChangeState_CHANGE_STATE_FAILED,
	store.ChangeDiscarded: dockwellv1.ChangeState_CHANGE_STATE_DISCARDED,
}

// State is a change's state, as the store records it, for the schema.
func State(state string) dockwellv1.ChangeState { return states[state] }

var stepStates = map[revision.StepState]dockwellv1.StepState{
	revision.StepQueued:  dockwellv1.StepState_STEP_STATE_QUEUED,
	revision.StepRunning: dockwellv1.StepState_STEP_STATE_RUNNING,
	revision.StepDone:    dockwellv1.StepState_STEP_STATE_DONE,
	revision.StepFailed:  dockwellv1.StepState_STEP_STATE_FAILED,
	revision.StepSkipped: dockwellv1.StepState_STEP_STATE_SKIPPED,
	revision.StepWaiting: dockwellv1.StepState_STEP_STATE_WAITING,
}

var rollbackStates = map[string]dockwellv1.RollbackState{
	revision.RollbackApplying: dockwellv1.RollbackState_ROLLBACK_STATE_APPLYING,
	revision.RollbackWaiting:  dockwellv1.RollbackState_ROLLBACK_STATE_WAITING,
	revision.RollbackApplied:  dockwellv1.RollbackState_ROLLBACK_STATE_APPLIED,
	revision.RollbackFailed:   dockwellv1.RollbackState_ROLLBACK_STATE_FAILED,
}

func rollbackProto(r *revision.Rollback) *dockwellv1.RollbackStatus {
	if r == nil {
		return nil
	}
	return &dockwellv1.RollbackStatus{
		Id: r.ID, State: rollbackStates[r.State], Steps: int32(min(r.Steps, math.MaxInt32)), //nolint:gosec // bounded just here
		WaitingFor: r.WaitingFor, Restored: r.Restored, Error: r.Error, Detail: r.Detail,
	}
}

// Proto is a change for the schema: its plan in full when the record has
// it, and in brief otherwise. actor names each actor.
func Proto(r *revision.Record, actor func(string) *dockwellv1.Actor) *dockwellv1.Change {
	c := &dockwellv1.Change{
		Id: r.ID, Summary: r.Summary, State: states[r.State], ProposedBy: actor(r.Actor), ProposedVia: Surfaces[r.ProposedVia],
		CreateTime: timestamppb.New(r.CreatedAt), Plan: planProto(r), Reverts: r.Reverts,
		Rollback: revision.IsRollback(r.Change), AppliedBy: actor(r.AppliedBy), AppliedVia: Surfaces[r.AppliedVia],
		Error: r.Error, RolledBackBy: r.RolledBackBy, UndoneBy: r.UndoneBy, RollbackStatus: rollbackProto(r.Rollback), Because: r.Brief.Because, Restart: r.Brief.Restart,
	}
	if !r.AppliedAt.IsZero() {
		c.ApplyTime = timestamppb.New(r.AppliedAt)
	}
	if d := r.UndoDeadline(); !d.IsZero() {
		c.UndoDeadline = timestamppb.New(d)
	}
	for i := range r.Progress.Steps {
		c.Steps = append(c.Steps, stepProto(&r.Progress.Steps[i]))
	}
	c.Confirmation = confirmationOf(c.Steps)
	return c
}

// confirmationOf is what a change waits for: the confirmation one of its
// running steps waits for, if any.
func confirmationOf(steps []*dockwellv1.StepProgress) *dockwellv1.ChangeConfirmation {
	for _, s := range steps {
		if s.GetConfirmation() != nil {
			return s.GetConfirmation()
		}
	}
	return nil
}

// planProto is a change's plan: in full when the record has it, and in
// brief in a list.
// rolloutProto is how plan p runs over its servers, wave by wave, as the API says
// it, with each server's name; nil for a plan with no waves (ADR 0094).
func rolloutProto(p *revision.Plan) *dockwellv1.PlanRollout {
	w := p.Waves
	if w == nil {
		return nil
	}
	names := map[string]string{}
	for i := range p.Servers {
		names[p.Servers[i].Server] = p.Servers[i].Name
	}
	out := &dockwellv1.PlanRollout{KeepGoing: w.KeepGoing}
	if w.Pause > 0 {
		out.Pause = durationpb.New(w.Pause)
	}
	for _, wave := range w.Order {
		pw := &dockwellv1.RolloutWave{ServerIds: wave}
		for _, id := range wave {
			pw.ServerNames = append(pw.ServerNames, cmp.Or(names[id], id))
		}
		out.Waves = append(out.Waves, pw)
	}
	return out
}

func planProto(r *revision.Record) *dockwellv1.Plan {
	b := r.Brief
	out := &dockwellv1.Plan{
		Fingerprint: b.Fingerprint,
		Summary: &dockwellv1.PlanSummary{
			Create: int32(b.Counts.Create), Change: int32(b.Counts.Change), Replace: int32(b.Counts.Replace), Destroy: int32(b.Counts.Destroy), //nolint:gosec // small counts
		},
		UndoWindow:  durationpb.New(revision.UndoWindow),
		Destructive: b.Destructive,
		HostAccess:  b.HostAccess,
		Intent:      intentProto(b.Intent),
	}
	p := r.Plan
	if p == nil {
		return out
	}
	if rm := p.Removes; rm != nil {
		out.Removes = &dockwellv1.ServerRemoval{ServerId: rm.Server, ServerName: rm.Name, Compromised: rm.Compromised}
	}
	out.Rollout = rolloutProto(p)
	for i := range p.Notes {
		n := &p.Notes[i]
		out.Notes = append(out.Notes, &dockwellv1.PlanNote{Reason: n.Reason, Message: n.Message, Resource: resourcepb.Ref(n.Ref), Names: n.Names, From: n.From, Deleted: n.Deleted, Named: n.Named})
	}
	for _, place := range p.Places {
		out.Places = append(out.Places, &dockwellv1.PlanPlace{Project: place.Project, Environment: place.Environment})
	}
	ids := map[resource.Ref]string{}
	for i := range p.Resources {
		r := &p.Resources[i]
		ids[r.Ref] = r.ID
		out.Resources = append(out.Resources, &dockwellv1.PlannedResource{
			Id: r.ID, Resource: resourcepb.Ref(r.Ref), Action: resourcepb.Action(r.Action), BaseRevision: int64(r.Base),
			Changes: resourcepb.Changes(r.Changes), Servers: r.Servers, PreviousServers: r.Previous,
			HostAccess: resourcepb.HostAccess(r.HostAccess), Install: resourcepb.Install(r.Install), Reboot: resourcepb.Reboot(r.Reboot), Unit: resourcepb.Unit(r.Unit), DockerDaemon: r.DockerDaemon, DockerMaintenance: r.DockerMaintenance, Panel: resourcepb.Panel(r.Panel),
			AgentUpgrade:  resourcepb.AgentUpgrade(r.AgentUpgrade),
			LetsGo:        r.Forget,
			Storage:       linkStorageProto(r.Storage),
			StorageBefore: linkStorageProto(r.StorageBefore),
			Reads:         connectionReadProto(r.Reads),
			ReadsBefore:   connectionReadProto(r.ReadsBefore),
			Sets:          linkSetsProto(r.Sets),
			SetsBefore:    linkSetsProto(r.SetsBefore),
			Needs:         needStoragesProto(r.Needs),
			NeedsBefore:   needStoragesProto(r.NeedsBefore),
		})
	}
	for i := range p.Servers {
		s := &p.Servers[i]
		sp := &dockwellv1.ServerPlan{
			ServerId: s.Server, ServerName: s.Name, Offline: s.Offline, Removed: s.Removed, Downtime: resourcepb.Duration(s.Downtime),
			UntilHealthy: resourcepb.Duration(s.UntilHealthy),
		}
		for j := range s.Steps {
			step := resourcepb.Step(s.Steps[j], ids[s.Steps[j].Ref])
			if d := step.GetDockerDaemon(); d != nil {
				step.DockerDaemon = &dockwellv1.DockerDaemonPlan{Impact: d.GetImpact(), FileChanges: d.GetFileChanges()}
			}
			sp.Steps = append(sp.Steps, step)
		}
		out.Servers = append(out.Servers, sp)
	}
	return out
}

// linkStorageProto is where a link keeps an app's files, as the plan says it.
func linkStorageProto(s *revision.LinkStorage) *dockwellv1.LinkStorage {
	if s == nil {
		return nil
	}
	return &dockwellv1.LinkStorage{Connection: s.Connection, ConnectionLabel: s.Label, Prefix: s.Prefix}
}

// needStoragesProto are where a catalog install's needs come from, as the plan says it.
func needStoragesProto(needs []revision.NeedStorage) []*dockwellv1.NeedStorage {
	out := make([]*dockwellv1.NeedStorage, 0, len(needs))
	for _, n := range needs {
		out = append(out, &dockwellv1.NeedStorage{Need: n.Need, Storage: linkStorageProto(n.Storage), Own: n.Own})
	}
	return out
}

// linkSetsProto are a link's sets whole, as the plan says them.
func linkSetsProto(sets []revision.LinkSetWords) []*resourcev1.LinkSet {
	out := make([]*resourcev1.LinkSet, 0, len(sets))
	for _, s := range sets {
		out = append(out, &resourcev1.LinkSet{Name: s.Name, Part: s.Part, Template: s.Template, TlsOn: s.TLSOn, TlsOff: s.TLSOff})
	}
	return out
}

// connectionReadProto is what a value reads from a connection, as the plan says it.
func connectionReadProto(r *revision.ConnectionRead) *dockwellv1.ConnectionRead {
	if r == nil {
		return nil
	}
	return &dockwellv1.ConnectionRead{Connection: r.Connection, ConnectionLabel: r.Label, Part: r.Part, Link: r.Link}
}

var intentKinds = map[string]dockwellv1.IntentKind{
	revision.IntentDeploy:      dockwellv1.IntentKind_INTENT_KIND_DEPLOY,
	revision.IntentPublishPort: dockwellv1.IntentKind_INTENT_KIND_PUBLISH_PORT,
	revision.IntentRollBack:    dockwellv1.IntentKind_INTENT_KIND_ROLL_BACK,
	revision.IntentRepull:      dockwellv1.IntentKind_INTENT_KIND_REPULL,
	revision.IntentUpgrade:     dockwellv1.IntentKind_INTENT_KIND_UPGRADE,
}

func intentProto(i *revision.Intent) *dockwellv1.Intent {
	if i == nil {
		return nil
	}
	out := &dockwellv1.Intent{
		Kind: intentKinds[i.Kind], ResourceId: i.Resource, Image: i.Image, Commit: i.Commit, Ref: i.Ref,
		FromImage: i.FromImage, FromDigest: i.FromDigest, FromRelease: i.FromRelease, Release: i.Release,
		FromCatalogRelease: i.FromCatalogRelease, CatalogRelease: i.CatalogRelease,
		ToDigest: i.ToDigest,
		Port:     int32(min(i.Port, math.MaxInt32)), Revision: int64(i.Revision), //nolint:gosec // bounded just here
	}
	if !i.FromBuildTime.IsZero() {
		out.FromBuildTime = timestamppb.New(i.FromBuildTime)
	}
	if !i.ToBuildTime.IsZero() {
		out.ToBuildTime = timestamppb.New(i.ToBuildTime)
	}
	if !i.ToPushTime.IsZero() {
		out.ToPushTime = timestamppb.New(i.ToPushTime)
	}
	return out
}

func stepProto(s *revision.StepProgress) *dockwellv1.StepProgress {
	p := &dockwellv1.StepProgress{
		ServerId: s.Server, ResourceId: s.ResourceID, Resource: resourcepb.Ref(s.Ref), Action: resourcepb.Action(s.Action),
		DockerReceipts: s.DockerReceipts, DockerImageReceipts: s.DockerImageReceipts, State: stepStates[s.State], Message: s.Message, Activity: s.Activity, Detail: s.Detail, Reason: s.Reason, Lost: s.Lost,
		Wave:  int32(s.Wave), //nolint:gosec // a wave's number, at most the fleet's size
		Phase: dockwellv1.StepPhase(s.Phase), DoneBytes: s.Done, TotalBytes: s.Total,
	}
	if !s.Started.IsZero() {
		p.StartTime = timestamppb.New(s.Started)
	}
	if !s.Finished.IsZero() {
		p.FinishTime = timestamppb.New(s.Finished)
	}
	p.Did = resourcepb.Did(s.Did)
	if w := s.Confirm; w != nil {
		p.Confirmation = &dockwellv1.ChangeConfirmation{ServerId: s.Server, Deadline: timestamppb.New(w.Deadline), SshPorts: w.SSHPorts, SshAccount: w.SSHAccount}
	}
	return p
}

// codes maps the engine's errors to codes and reasons. Their messages are
// written whole, for people, with the kind of error apart from the text
// (internal/fault), so they go out as they are; anything else is logged and
// reported as internal.
var codes = []struct {
	err    error
	code   connect.Code
	reason string
}{
	{revision.ErrNotFound, connect.CodeNotFound, "not_found"},
	{revision.ErrOutOfReach, connect.CodeNotFound, "not_found"},
	{revision.ErrNeedsRole, connect.CodePermissionDenied, "needs_role"},
	{scope.ErrNotFound, connect.CodeNotFound, "not_found"},
	{revision.ErrInvalid, connect.CodeInvalidArgument, "invalid"},
	{revision.ErrAlreadyExists, connect.CodeInvalidArgument, "already_exists"},
	{revision.ErrStale, connect.CodeFailedPrecondition, "plan_stale"},
	{revision.ErrPlanChanged, connect.CodeFailedPrecondition, "plan_changed"},
	{revision.ErrNotPlanned, connect.CodeFailedPrecondition, "not_planned"},
	{revision.ErrBusy, connect.CodeFailedPrecondition, "busy"},
	{revision.ErrCantUndo, connect.CodeFailedPrecondition, "cant_undo"},
	{revision.ErrOffline, connect.CodeUnavailable, "server_offline"},
	{revision.ErrUnavailable, connect.CodeUnavailable, "not_known_yet"},
	{revision.ErrStopping, connect.CodeUnavailable, "stopping"},
	{resource.ErrLost, connect.CodeUnavailable, "server_offline"},
	{resource.ErrNotConnected, connect.CodeUnavailable, "server_offline"},
	{resource.ErrNoDriver, connect.CodeFailedPrecondition, "no_driver"},
	{secrets.ErrUnsealed, connect.CodeFailedPrecondition, "spec_unreadable"},
	{secrets.ErrOpen, connect.CodeFailedPrecondition, "spec_unreadable"},
}

func (h *Handler) rpcError(ctx context.Context, err error) error {
	// A client that went away comes first: what it waited on, such as an
	// agent's plan, didn't fail.
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	var refusal *connect.Error
	if errors.As(err, &refusal) {
		return refusal
	}
	var choice *scope.ChoiceError
	if errors.As(err, &choice) {
		// The options go in a detail too, so a client can ask which.
		cerr := authapi.ReasonError(connect.CodeInvalidArgument, err, "choice")
		detail := &dockwellv1.Choice{Level: string(choice.Level), Options: choice.Options, Project: choice.Project}
		var at *choiceAt
		if errors.As(err, &at) {
			detail.Field = at.field
		}
		if d, derr := connect.NewErrorDetail(detail); derr == nil {
			cerr.AddDetail(d)
		}
		return cerr
	}
	h.logUnreadable(ctx, err)
	for _, c := range codes {
		if errors.Is(err, c.err) {
			return authapi.ReasonError(c.code, err, c.reason)
		}
	}
	var agent *revision.AgentError
	switch {
	case errors.As(err, &agent) && errors.Is(err, context.DeadlineExceeded):
		return authapi.ReasonError(connect.CodeDeadlineExceeded, err, "agent_timeout")
	case errors.As(err, &agent):
		return authapi.ReasonError(connect.CodeFailedPrecondition, err, "agent_failed")
	}
	h.logger.ErrorContext(ctx, "change call failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't complete the request; try again, and check its logs if it keeps failing"))
}

// logUnreadable puts a stored spec Dockwell won't read in the log, as what reads it
// here says so to its caller and nothing else would: it's the operator's to fix, and
// it's the one failure of this API that says the database itself is wrong.
func (h *Handler) logUnreadable(ctx context.Context, err error) {
	if errors.Is(err, secrets.ErrUnsealed) || errors.Is(err, secrets.ErrOpen) {
		h.logger.ErrorContext(ctx, "can't read a stored spec", slog.String("error", err.Error()))
	}
}

// serverReason says why one server couldn't answer, as an error's reason
// would, or false for an error that isn't the server's.
func serverReason(err error) (string, bool) {
	var agent *revision.AgentError
	switch {
	case errors.Is(err, revision.ErrOffline), errors.Is(err, resource.ErrLost), errors.Is(err, resource.ErrNotConnected):
		return "server_offline", true
	case errors.Is(err, resource.ErrNoDriver):
		return "no_driver", true
	case errors.Is(err, resource.ErrAgentTooOld):
		return "agent_outdated", true
	case errors.Is(err, secrets.ErrUnsealed), errors.Is(err, secrets.ErrOpen):
		return "spec_unreadable", true // one row of what it runs, not the whole store
	case errors.As(err, &agent) && errors.Is(err, context.DeadlineExceeded):
		return "agent_timeout", true
	case errors.As(err, &agent):
		return "agent_failed", true
	}
	return "", false
}
