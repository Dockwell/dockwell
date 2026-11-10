// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/health"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/store"
)

// signals reads, once a tick, what the control plane holds of servers, services,
// certificates, backups and changes, for each rule to judge.
type signals struct {
	e   *Evaluator
	now time.Time

	servers []*dockwellv1.Server
	health  []*dockwellv1.ServiceHealth
	read    bool
	// lastChange is the newest failed change read, which the next tick reads after.
	lastChange time.Time
}

// of is what a rule's signal says of each thing it watches.
func (s *signals) of(ctx context.Context, r rule) ([]finding, error) {
	switch r.spec.GetCondition() {
	case resourcev1.AlertRule_CONDITION_SERVICE_DOWN:
		return s.servicesDown(ctx)
	case resourcev1.AlertRule_CONDITION_SERVER_UNREACHABLE:
		return s.unreachable(), nil
	case resourcev1.AlertRule_CONDITION_DISK_FULL:
		return s.disks(r), nil
	case resourcev1.AlertRule_CONDITION_MEMORY_HIGH:
		return s.memory(r), nil
	case resourcev1.AlertRule_CONDITION_CERTIFICATE_EXPIRING, resourcev1.AlertRule_CONDITION_CERTIFICATE_FAILED:
		return s.certificates(ctx, r)
	case resourcev1.AlertRule_CONDITION_BACKUP_FAILED:
		return s.backupsFailed(ctx)
	case resourcev1.AlertRule_CONDITION_BACKUP_LATE:
		return s.backupsLate(ctx)
	case resourcev1.AlertRule_CONDITION_CHANGE_FAILED:
		return s.changesFailed(ctx, r)
	case resourcev1.AlertRule_CONDITION_RESTORE_DRILL_FAILED:
		return s.drillsFailed(ctx)
	case resourcev1.AlertRule_CONDITION_UNSPECIFIED:
	}
	return nil, fmt.Errorf("no signal for %v", r.spec.GetCondition())
}

func (s *signals) fleet() []*dockwellv1.Server {
	if s.servers == nil && s.e.Fleet != nil {
		s.servers = s.e.Fleet.List()
	}
	return s.servers
}

func (s *signals) servicesDown(ctx context.Context) ([]finding, error) {
	if !s.read {
		if s.e.Health == nil {
			return nil, errors.New("nothing tells the control plane how services are")
		}
		h, err := s.e.Health.Placed(ctx)
		if err != nil {
			return nil, err
		}
		s.health, s.read = h, true
	}
	var out []finding
	for _, svc := range s.health {
		name := svc.GetResource().GetName()
		project := docker.Project(svc.GetResource().GetScope())
		for _, on := range svc.GetServers() {
			f := finding{
				thing: "svc:" + svc.GetId() + "@" + on.GetServerId(), serverID: on.GetServerId(),
				subjectID: svc.GetId(), subjectName: name, path: "/apps/" + svc.GetId(),
				facts: Facts{Server: on.GetServerName()}, verdict: unknown,
			}
			if state := on.GetState(); state == dockwellv1.HealthState_HEALTH_STATE_UNKNOWN || state == dockwellv1.HealthState_HEALTH_STATE_UNSPECIFIED {
				// Its server is away, or hasn't said: server-unreachable says that.
				out = append(out, f)
				continue
			}
			standing := s.standing(on, project, name)
			switch {
			case !standing.Heard:
			case standing.Finished, standing.State == dockwellv1.HealthState_HEALTH_STATE_PAUSED, !standing.Down:
				// A job that did its work, and an app someone paused, aren't down.
				f.verdict = fine
			default:
				f.verdict, f.since = holds, standing.Since
				switch {
				case standing.Flapping:
					f.facts.Variant, f.title = "flapping", fmt.Sprintf("%s keeps going down on %s", name, on.GetServerName())
				case standing.State == dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY:
					f.facts.Variant, f.title = "unhealthy", fmt.Sprintf("%s fails its health check on %s", name, on.GetServerName())
				case standing.State == dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING:
					f.facts.Variant, f.title = "not_running", fmt.Sprintf("%s isn't running on %s", name, on.GetServerName())
				default:
					f.facts.Variant, f.title = "restarting", fmt.Sprintf("%s keeps restarting on %s", name, on.GetServerName())
				}
				f.detail = "Its visitors get an error there. Its logs say why: fix it, then deploy again, or roll back to what ran before."
			}
			out = append(out, f)
		}
	}
	return out, nil
}

// standing is how a service stands on a server: as the health recorder folds it,
// or, with none, as its state says.
func (s *signals) standing(on *dockwellv1.ServerHealth, project, name string) health.Standing {
	if s.e.Standings != nil {
		return s.e.Standings.Standing(on.GetServerId(), project, name)
	}
	st := health.Standing{Heard: true, State: on.GetState()}
	if state := on.GetState(); state == dockwellv1.HealthState_HEALTH_STATE_UNHEALTHY || state == dockwellv1.HealthState_HEALTH_STATE_NOT_RUNNING {
		st.Down = true
		if on.GetSince() != nil {
			st.Since = on.GetSince().AsTime()
		}
	}
	return st
}

func (s *signals) unreachable() []finding {
	var out []finding
	for _, srv := range s.fleet() {
		f := finding{thing: "srv:" + srv.GetId(), serverID: srv.GetId(), path: "/servers/" + srv.GetId(), facts: Facts{Server: srv.GetName()}}
		switch srv.GetConnection() {
		case dockwellv1.Connection_CONNECTION_CONNECTED:
			f.verdict = fine
		case dockwellv1.Connection_CONNECTION_UNREACHABLE:
			f.verdict, f.title = holds, srv.GetName()+" can't be reached"
			// After a restart every agent has to dial in again, so its clock starts
			// then, not when it was last heard before.
			f.since = s.e.started
			last := "the control plane started"
			if seen := srv.GetLastSeenTime(); seen != nil {
				if seen.AsTime().After(f.since) {
					f.since = seen.AsTime()
				}
				last = seen.AsTime().UTC().Format("15:04 UTC, 2 Jan")
			}
			f.detail = fmt.Sprintf("Dockwell hasn't heard from its agent since %s. What runs there may still run, but nothing there can be changed or checked. Check the server is up and reaches the control plane, then run systemctl restart dockwell on it.", last)
		default:
			continue // it hasn't joined yet
		}
		out = append(out, f)
	}
	return out
}

// fresh are a server's stats, while they say how it is now.
func (s *signals) fresh(srv *dockwellv1.Server) *dockwellv1.ServerStats {
	st := srv.GetStats()
	if srv.GetConnection() != dockwellv1.Connection_CONNECTION_CONNECTED || st == nil || st.GetTime() == nil || s.now.Sub(st.GetTime().AsTime()) > statsGoodFor {
		return nil
	}
	return st
}

func percent(used, total uint64) int {
	if total == 0 {
		return 0
	}
	return int(math.Round(float64(used) * 100 / float64(total)))
}

func (s *signals) disks(r rule) []finding {
	var out []finding
	limit := int(r.spec.GetThreshold())
	for _, srv := range s.fleet() {
		prefix := "disk:" + srv.GetId() + ":"
		st := s.fresh(srv)
		if st == nil {
			out = append(out, finding{thing: prefix, prefix: true, verdict: unknown})
			continue
		}
		for _, d := range st.GetDisks() {
			pct := percent(d.GetUsedBytes(), d.GetSizeBytes())
			f := finding{
				thing: prefix + d.GetMountPoint(), serverID: srv.GetId(), path: "/servers/" + srv.GetId(),
				verdict: level(d.GetSizeBytes() > 0, pct, limit), facts: Facts{Server: srv.GetName(), Where: d.GetMountPoint(), Value: int32(pct)}, //nolint:gosec // a percent
			}
			if f.verdict == holds {
				f.title = fmt.Sprintf("%s's disk %s is %d%% full", srv.GetName(), d.GetMountPoint(), pct)
				f.detail = fmt.Sprintf("It's over %d%%. Once it fills, apps there can't write, and Docker can't pull. Free space with docker system prune, or grow the disk.", limit)
			}
			out = append(out, f)
		}
	}
	return out
}

func (s *signals) memory(r rule) []finding {
	var out []finding
	limit := int(r.spec.GetThreshold())
	for _, srv := range s.fleet() {
		f := finding{thing: "mem:" + srv.GetId(), serverID: srv.GetId(), path: "/servers/" + srv.GetId(), verdict: unknown, facts: Facts{Server: srv.GetName()}}
		if st := s.fresh(srv); st != nil {
			pct := percent(st.GetMemoryUsedBytes(), st.GetMemoryTotalBytes())
			f.facts.Value = int32(pct) //nolint:gosec // a percent
			if f.verdict = level(st.GetMemoryTotalBytes() > 0, pct, limit); f.verdict == holds {
				f.title = fmt.Sprintf("%s uses %d%% of its memory", srv.GetName(), pct)
				f.detail = fmt.Sprintf("It's over %d%%. Past what it has, the kernel stops apps to free memory. The server's page says which app takes it: give the server more, or the app a limit.", limit)
			}
		}
		out = append(out, f)
	}
	return out
}

// level judges a percent against a rule's threshold: over it holds, and an open
// alert is clear of it only once it's band points under, so a level that hovers
// about the line doesn't open and resolve over and over. Under the line, within
// the band, it's nearly: a pending alert starts again there, as it isn't over.
func level(known bool, pct, limit int) verdict {
	switch {
	case !known:
		return fine
	case pct > limit:
		return holds
	case pct > limit-band:
		return nearly
	}
	return fine
}

func (s *signals) certificates(ctx context.Context, r rule) ([]finding, error) {
	expiring := r.spec.GetCondition() == resourcev1.AlertRule_CONDITION_CERTIFICATE_EXPIRING
	days := int(r.spec.GetThreshold())
	names := map[string]string{}
	for _, srv := range s.fleet() {
		names[srv.GetId()] = srv.GetName()
	}
	var out []finding
	err := s.e.DB.View(ctx, func(tx *store.Tx) error {
		routes, err := tx.ListKind(ctx, string(route.Kind))
		if err != nil {
			return err
		}
		ids := make([]string, len(routes))
		for i := range routes {
			ids[i] = routes[i].ID
		}
		placed, err := tx.LatestDeployments(ctx, ids)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for i := range routes {
			rt := &routes[i]
			spec, err := s.e.Keeper.Decode(ctx, tx, store.InRevision(rt.ID, rt.Kind, rt.Revision), rt.Spec)
			if err != nil {
				return err
			}
			r := spec.(*resourcev1.Route)
			if r.GetDomain() == "" || r.GetTls() == ingress.TLSOff {
				continue
			}
			for j := range placed[rt.ID] {
				d := &placed[rt.ID][j]
				thing := "cert:" + r.GetDomain() + "@" + d.Server
				if seen[thing] || names[d.Server] == "" {
					continue
				}
				seen[thing] = true
				out = append(out, s.certificate(thing, r.GetDomain(), d.Server, names[d.Server], rt.ID, expiring, days))
			}
		}
		return nil
	})
	return out, err
}

func (s *signals) certificate(thing, domain, server, serverName, routeID string, expiring bool, days int) finding {
	f := finding{thing: thing, serverID: server, subjectID: routeID, subjectName: domain, path: "/domains", verdict: unknown, facts: Facts{Server: serverName}}
	status, connected := s.e.Domains.Domain(server, domain)
	if !connected || status == nil {
		return f
	}
	c := status.GetCertificate()
	f.verdict = fine
	switch {
	case expiring && c.GetState() == dockwellv1.CertificateState_CERTIFICATE_STATE_ISSUED && c.GetExpireTime() != nil:
		remaining := c.GetExpireTime().AsTime().Sub(s.now)
		if remaining < time.Duration(days)*24*time.Hour {
			left := max(int(math.Ceil(remaining.Hours()/24)), 0)
			f.verdict, f.facts.Value = holds, int32(left) //nolint:gosec // days, under 90
			switch left {
			case 0:
				f.title = domain + "'s certificate has ended"
			case 1:
				f.title = domain + "'s certificate ends within a day"
			default:
				f.title = fmt.Sprintf("%s's certificate ends in %d days", domain, left)
			}
			why := "Its renewal hasn't worked yet."
			if c.GetMessage() != "" {
				why = c.GetMessage()
			}
			f.detail = fmt.Sprintf("Dockwell renews a certificate 30 days before it ends, and this one hasn't been: %s Once it ends, visitors get an error over HTTPS. Check %s still points at %s, then see Domains.", why, domain, serverName)
		}
	case !expiring && c.GetState() == dockwellv1.CertificateState_CERTIFICATE_STATE_FAILED:
		f.verdict = holds
		f.title = domain + " has no certificate"
		why := "Its certificate couldn't be issued."
		if c.GetMessage() != "" {
			why = c.GetMessage()
		}
		f.detail = why + " Visitors get an error over HTTPS until it has one. Domains says what to fix."
	case !expiring && c.GetState() == dockwellv1.CertificateState_CERTIFICATE_STATE_PENDING:
		f.verdict = unknown // it's being issued: whether it will be is yet to say
	}
	return f
}

func (s *signals) backupsFailed(ctx context.Context) ([]finding, error) {
	var out []finding
	err := s.e.DB.View(ctx, func(tx *store.Tx) error {
		// However long ago a volume's newest backup was, it says how its backups
		// stand: a failure stays one until a backup works, or the volume goes.
		latest, err := tx.LatestBackups(ctx, time.Time{})
		if err != nil {
			return err
		}
		volumes, err := tx.ListKind(ctx, "volume")
		if err != nil {
			return err
		}
		there := make(map[string]bool, len(volumes))
		for i := range volumes {
			there[volumes[i].ID] = true
		}
		for k := range latest {
			b := latest[k]
			if !there[b.VolumeID] {
				continue
			}
			f := finding{thing: "vol:" + b.VolumeID, serverID: b.ServerID, subjectID: b.VolumeID, subjectName: b.Volume, path: "/backups", verdict: fine}
			at := b.StartedAt
			if b.FinishedAt != nil {
				at = *b.FinishedAt
			}
			switch {
			case b.State == store.BackupRunning:
				f.verdict = unknown
			case b.State == store.BackupFailed:
				f.verdict, f.since, f.facts.Variant = holds, at, "failed"
				f.title = "The backup of " + b.Volume + " failed"
				f.detail = sentence(b.Error) + ". Until one works, there's no newer copy to restore. Backups says more: fix it, then back it up again."
			case b.CopyError != "":
				f.verdict, f.since, f.facts.Variant, f.facts.Where = holds, at, "copy", b.Destination
				f.title = fmt.Sprintf("The backup of %s didn't reach %s", b.Volume, b.Destination)
				f.detail = sentence(b.CopyError) + ". It's on its server alone, so it goes if the server does. Backups says more."
			}
			out = append(out, f)
		}
		return nil
	})
	return out, err
}

// drillsFailed follows each volume's newest backup that a restore drill checked, on
// each server (ADR 0068): it holds while that backup's drill failed, and resolves once
// a backup at least as new passes. Deleting the failed backup alone doesn't resolve
// it, and a drill of an older backup, or one that wasn't checked, says nothing here.
func (s *signals) drillsFailed(ctx context.Context) ([]finding, error) {
	var out []finding
	err := s.e.DB.View(ctx, func(tx *store.Tx) error {
		volumes, err := tx.ListKind(ctx, "volume")
		if err != nil {
			return err
		}
		for i := range volumes {
			drills, err := tx.ListDrills(ctx, store.DrillFilter{VolumeID: volumes[i].ID})
			if err != nil {
				return err
			}
			if len(drills) == 0 {
				continue
			}
			type judged struct {
				drill  store.Drill
				backup time.Time // when what it checked was taken
				volume string
			}
			newest := map[string]judged{} // by server
			for k := range drills {
				d := &drills[k]
				if d.Result != store.DrillPassed && d.Result != store.DrillFailed {
					continue
				}
				b, err := tx.GetBackup(ctx, d.BackupID)
				switch {
				case errors.Is(err, store.ErrNotFound):
					// A failed backup that's gone still counts, until a newer one passes.
					if d.Result != store.DrillFailed {
						continue
					}
					b = store.Backup{StartedAt: d.StartedAt, Volume: volumes[i].Name}
				case err != nil:
					return err
				}
				had, ok := newest[d.ServerID]
				// Drills come newest first: of two of one backup, the newest stands.
				if !ok || b.StartedAt.After(had.backup) {
					newest[d.ServerID] = judged{drill: *d, backup: b.StartedAt, volume: cmp.Or(b.Volume, volumes[i].Name)}
				}
			}
			for server := range newest {
				j := newest[server]
				f := finding{thing: "drill:" + volumes[i].ID + ":" + server, serverID: server, subjectID: volumes[i].ID, subjectName: j.volume, path: "/backups", verdict: fine}
				if j.drill.Result == store.DrillFailed {
					at := j.drill.StartedAt
					if j.drill.FinishedAt != nil {
						at = *j.drill.FinishedAt
					}
					f.verdict, f.since = holds, at
					f.title = "The newest checked backup of " + j.volume + " doesn't restore"
					f.detail = sentence(j.drill.Why) + ". A restore of it would fail the same way. Backups says more: fix what it says, then back it up and check it again."
				}
				out = append(out, f)
			}
		}
		return nil
	})
	return out, err
}

func (s *signals) backupsLate(ctx context.Context) ([]finding, error) {
	var out []finding
	err := s.e.DB.View(ctx, func(tx *store.Tx) error {
		policies, err := tx.ListKind(ctx, string(backuppolicy.Kind))
		if err != nil {
			return err
		}
		for i := range policies {
			p := &policies[i]
			decoded, err := s.e.Keeper.Decode(ctx, tx, store.InRevision(p.ID, p.Kind, p.Revision), p.Spec)
			if err != nil {
				return err
			}
			spec := decoded.(*resourcev1.BackupPolicy)
			rev, err := tx.GetRevision(ctx, p.ID, p.Revision)
			if err != nil {
				return err
			}
			since := rev.CreatedAt
			list, err := tx.ListBackups(ctx, store.BackupFilter{PolicyID: p.ID, Limit: 1})
			if err != nil {
				return err
			}
			f := finding{thing: "bpl:" + p.ID, subjectID: p.ID, subjectName: p.Name, path: "/backups", verdict: fine}
			if len(list) > 0 {
				f.serverID = list[0].ServerID
				if list[0].State == store.BackupRunning {
					out = append(out, f) // it started
					continue
				}
				if list[0].StartedAt.After(since) {
					since = list[0].StartedAt
				}
			}
			due := backuppolicy.Next(spec, since)
			if s.now.After(due) {
				f.verdict, f.since = holds, due
				f.title = fmt.Sprintf("The backup of %s is late", spec.GetVolume())
				zone, err := time.LoadLocation(spec.GetTimeZone())
				if err != nil {
					zone = time.UTC
				}
				f.detail = fmt.Sprintf("It was due at %s, and hasn't started. Its server may be away, or a run stuck. Backups says more.", due.In(zone).Format("15:04 MST, 2 Jan"))
			}
			out = append(out, f)
		}
		return nil
	})
	return out, err
}

func (s *signals) changesFailed(ctx context.Context, r rule) ([]finding, error) {
	var out []finding
	err := s.e.DB.View(ctx, func(tx *store.Tx) error {
		after := s.e.started
		if b, err := tx.GetSetting(ctx, settingChangesTold); err == nil {
			if ms, err := strconv.ParseInt(string(b), 10, 64); err == nil {
				after = time.UnixMilli(ms)
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if floor := s.now.Add(-changesBack); after.Before(floor) {
			after = floor
		}
		events, err := tx.EventsSince(ctx, "change.failed", after, 100)
		if err != nil {
			return err
		}
		for i := range events {
			ev := &events[i]
			s.lastChange = ev.At
			thing := "chg:" + ev.Subject
			told, err := tx.AlertTold(ctx, r.id, thing)
			if err != nil {
				return err
			}
			if told {
				continue
			}
			out = append(out, finding{
				thing: thing, verdict: holds, since: ev.At, subjectID: ev.Subject, subjectName: ev.Detail, path: "/changes/" + ev.Subject,
				title:  "A change failed: " + ev.Detail,
				detail: "What ran before goes on running, as Dockwell rolls a failed change back. Activity says what failed: fix it, and plan it again.",
			})
		}
		return nil
	})
	return out, err
}

// sentence ends a message without its own full stop, so another can follow.
func sentence(s string) string {
	if s == "" {
		return "It didn't say why"
	}
	for s != "" && (s[len(s)-1] == '.' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
