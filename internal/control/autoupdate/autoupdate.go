// SPDX-License-Identifier: AGPL-3.0-only

// Package autoupdate applies the updates each service takes on its own, in its
// window, as Dockwell's own change (ADR 0080).
package autoupdate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/services"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

// Every is how often the runner looks for a window that's open.
const Every = time.Minute

// Planner plans an update, as ServiceService.UpgradeService does.
type Planner interface {
	UpgradeService(ctx context.Context, req *connect.Request[dockwellv1.UpgradeServiceRequest]) (*connect.Response[dockwellv1.UpgradeServiceResponse], error)
}

// Images reads what an image declares, as a server's pull would find it, for how
// far a moving tag's new image moves (ADR 0080). Nil config where it can't tell.
type Images interface {
	Config(ctx context.Context, server, reference string) (*dockwellv1.ImageConfig, error)
}

// Engine applies and discards what the runner plans.
type Engine interface {
	Apply(ctx context.Context, changeID string) error
	Discard(ctx context.Context, changeID string, by revision.By) error
}

// Runner looks each minute for services whose window is open and that have an
// update their level takes, and updates each once a window: it plans the update
// as Dockwell, and applies it unless its plan needs a person, which it leaves
// planned for one, on Activity. An update that fails rolls back as any does, and
// the change-failed alert says so.
type Runner struct {
	DB      *store.DB
	Keeper  *secrets.Keeper
	Planner Planner
	Engine  Engine
	Updates services.Updates
	// Images reads the versions a moving tag's images say. Nil reads none, so such
	// an update counts as a major one.
	Images   Images
	Logger   *slog.Logger
	Now      func() time.Time
	Interval time.Duration
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Run ticks until ctx is done.
func (r *Runner) Run(ctx context.Context) {
	every := r.Interval
	if every <= 0 {
		every = Every
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := r.Tick(ctx); err != nil && ctx.Err() == nil {
			r.Logger.WarnContext(ctx, "can't look for automatic updates to apply; it looks again in a minute", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// due is a service whose window is open now.
type due struct {
	id, name string
	spec     *resourcev1.Service
	opens    time.Time
	tried    tried
	// server and digest are a server it runs on and the image it runs there, for
	// how far a moving tag's new image moves.
	server, digest string
}

// tried is what the runner last did for a service, kept as a setting so a
// restart neither repeats a window's update nor forgets one it left for a person.
type tried struct {
	Opens  time.Time
	Change string
}

func settingKey(serviceID string) string { return "autoupdate." + serviceID }

func (t tried) encode() []byte {
	return []byte(t.Opens.UTC().Format(time.RFC3339) + " " + t.Change)
}

func decode(b []byte) tried {
	when, change, _ := strings.Cut(string(b), " ")
	at, err := time.Parse(time.RFC3339, when)
	if err != nil {
		return tried{}
	}
	return tried{Opens: at, Change: change}
}

// Tick updates each service whose window is open, once a window.
func (r *Runner) Tick(ctx context.Context) error {
	if r.Updates == nil {
		return nil
	}
	list, err := r.due(ctx)
	if err != nil {
		return err
	}
	for i := range list {
		d := &list[i]
		if err := r.update(ctx, d); err != nil && ctx.Err() == nil {
			r.Logger.WarnContext(ctx, "can't update a service on its own; it tries again in its next window",
				slog.String("service", d.name), slog.String("error", err.Error()))
		}
	}
	return ctx.Err()
}

// due reads the services whose window is open now, and what was last done for each.
func (r *Runner) due(ctx context.Context) ([]due, error) {
	now := r.now()
	var out []due
	err := r.DB.View(ctx, func(tx *store.Tx) error {
		after := ""
		for {
			page, err := tx.PageResources(ctx, string(service.Kind), "", after, 500)
			if err != nil {
				return err
			}
			for i := range page {
				res := page[i].Resource
				after = res.ID
				decoded, err := r.Keeper.Decode(ctx, tx, store.InRevision(res.ID, res.Kind, res.Revision), res.Spec)
				if err != nil {
					continue // what can't be read can't be updated, and its page says why
				}
				svc, ok := decoded.(*resourcev1.Service)
				if !ok || svc.GetAutoUpdate() == nil {
					continue
				}
				opens, _, open := service.UpdateWindow(svc.GetAutoUpdate(), now)
				if !open {
					continue
				}
				d := due{id: res.ID, name: res.Name, spec: svc, opens: opens}
				if d.server, d.digest, err = ranDigest(ctx, tx, res); err != nil {
					return err
				}
				if b, err := tx.GetSetting(ctx, settingKey(res.ID)); err == nil {
					d.tried = decode(b)
				} else if !errors.Is(err, store.ErrNotFound) {
					return err
				}
				out = append(out, d)
			}
			if len(page) < 500 {
				return nil
			}
		}
	})
	return out, err
}

// lookBack is how many revisions ranDigest reads back.
const lookBack = 20

// ranDigest is a server a service runs on and the image it runs there: the newest
// a revision recorded, as one that changed only what the control plane keeps,
// such as its automatic updates, ran no step and recorded none.
func ranDigest(ctx context.Context, tx *store.Tx, res store.Resource) (server, digest string, err error) {
	var revs []int
	for rev := res.Revision; rev >= 1 && len(revs) < lookBack; rev-- {
		revs = append(revs, rev)
	}
	deployed, err := tx.DeploymentsOf(ctx, res.ID, revs)
	if err != nil {
		return "", "", err
	}
	for _, rev := range revs {
		for i := range deployed[rev] {
			if dep := &deployed[rev][i]; dep.Digest != "" {
				return dep.Server, dep.Digest, nil
			}
		}
	}
	return "", "", nil
}

// update plans and applies one service's update, if it has one its level takes,
// and records that this window's is done.
func (r *Runner) update(ctx context.Context, d *due) error {
	if d.tried.Opens.Equal(d.opens) {
		return nil
	}
	newer, ok, err := r.Updates.Of(ctx, d.id)
	if err != nil || !ok {
		return err
	}
	a := d.spec.GetAutoUpdate()
	kind := newer.Kind
	if kind == "digest" {
		kind = r.movedBy(ctx, d, newer.Image)
	}
	if !service.Takes(a.GetLevel(), kind) {
		return nil
	}
	// A database's major release is never taken on its own: the plan would refuse a
	// plain swap anyway, and the safe move is a person's to start.
	if kind == "major" {
		if _, db := database.FromSpec(d.spec); db {
			return nil
		}
	}
	// What a window left for a person goes once the next window plans again.
	if d.tried.Change != "" {
		if err := r.Engine.Discard(ctx, d.tried.Change, revision.By{Actor: revision.SystemActor, Via: revision.ViaAutomatic}); err != nil && !errors.Is(err, revision.ErrNotFound) && !errors.Is(err, revision.ErrNotPlanned) {
			r.Logger.WarnContext(ctx, "can't discard the update an earlier window left for a person", slog.String("change", d.tried.Change), slog.String("error", err.Error()))
		}
	}
	// A catalog install's update upgrades the install, which takes no image.
	image, summary := newer.Image, fmt.Sprintf("Update %s to %s on its own, in its window", d.name, newer.Image)
	if newer.Catalog {
		image, summary = "", fmt.Sprintf("Update %s to its catalog's newer release on its own, in its window", d.name)
	}
	res, err := r.Planner.UpgradeService(changes.AsDockwell(ctx), connect.NewRequest(&dockwellv1.UpgradeServiceRequest{
		Id: d.id, Image: image, SkipBackup: a.GetSkipBackup(),
		Summary: summary,
	}))
	if err != nil {
		// What it can't plan now, such as with its server offline, it tries again
		// within the window; what it refuses, as a plan always would, waits for the next.
		if code := connect.CodeOf(err); code == connect.CodeInvalidArgument || code == connect.CodeFailedPrecondition {
			return errors.Join(err, r.record(ctx, d.id, tried{Opens: d.opens}))
		}
		return err
	}
	c := res.Msg.GetChange()
	switch plan := c.GetPlan(); {
	case len(plan.GetResources()) == 0 && len(plan.GetServers()) == 0:
		return errors.Join(r.Engine.Discard(ctx, c.GetId(), revision.By{Actor: revision.SystemActor, Via: revision.ViaAutomatic}), r.record(ctx, d.id, tried{Opens: d.opens}))
	case needsPerson(c):
		r.Logger.InfoContext(ctx, "an automatic update needs a person to apply it, so it's left planned", slog.String("service", d.name), slog.String("change", c.GetId()))
		return r.record(ctx, d.id, tried{Opens: d.opens, Change: c.GetId()})
	}
	if err := r.record(ctx, d.id, tried{Opens: d.opens}); err != nil {
		return err
	}
	return r.Engine.Apply(ctx, c.GetId())
}

// movedBy is how far a moving tag's new image moves from what runs, as their
// versions say, read through the agent of a server it runs on: what runs from
// the server's own copy, and the new image from its registry. A tag that names a
// release whole, as 1.27.3 does, moves within it. Where nothing can tell, the
// move counts as the largest, a major one, so only an app that takes any update
// takes it on its own: "latest" can move ghost from 5.x to 6.0 (ADR 0080).
func (r *Runner) movedBy(ctx context.Context, d *due, image string) string {
	if r.Images != nil && d.server != "" && d.digest != "" {
		was, errWas := r.Images.Config(ctx, d.server, docker.Repository(image)+"@"+d.digest)
		now, errNow := r.Images.Config(ctx, d.server, image)
		if errWas == nil && errNow == nil {
			if step := Step(image, was, now); step != "" {
				return step
			}
		}
	}
	if NamesARelease(image) {
		return "patch"
	}
	return "major"
}

// needsPerson says a plan asks for a person: one that destroys data, lets two
// containers write to one place or reaches what a container can reach on its
// server, or waits for an admin.
func needsPerson(c *dockwellv1.Change) bool {
	return c.GetPlan().GetDestructive() || c.GetPlan().GetHostAccess() || c.GetWaits() != ""
}

func (r *Runner) record(ctx context.Context, serviceID string, t tried) error {
	return r.DB.Update(ctx, func(tx *store.Tx) error { return tx.PutSetting(ctx, settingKey(serviceID), t.encode()) })
}
