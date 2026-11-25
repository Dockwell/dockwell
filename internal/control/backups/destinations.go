// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/backup/s3"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/control/updates"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// VirtualHostBackupsSince is the first release whose agent reads virtual_host
// from a backup destination's values map (#831).
const VirtualHostBackupsSince = "v0.3.1"

// beforeVirtualHostBackups reports a released agent version older than
// VirtualHostBackupsSince. Dev or unreleased builds without a release semver are
// treated as current, matching connections.beforeReads.
func beforeVirtualHostBackups(version string) bool {
	v := strings.TrimPrefix(version, "v")
	if v == "" || strings.ContainsAny(v, "-+") || strings.Count(v, ".") != 2 {
		return false
	}
	return updates.CompareVersions("v"+v, VirtualHostBackupsSince) < 0
}

func virtualHostTooOld(server string) error {
	msg := fmt.Sprintf("Update the agent on %s, which sends path style only", server)
	return errors.New(msg)
}

func checkVirtualHostAgent(s *agentapi.Session, serverName string, destination map[string]string) error {
	if destination["virtual_host"] == "true" && beforeVirtualHostBackups(s.Version()) {
		return authapi.ReasonError(connect.CodeFailedPrecondition, virtualHostTooOld(cmp.Or(serverName, s.Name())), "agent_too_old")
	}
	return nil
}

// destinations reads the destinations, by name, with what signs in to each. What it
// returns goes no further than a call to an agent, sealed.
func (h *Handler) destinations(ctx context.Context, tx *store.Tx) (map[string]*resourcev1.BackupDestination, map[string]string, error) {
	stored, err := tx.ListKind(ctx, string(backuppolicy.DestinationKind))
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]*resourcev1.BackupDestination, len(stored))
	ids := make(map[string]string, len(stored))
	for i := range stored {
		r := &stored[i]
		spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return nil, nil, fmt.Errorf("can't read the backup destination %s: %w", r.Name, err)
		}
		out[r.Name], ids[r.Name] = spec.(*resourcev1.BackupDestination), r.ID
	}
	return out, ids, nil
}

// resolveConnection reads and decodes the S3 connection a backup destination names.
func (h *Handler) resolveConnection(ctx context.Context, tx *store.Tx, name string) (store.Resource, *resourcev1.ConnectionSpec, error) {
	org, err := tx.Organization(ctx)
	if err != nil {
		return store.Resource{}, nil, err
	}
	r, err := tx.GetResource(ctx, string(connection.Kind), org.ID, name)
	if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
		return store.Resource{}, nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("the backup destination uses connection %s, which isn't there", name), "not_found")
	}
	if err != nil {
		return store.Resource{}, nil, err
	}
	spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
	if err != nil {
		return store.Resource{}, nil, fmt.Errorf("can't read the connection %s: %w", name, err)
	}
	c, ok := spec.(*resourcev1.ConnectionSpec)
	if !ok || c.GetKind() != resourcev1.LinkKind_LINK_KIND_S3 || c.GetS3() == nil {
		return store.Resource{}, nil, authapi.Invalid(fmt.Errorf("the connection %s isn't S3 storage", name))
	}
	return r, c, nil
}

// destination reads one destination, as a call carries it, resolving a referenced
// S3 connection's sealed keys fresh. An empty name is none.
func (h *Handler) destination(ctx context.Context, name string) (map[string]string, error) {
	if name == "" {
		return nil, nil
	}
	var values map[string]string
	err := h.db.View(ctx, func(tx *store.Tx) error {
		org, err := tx.Organization(ctx)
		if err != nil {
			return err
		}
		r, err := tx.GetResource(ctx, string(backuppolicy.DestinationKind), org.ID, name)
		if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
			return authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no backup destination %s. List them to find its name", name), "not_found")
		}
		if err != nil {
			return err
		}
		spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return fmt.Errorf("can't read the backup destination %s: %w", name, err)
		}
		d := spec.(*resourcev1.BackupDestination)
		if d.GetConnection() != "" {
			_, c, err := h.resolveConnection(ctx, tx, d.GetConnection())
			if err != nil {
				return err
			}
			s3c := c.GetS3()
			if err := backuppolicy.CheckTransport(s3c.GetEndpoint(), s3c.GetBucket(), s3c.GetPathStyle()); err != nil {
				return authapi.Invalid(fmt.Errorf("the connection %s can't be used for backups: its endpoint %s. Change %s to HTTPS", d.GetConnection(), err.Error(), d.GetConnection()))
			}
			values = map[string]string{
				"endpoint": s3c.GetEndpoint(), "region": s3c.GetRegion(), "bucket": s3c.GetBucket(), "prefix": d.GetPrefix(),
				"access_key": s3c.GetAccessKeyId(), "secret_key": s3c.GetSecretKey(), "passphrase": d.GetPassphrase(),
			}
			if !s3c.GetPathStyle() {
				values["virtual_host"] = "true"
			}
			return nil
		}
		values = map[string]string{
			"endpoint": d.GetEndpoint(), "region": d.GetRegion(), "bucket": d.GetBucket(), "prefix": d.GetPrefix(),
			"access_key": d.GetAccessKey(), "secret_key": d.GetSecretKey(), "passphrase": d.GetPassphrase(),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return values, nil
}

// sealedWith says which passphrase a destination seals with, as a keyed hash that says
// nothing of it without the install's key. A destination can go and come back under
// its name with another passphrase, and this tells that from the one a copy needs. It
// stays off the API, as a value anyone could compare would confirm a guess.
func (h *Handler) sealedWith(passphrase string) string {
	return hex.EncodeToString(h.keeper.Digest("backup passphrase", []byte(passphrase)))
}

// ListBackupDestinations returns the destinations, and never what signs in to one.
func (h *Handler) ListBackupDestinations(ctx context.Context, _ *connect.Request[dockwellv1.ListBackupDestinationsRequest]) (*connect.Response[dockwellv1.ListBackupDestinationsResponse], error) {
	out := &dockwellv1.ListBackupDestinationsResponse{}
	admin, mine := callerProjects(ctx)
	err := h.db.View(ctx, func(tx *store.Tx) error {
		found, ids, err := h.destinations(ctx, tx)
		if err != nil {
			return err
		}
		since, err := revisedAt(ctx, tx)
		if err != nil {
			return err
		}
		all, err := tx.ListBackups(ctx, store.BackupFilter{})
		if err != nil {
			return err
		}
		names := make([]string, 0, len(found))
		for name := range found {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			d := found[name]
			info := &dockwellv1.BackupDestinationInfo{Id: ids[name], Name: name, Endpoint: d.GetEndpoint(), Bucket: d.GetBucket(), Prefix: d.GetPrefix()}
			changedAt := since[name]
			if d.GetConnection() != "" {
				cr, c, err := h.resolveConnection(ctx, tx, d.GetConnection())
				if err != nil {
					return err
				}
				info.Endpoint, info.Bucket = c.GetS3().GetEndpoint(), c.GetS3().GetBucket()
				if admin || reachesAny(c, mine) {
					info.Connection = d.GetConnection()
				}
				connRev, err := tx.GetRevision(ctx, cr.ID, cr.Revision)
				if err != nil {
					return err
				}
				changedAt = max2(changedAt, connRev.CreatedAt)
			}
			for i := range all {
				if b := &all[i]; b.Destination == name && b.CopyKey != "" {
					info.Backups++
					info.UsedBytes += b.CopyBytes
				}
			}
			s := standing(all, name, changedAt)
			if s.upload != nil {
				info.LastUpload = timestamppb.New(*s.upload)
			}
			if s.failure != nil {
				info.LastFailure, info.LastFailureError = timestamppb.New(*s.failure), s.why
			}
			out.Destinations = append(out.Destinations, info)
		}
		return nil
	})
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	return connect.NewResponse(out), nil
}

var (
	connectionBucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	connectionRegionRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
)

// convertDestinationS3 canonicalizes an inline backup destination's endpoint
// (stripping unused query/fragment/trailing slash) and validates that its bucket
// and region can be represented on a connection, returning a conversion-specific
// error naming the field and fix when not (#831, M4).
func convertDestinationS3(destName string, d *resourcev1.BackupDestination) (*resourcev1.ConnectionS3, error) {
	ep, err := backuppolicy.CanonicalEndpoint(d.GetEndpoint())
	if err != nil {
		return nil, authapi.Invalid(fmt.Errorf("backup destination %s can't be converted because its endpoint %w. Edit the destination's endpoint, or create a new S3 connection by hand", destName, err))
	}
	inlinePath, err := (&s3.Bucket{Endpoint: d.GetEndpoint(), Name: d.GetBucket()}).SignedPath("probe")
	if err != nil {
		return nil, authapi.Invalid(fmt.Errorf("backup destination %s can't be converted because its endpoint %w. Keep this destination inline, or create a new S3 connection by hand", destName, err))
	}
	connPath, err := (&s3.Bucket{Endpoint: ep, Name: d.GetBucket()}).SignedPath("probe")
	if err != nil {
		return nil, authapi.Invalid(fmt.Errorf("backup destination %s can't be converted because its endpoint %w. Keep this destination inline, or create a new S3 connection by hand", destName, err))
	}
	if inlinePath != connPath {
		return nil, authapi.Invalid(fmt.Errorf("backup destination %s can't be converted because its endpoint %q signs requests at %s, while the connection endpoint %q would sign %s. Keep this destination inline, or add a new S3 connection for future backups", destName, d.GetEndpoint(), inlinePath, ep, connPath))
	}
	if !connectionBucketRe.MatchString(d.GetBucket()) || strings.Contains(d.GetBucket(), "..") {
		return nil, authapi.Invalid(fmt.Errorf("backup destination %s can't be converted because its bucket %q isn't a valid connection bucket name (3 to 63 lowercase letters, digits, dots and dashes). Create a new S3 connection with a standard bucket name, or keep this destination as it is", destName, d.GetBucket()))
	}
	region := cmp.Or(d.GetRegion(), "auto")
	if !connectionRegionRe.MatchString(region) {
		return nil, authapi.Invalid(fmt.Errorf("backup destination %s can't be converted because its region %q isn't a valid connection region (lowercase letters, digits and dashes, such as auto or eu-central-1). Update the destination's region, or create a new S3 connection by hand", destName, d.GetRegion()))
	}
	return &resourcev1.ConnectionS3{
		Endpoint:    ep,
		Region:      region,
		Bucket:      d.GetBucket(),
		AccessKeyId: d.GetAccessKey(),
		SecretKey:   d.GetSecretKey(),
		PathStyle:   true,
	}, nil
}

// UseDestinationAsConnection plans moving an inline backup destination's endpoint,
// bucket and keys into a new install-only S3 connection and pointing the destination
// at it, in one atomic change (ADR 0099).
func (h *Handler) UseDestinationAsConnection(ctx context.Context, req *connect.Request[dockwellv1.UseDestinationAsConnectionRequest]) (*connect.Response[dockwellv1.UseDestinationAsConnectionResponse], error) {
	if h.changes == nil || h.engine == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this control plane plans no changes"))
	}
	destName := strings.TrimSpace(req.Msg.GetDestination())
	if destName == "" {
		return nil, authapi.Invalid(errors.New("say which backup destination to move, by name"))
	}
	explicitName := strings.TrimSpace(req.Msg.GetConnection())
	if explicitName != "" {
		if err := (connection.Type{}).CheckName(explicitName); err != nil {
			return nil, authapi.Invalid(err)
		}
	}
	var (
		orgID   string
		d       *resourcev1.BackupDestination
		destRev int
		taken   map[string]bool
	)
	err := h.db.View(ctx, func(tx *store.Tx) error {
		org, err := tx.Organization(ctx)
		if err != nil {
			return err
		}
		orgID = org.ID
		r, err := tx.GetResource(ctx, string(backuppolicy.DestinationKind), org.ID, destName)
		if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
			return authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no backup destination %s. List them to find its name", destName), "not_found")
		}
		if err != nil {
			return err
		}
		spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return fmt.Errorf("can't read the backup destination %s: %w", destName, err)
		}
		d = spec.(*resourcev1.BackupDestination)
		if d.GetConnection() != "" {
			return authapi.ReasonError(connect.CodeFailedPrecondition,
				fmt.Errorf("backup destination %s already uses the connection %s. Open %s on Connections to manage its storage keys", destName, d.GetConnection(), d.GetConnection()),
				"already_connected")
		}
		destRev = r.Revision
		existing, err := tx.ListKind(ctx, string(connection.Kind))
		if err != nil {
			return err
		}
		taken = make(map[string]bool, len(existing))
		for i := range existing {
			taken[existing[i].Name] = true
		}
		return nil
	})
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	s3Spec, err := convertDestinationS3(destName, d)
	if err != nil {
		return nil, err
	}
	summary := cmp.Or(strings.TrimSpace(req.Msg.GetSummary()), fmt.Sprintf("Use %s as a connection", destName))
	displayName := strings.TrimSpace(req.Msg.GetDisplayName())

	const maxAttempts = 16
	var connName string
	change, err := h.changes.Planned(ctx, func(who revision.By) (*revision.Plan, error) {
		for attempt := range maxAttempts {
			if explicitName != "" {
				connName = explicitName
			} else {
				connName = freeConnectionName(destName, taken)
			}
			connSpec := &resourcev1.ConnectionSpec{
				DisplayName: displayName,
				Kind:        resourcev1.LinkKind_LINK_KIND_S3,
				S3:          s3Spec,
				Reach:       resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED,
			}
			destSpec := &resourcev1.BackupDestination{
				Connection: connName,
				Prefix:     d.GetPrefix(),
			}
			p, err := h.engine.Propose(ctx, revision.Proposal{
				Summary: summary,
				Actor:   who.Actor,
				Via:     who.Via,
				Edits: []revision.Edit{
					{
						Ref:        resource.Ref{Kind: connection.Kind, Scope: orgID, Name: connName},
						Spec:       connSpec,
						MustCreate: true,
					},
					{
						Ref:  resource.Ref{Kind: backuppolicy.DestinationKind, Scope: orgID, Name: destName},
						Spec: destSpec,
						Base: destRev,
					},
				},
			})
			if errors.Is(err, revision.ErrAlreadyExists) && explicitName == "" && attempt+1 < maxAttempts {
				taken[connName] = true
				continue
			}
			return p, err
		}
		return nil, revision.ErrAlreadyExists
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.UseDestinationAsConnectionResponse{Change: change, Connection: connName}), nil
}

// freeConnectionName picks base if unused, or base-2, base-3, ... within the 63-byte label limit.
func freeConnectionName(base string, taken map[string]bool) string {
	if !taken[base] {
		return base
	}
	for n := 2; ; n++ {
		suffix := "-" + strconv.Itoa(n)
		stem := base
		if len(stem)+len(suffix) > 63 {
			stem = strings.TrimRight(stem[:63-len(suffix)], "-")
		}
		candidate := stem + suffix
		if !taken[candidate] {
			return candidate
		}
	}
}

// callerProjects says whether the caller is an install admin, and otherwise the
// projects they hold a grant on; nil for every project, from a grant on the install.
func callerProjects(ctx context.Context) (admin bool, projects map[string]bool) {
	c, ok := place.CallerFrom(ctx)
	if !ok {
		return true, nil
	}
	if store.RoleRank(place.Holds(c.Grants, place.Install)) >= store.RoleRank(store.RoleAdmin) {
		return true, nil
	}
	projects = map[string]bool{}
	for _, g := range c.Grants {
		if g.ProjectID == store.Install {
			return false, nil
		}
		projects[g.ProjectID] = true
	}
	return false, projects
}

// reachesAny says whether a connection reaches one of projects; nil projects is any
// project, for a grant on the whole install.
func reachesAny(c *resourcev1.ConnectionSpec, projects map[string]bool) bool {
	if c.GetReach() == resourcev1.ConnectionReach_CONNECTION_REACH_EVERY {
		return true
	}
	if projects == nil {
		return c.GetReach() == resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS
	}
	for _, p := range c.GetProjects() {
		if projects[p] && c.GetReach() == resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS {
			return true
		}
	}
	return false
}

// revisedAt says when each destination was last made or changed, by name.
func revisedAt(ctx context.Context, tx *store.Tx) (map[string]time.Time, error) {
	stored, err := tx.ListKind(ctx, string(backuppolicy.DestinationKind))
	if err != nil {
		return nil, err
	}
	out := make(map[string]time.Time, len(stored))
	for i := range stored {
		rev, err := tx.GetRevision(ctx, stored[i].ID, stored[i].Revision)
		if err != nil {
			return nil, err
		}
		out[stored[i].Name] = rev.CreatedAt
	}
	return out, nil
}

// destinationStanding is how a destination stands by its backups: when the
// last one got there, and when the last one didn't, and why.
type destinationStanding struct {
	upload, failure *time.Time
	why             string
}

// standing reads how the destination name stands from all backups. Each is
// the latest by when it finished, so backups that overlap, one starting before
// another and finishing after it, are put in the order they ended. A backup
// that began before since, when the destination was last made or changed,
// doesn't count either way: it read the destination as it was then, such as
// one removed and connected again, or pointed at another bucket.
func standing(all []store.Backup, name string, since time.Time) destinationStanding {
	var out destinationStanding
	for i := range all {
		b := &all[i]
		if b.Destination != name || b.StartedAt.Before(since) {
			continue
		}
		switch {
		case b.CopyKey != "" && b.FinishedAt != nil:
			if out.upload == nil || b.FinishedAt.After(*out.upload) {
				at := *b.FinishedAt
				out.upload = &at
			}
		case b.CopyKey == "" && b.CopyError != "":
			at := b.StartedAt
			if b.FinishedAt != nil {
				at = *b.FinishedAt
			}
			if out.failure == nil || at.After(*out.failure) {
				out.failure, out.why = &at, b.CopyError
			}
		}
	}
	return out
}

// FetchBackup starts bringing a backup back from its destination.
func (h *Handler) FetchBackup(ctx context.Context, req *connect.Request[dockwellv1.FetchBackupRequest]) (*connect.Response[dockwellv1.FetchBackupResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("sign in first"))
	}
	client, _ := authapi.ClientFrom(ctx)
	b, err := h.backup(ctx, req.Msg.GetId())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if b.CopyKey == "" {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s isn't on a destination, so there's nothing to fetch", b.ID), "not_stored")
	}
	var server store.Server
	if b.WebsiteID != "" {
		target, werr := h.websiteOf(ctx, b.WebsiteID, req.Msg.GetServer(), store.RoleDeveloper)
		if werr != nil {
			return nil, h.fail(ctx, werr)
		}
		server = target.server
	} else {
		_, server, err = h.volumeOf(ctx, b.VolumeID, req.Msg.GetServer())
		if err != nil {
			return nil, h.fail(ctx, err)
		}
	}
	s, err := h.session(server)
	if err != nil {
		return nil, err
	}
	destination, err := h.destination(ctx, b.Destination)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if err := checkVirtualHostAgent(s, server.Name, destination); err != nil {
		clear(destination)
		return nil, err
	}
	surface := client.Surface(p)
	actor := p.Actor()
	if err := h.db.Update(ctx, func(tx *store.Tx) error { return tx.SetBackupFetchError(ctx, b.ID, "") }); err != nil {
		return nil, h.fail(ctx, err)
	}
	// From here, not from the server's first word, so a page can tell it's under way.
	h.mu.Lock()
	h.progress[b.ID] = "Fetching " + b.Volume
	h.mu.Unlock()
	h.wg.Go(func() { h.fetch(s, b, server.ID, destination, actor, surface) })
	out, err := h.proto(ctx, b)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.FetchBackupResponse{Backup: out}), nil
}

// fetch brings a backup back, and says so.
func (h *Handler) fetch(s *agentapi.Session, b store.Backup, server string, destination map[string]string, actor, surface string) {
	defer clear(destination)
	ctx := resource.WithProgress(h.base, func(message string) {
		h.mu.Lock()
		h.progress[b.ID] = message
		h.mu.Unlock()
	})
	defer func() {
		h.mu.Lock()
		delete(h.progress, b.ID)
		h.mu.Unlock()
	}()
	ref := backupRef(b)
	_, err := s.FetchBackup(ctx, &agentv1.FetchBackupCall{Id: b.ID, Volume: resourcepb.Ref(ref), Key: b.CopyKey, Sha256: b.SHA256, HostPath: b.WebsitePath}, destination)
	record := context.WithoutCancel(ctx)
	action, told := ActionFetched, Told{ID: b.ID, Volume: b.Volume, Destination: b.Destination}
	if err != nil {
		action = ActionFetchFailed
		told.Error, _ = resource.Explain(err)
		if b.SealedWith != "" && b.SealedWith != h.sealedWith(destination["passphrase"]) {
			told.Error += ". This copy was sealed with the passphrase " + b.Destination + " had before, so today's can't open it. Add " + b.Destination + " again with that passphrase, or open the file with dockwell open-backup"
		}
	}
	if werr := h.say(record, &b, action, told, actor, surface, func(tx *store.Tx) error {
		if err != nil {
			return tx.SetBackupFetchError(record, b.ID, told.Error)
		}
		return tx.MoveBackup(record, b.ID, server)
	}); werr != nil {
		h.logger.ErrorContext(record, "can't record that a backup was fetched", "backup", b.ID, "error", werr.Error())
	}
}
