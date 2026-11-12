// SPDX-License-Identifier: AGPL-3.0-only

package serviceupdates

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// The audit log's actions for what a person sets aside (ADR 0079).
const (
	ActionSkip     = "service.update_skip"
	ActionUnskip   = "service.update_unskip"
	ActionSnooze   = "service.update_snooze"
	ActionUnsnooze = "service.update_unsnooze"
)

// Actions are every action this package writes to the audit log.
var Actions = []string{ActionSkip, ActionUnskip, ActionSnooze, ActionUnsnooze}

// checkWait is how long Check now waits for what it checks before it answers
// with what's known, the check going on.
const checkWait = 30 * time.Second

// Services finds services and environments a request names.
type Services interface {
	// Find reads the service a request names, by ID or by name.
	Find(ctx context.Context, named, project, environment string) (store.Resource, error)
	// EnvironmentID is the ID of the environment a request names.
	EnvironmentID(ctx context.Context, project, environment string) (string, error)
}

// Handler serves ServiceUpdateService.
type Handler struct {
	keeper   *Keeper
	services Services
	servers  func(named string) (string, error)
	logger   *slog.Logger
}

var _ dockwellv1connect.ServiceUpdateServiceHandler = (*Handler)(nil)

// NewHandler serves what keeper knows, finding services through services and
// servers, by ID or name, through servers.
func NewHandler(keeper *Keeper, services Services, servers func(named string) (string, error), logger *slog.Logger) *Handler {
	return &Handler{keeper: keeper, services: services, servers: servers, logger: logger}
}

func (h *Handler) internal(ctx context.Context, err error) error {
	var said *connect.Error
	if errors.As(err, &said) {
		return said
	}
	if errors.Is(err, context.Canceled) {
		return connect.NewError(connect.CodeCanceled, err)
	}
	h.logger.ErrorContext(ctx, "a call about the apps' updates failed", slog.String("error", err.Error()))
	return connect.NewError(connect.CodeInternal, errors.New("the control plane couldn't read the apps' updates; try again, and check its logs if it keeps failing"))
}

// ListServiceUpdates lists each service with what's newer than what it runs.
func (h *Handler) ListServiceUpdates(ctx context.Context, req *connect.Request[dockwellv1.ListServiceUpdatesRequest]) (*connect.Response[dockwellv1.ListServiceUpdatesResponse], error) {
	m := req.Msg
	in := ""
	if m.GetProject() != "" || m.GetEnvironment() != "" {
		env, err := h.services.EnvironmentID(ctx, m.GetProject(), m.GetEnvironment())
		if err != nil {
			return nil, err
		}
		in = env
	}
	rows, err := h.keeper.rows(ctx, func(s subject) bool { return in == "" || s.Environment == in })
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	res := &dockwellv1.ListServiceUpdatesResponse{}
	for _, r := range rows {
		if m.GetAvailable() && !Counts(r.GetUpdate(), h.keeper.now()) {
			continue
		}
		res.Services = append(res.Services, r)
	}
	return connect.NewResponse(res), nil
}

// Counts reports a service's updates that count: something newer that isn't
// skipped, while they aren't snoozed.
func Counts(u *dockwellv1.ServiceUpdate, now time.Time) bool {
	if u.GetCheck() != dockwellv1.UpdateCheck_UPDATE_CHECK_AVAILABLE {
		return false
	}
	if t := u.GetSnoozeTime(); t != nil && t.AsTime().After(now) {
		return false
	}
	return slices.ContainsFunc(u.GetAvailable(), func(a *dockwellv1.AvailableUpdate) bool { return !a.GetSkipped() })
}

// CheckServiceUpdates checks a service, or a server's services, now.
func (h *Handler) CheckServiceUpdates(ctx context.Context, req *connect.Request[dockwellv1.CheckServiceUpdatesRequest]) (*connect.Response[dockwellv1.CheckServiceUpdatesResponse], error) {
	m := req.Msg
	var keep func(subject) bool
	switch {
	case m.GetId() != "":
		r, err := h.services.Find(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
		if err != nil {
			return nil, err
		}
		if err := h.keeper.CheckService(ctx, r.ID); err != nil {
			return nil, h.internal(ctx, err)
		}
		keep = func(s subject) bool { return s.ID == r.ID }
	case m.GetServer() != "":
		server, err := h.servers(m.GetServer())
		if err != nil {
			return nil, err
		}
		if err := h.keeper.CheckServer(ctx, server); err != nil {
			return nil, h.internal(ctx, err)
		}
		keep = func(s subject) bool { return s.home == server }
	default:
		return nil, authapi.Invalid(errors.New("name a service to check, by its ID or name, or a server to check every app on"))
	}
	rows, err := h.keeper.rows(ctx, keep)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.CheckServiceUpdatesResponse{Services: rows}), nil
}

// SkipServiceUpdate sets one version of a service's updates aside, or takes
// that back.
func (h *Handler) SkipServiceUpdate(ctx context.Context, req *connect.Request[dockwellv1.SkipServiceUpdateRequest]) (*connect.Response[dockwellv1.SkipServiceUpdateResponse], error) {
	m := req.Msg
	version := strings.TrimSpace(m.GetVersion())
	if version == "" || len(version) > 200 {
		return nil, authapi.Invalid(errors.New("name the version to skip, as the update says it, such as 17.2"))
	}
	r, err := h.services.Find(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first"))
	}
	client, _ := authapi.ClientFrom(ctx)
	now := h.keeper.now()
	err = h.keeper.DB.Update(ctx, func(tx *store.Tx) error {
		rows, err := tx.ServiceUpdates(ctx, []string{r.ID})
		if err != nil {
			return err
		}
		skipped := decodeSkipped(rows[r.ID].Skipped)
		had := slices.Contains(skipped, version)
		action := ActionSkip
		switch {
		case m.GetUnskip() && !had, !m.GetUnskip() && had:
			return nil // as asked already
		case m.GetUnskip():
			action = ActionUnskip
			skipped = slices.DeleteFunc(skipped, func(v string) bool { return v == version })
		default:
			skipped = append(skipped, version)
			if len(skipped) > maxSkipped {
				skipped = skipped[len(skipped)-maxSkipped:]
			}
		}
		b, _ := json.Marshal(skipped)
		if len(skipped) == 0 {
			b = nil
		}
		if err := tx.SetServiceUpdateSkipped(ctx, r.ID, string(b)); err != nil {
			return err
		}
		return record(ctx, tx, action, r, map[string]string{"version": version}, p.Actor(), client.Surface(p), now)
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	u, err := h.keeper.one(ctx, r.ID)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.SkipServiceUpdateResponse{Update: u}), nil
}

// SnoozeServiceUpdates sets a service's updates aside for a while, or ends that.
func (h *Handler) SnoozeServiceUpdates(ctx context.Context, req *connect.Request[dockwellv1.SnoozeServiceUpdatesRequest]) (*connect.Response[dockwellv1.SnoozeServiceUpdatesResponse], error) {
	m := req.Msg
	var d time.Duration
	if s := strings.TrimSpace(m.GetDuration()); s != "" {
		var err error
		if d, err = parseDuration(s); err != nil || d <= 0 || d > maxSnooze {
			return nil, authapi.Invalid(fmt.Errorf("%q isn't a snooze Dockwell takes. Say 1d, 7d or 30d, or a duration such as 12h, 90 days at most", s))
		}
	}
	r, err := h.services.Find(ctx, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, err
	}
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first"))
	}
	client, _ := authapi.ClientFrom(ctx)
	now := h.keeper.now().Truncate(time.Millisecond)
	err = h.keeper.DB.Update(ctx, func(tx *store.Tx) error {
		rows, err := tx.ServiceUpdates(ctx, []string{r.ID})
		if err != nil {
			return err
		}
		snoozed := rows[r.ID].SnoozeUntil.After(now)
		if d == 0 {
			if !snoozed {
				return nil
			}
			if err := tx.SetServiceUpdateSnooze(ctx, r.ID, time.Time{}, ""); err != nil {
				return err
			}
			return record(ctx, tx, ActionUnsnooze, r, nil, p.Actor(), client.Surface(p), now)
		}
		until := now.Add(d)
		if err := tx.SetServiceUpdateSnooze(ctx, r.ID, until, p.Actor()); err != nil {
			return err
		}
		return record(ctx, tx, ActionSnooze, r, map[string]string{"until": until.UTC().Format(time.RFC3339)}, p.Actor(), client.Surface(p), now)
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	u, err := h.keeper.one(ctx, r.ID)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.SnoozeServiceUpdatesResponse{Update: u}), nil
}

// parseDuration reads a snooze's length: days as "7d", or Go's durations.
func parseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, err
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// record puts what a person set aside on the audit log, about the service.
func record(ctx context.Context, tx *store.Tx, action string, r store.Resource, detail map[string]string, actor, surface string, now time.Time) error {
	if detail == nil {
		detail = map[string]string{}
	}
	detail["service"] = r.Name
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	return tx.RecordEvent(ctx, store.Event{
		ID: id.New(id.Event), At: now, Actor: actor, Action: action, Subject: r.ID, Detail: string(b), Surface: surface,
	})
}

// rows are the services keep holds, each with its updates, by environment and
// name.
func (k *Keeper) rows(ctx context.Context, keep func(subject) bool) ([]*dockwellv1.ServiceUpdates, error) {
	out, _, err := k.rowsWith(ctx, keep)
	return out, err
}

// rowsWith is rows, with the server each service is checked through, by its ID.
func (k *Keeper) rowsWith(ctx context.Context, keep func(subject) bool) ([]*dockwellv1.ServiceUpdates, map[string]string, error) {
	subjects, _, err := k.load(ctx)
	if err != nil {
		return nil, nil, err
	}
	homes := make(map[string]string, len(subjects))
	for i := range subjects {
		homes[subjects[i].ID] = subjects[i].home
	}
	var picked []subject
	for i := range subjects {
		if keep(subjects[i]) {
			picked = append(picked, subjects[i])
		}
	}
	plain := make([]Subject, len(picked))
	for i := range picked {
		plain[i] = picked[i].Subject
	}
	described, err := k.Describe(ctx, plain)
	if err != nil {
		return nil, nil, err
	}
	out := make([]*dockwellv1.ServiceUpdates, 0, len(picked))
	for i := range picked {
		s := &picked[i]
		out = append(out, &dockwellv1.ServiceUpdates{
			Id:       s.ID,
			Resource: &dockwellv1.ResourceRef{Kind: "service", Scope: s.Environment, Name: s.Name},
			Image:    imageOf(s.Spec),
			Update:   described[s.ID],
		})
	}
	slices.SortFunc(out, func(a, b *dockwellv1.ServiceUpdates) int {
		return cmp.Or(cmp.Compare(a.GetResource().GetScope(), b.GetResource().GetScope()), cmp.Compare(a.GetResource().GetName(), b.GetResource().GetName()))
	})
	return out, homes, nil
}

// one is a service's updates as they stand.
func (k *Keeper) one(ctx context.Context, serviceID string) (*dockwellv1.ServiceUpdate, error) {
	rows, err := k.rows(ctx, func(s subject) bool { return s.ID == serviceID })
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0].GetUpdate(), nil
}

// asked reports, and notes, a Check now of a target within the last minute.
func (k *Keeper) asked(target string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.askedAt == nil {
		k.askedAt = map[string]time.Time{}
	}
	now := k.now()
	if at, ok := k.askedAt[target]; ok && now.Sub(at) < nowEvery {
		return true
	}
	k.askedAt[target] = now
	return false
}

// CheckService checks one service now, through its server, and waits up to
// half a minute for it. Asked again within the minute, it checks nothing.
func (k *Keeper) CheckService(ctx context.Context, serviceID string) error {
	if k.asked("svc:" + serviceID) {
		return nil
	}
	subjects, _, err := k.load(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(subjects, func(s subject) bool { return s.ID == serviceID })
	if i < 0 || subjects[i].install {
		return nil // a catalog install's is read from its catalog, as it's asked for
	}
	s := subjects[i]
	if s.home == "" || !k.Agents.Connected(s.home) {
		return k.unreachable(ctx, s)
	}
	return k.wait(ctx, func(bg context.Context) { k.check(bg, s.home, []subject{s}) })
}

// CheckServer checks every service on a server now, and waits up to half a
// minute for it. Asked again within the minute, it checks nothing.
func (k *Keeper) CheckServer(ctx context.Context, server string) error {
	if k.asked("srv:" + server) {
		return nil
	}
	if !k.Agents.Connected(server) {
		return authapi.ReasonError(connect.CodeUnavailable, errors.New("that server's agent isn't connected, so its apps can't be checked now. They're checked when it reconnects"), authapi.ReasonUnavailable)
	}
	subjects, _, err := k.load(ctx)
	if err != nil {
		return err
	}
	var list []subject
	for i := range subjects {
		if s := &subjects[i]; s.home == server && !s.install {
			list = append(list, *s)
		}
	}
	if len(list) == 0 {
		return nil
	}
	return k.wait(ctx, func(bg context.Context) { k.pass(bg, server, list) })
}

// wait runs a check apart from the request, so leaving doesn't stop it, and
// waits for it up to checkWait.
func (k *Keeper) wait(ctx context.Context, run func(context.Context)) error {
	done := make(chan struct{})
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
	go func() {
		defer cancel()
		defer close(done)
		run(bg)
	}()
	t := time.NewTimer(checkWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// unreachable notes a service that can't be checked now: one that runs nowhere
// yet, or on no connected server, keeping what was found before.
func (k *Keeper) unreachable(ctx context.Context, s subject) error {
	now := k.now()
	row := store.ServiceUpdate{ServiceID: s.ID, ServerID: s.home, Image: checkedAs(s.Spec), TriedAt: now, NextAt: now.Add(k.every())}
	if s.kept && s.row.Image == row.Image {
		row.CheckedAt, row.Found = s.row.CheckedAt, s.row.Found
	}
	row.State, row.Problem = int32(dockwellv1.UpdateCheck_UPDATE_CHECK_FAILED), int32(dockwellv1.UpdateProblem_UPDATE_PROBLEM_SERVER_OFFLINE)
	if s.home == "" {
		row.State, row.Problem = int32(dockwellv1.UpdateCheck_UPDATE_CHECK_NOT_CHECKED), int32(dockwellv1.UpdateProblem_UPDATE_PROBLEM_NOT_RUNNING)
	}
	return k.DB.Update(ctx, func(tx *store.Tx) error { return tx.PutServiceUpdateCheck(ctx, row) })
}

// Next is what an update of a service with no image named moves it to: the
// first of what's newer that a person hasn't skipped, nil when there's none
// or its updates are snoozed. It's what U2's automatic updates and its plan
// with no image read (ADR 0080).
func (k *Keeper) Next(ctx context.Context, serviceID string) (*dockwellv1.AvailableUpdate, error) {
	u, err := k.one(ctx, serviceID)
	if err != nil || u == nil {
		return nil, err
	}
	if t := u.GetSnoozeTime(); t != nil && t.AsTime().After(k.now()) {
		return nil, nil
	}
	for _, a := range u.GetAvailable() {
		if !a.GetSkipped() {
			return a, nil
		}
	}
	return nil, nil
}

// Skipped reports a version a person skipped of a service's updates, as an
// AvailableUpdate's to_version, to_digest or to_commit says it.
func (k *Keeper) Skipped(ctx context.Context, serviceID, version string) (bool, error) {
	var skipped []string
	err := k.DB.View(ctx, func(tx *store.Tx) error {
		rows, err := tx.ServiceUpdates(ctx, []string{serviceID})
		skipped = decodeSkipped(rows[serviceID].Skipped)
		return err
	})
	return slices.Contains(skipped, version), err
}

// Snoozed reports a service whose updates a person set aside for now.
func (k *Keeper) Snoozed(ctx context.Context, serviceID string) (bool, error) {
	var until time.Time
	err := k.DB.View(ctx, func(tx *store.Tx) error {
		rows, err := tx.ServiceUpdates(ctx, []string{serviceID})
		until = rows[serviceID].SnoozeUntil
		return err
	})
	return until.After(k.now()), err
}

// countingFor is how long what Counting found is kept, as the alerts ask it
// each time they look.
const countingFor = time.Minute

// Counted is a service with an update that counts, and the first it offers.
type Counted struct {
	ServiceID, Name, ServerID string
	Offer                     *dockwellv1.AvailableUpdate
}

// Counting is every service with an update that counts: newer than what it runs,
// not skipped and not snoozed, as the alerts read it, kept a minute.
func (k *Keeper) Counting(ctx context.Context) ([]Counted, error) {
	k.mu.Lock()
	if k.counted != nil && k.now().Sub(k.countedAt) < countingFor {
		out := k.counted
		k.mu.Unlock()
		return out, nil
	}
	k.mu.Unlock()
	now := k.now()
	rows, homes, err := k.rowsWith(ctx, func(subject) bool { return true })
	if err != nil {
		return nil, err
	}
	out := []Counted{}
	for _, r := range rows {
		if !Counts(r.GetUpdate(), now) {
			continue
		}
		out = append(out, Counted{ServiceID: r.GetId(), Name: r.GetResource().GetName(), ServerID: homes[r.GetId()], Offer: offered(r.GetUpdate())})
	}
	k.mu.Lock()
	k.counted, k.countedAt = out, now
	k.mu.Unlock()
	return out, nil
}

// offered is the first update that isn't skipped.
func offered(u *dockwellv1.ServiceUpdate) *dockwellv1.AvailableUpdate {
	for _, a := range u.GetAvailable() {
		if !a.GetSkipped() {
			return a
		}
	}
	return nil
}
