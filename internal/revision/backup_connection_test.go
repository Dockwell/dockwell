// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/secrets/secretstest"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestBackupDestinationConnectionRules(t *testing.T) {
	storetest.Each(t, testBackupDestinationConnectionRules)
}

func testBackupDestinationConnectionRules(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	logger := slog.New(slog.DiscardHandler)
	if _, err := Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(
		scope.ProjectType{}, scope.EnvironmentType{},
		connection.Type{}, link.Type{}, variable.Type{}, variable.SecretType{},
		backuppolicy.Type{}, backuppolicy.Destination{},
	)
	e := New(db, kinds, fleet{}, secretstest.Keeper(db, kinds), logger)
	tr := tree(t, db)
	org := tr.Organization.ID
	proj := tr.Projects[0].ID
	env := tr.Projects[0].Environments[0].ID

	s3Conn := func(bucket string, shared bool, reach resourcev1.ConnectionReach) *resourcev1.ConnectionSpec {
		return &resourcev1.ConnectionSpec{
			DisplayName: "Backups",
			Kind:        resourcev1.LinkKind_LINK_KIND_S3,
			Shared:      shared,
			Reach:       reach,
			S3: &resourcev1.ConnectionS3{
				Endpoint: "https://s3.example.com", Region: "us-east-1", Bucket: bucket,
				AccessKeyId: "AKIAOLD", SecretKey: "old-secret-key", PathStyle: true,
			},
		}
	}
	destSpec := func(conn string) *resourcev1.BackupDestination {
		return &resourcev1.BackupDestination{Connection: conn, Prefix: "backups", Passphrase: "a-long-enough-passphrase"}
	}
	connRef := func(name string) resource.Ref { return resource.Ref{Kind: connection.Kind, Scope: org, Name: name} }
	destRef := func(name string) resource.Ref {
		return resource.Ref{Kind: backuppolicy.DestinationKind, Scope: org, Name: name}
	}
	linkRef := func(name string) resource.Ref { return resource.Ref{Kind: link.Kind, Scope: env, Name: name} }

	// 1. Missing connection is refused with install wording, not environment wording.
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Missing connection",
		Edits:   []Edit{{Ref: destRef("missing-dest"), Spec: destSpec("nope")}},
	}); err == nil || !strings.Contains(err.Error(), "isn't on this install") {
		t.Fatalf("missing connection = %v", err)
	}

	// 2. SMTP connection is refused for a backup destination.
	apply(t, e, propose(t, e, "Add SMTP", Edit{
		Ref: connRef("mail"),
		Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS},
		},
	}))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "SMTP dest",
		Edits:   []Edit{{Ref: destRef("mail-dest"), Spec: destSpec("mail")}},
	}); err == nil || !strings.Contains(err.Error(), "isn't S3 storage") {
		t.Fatalf("SMTP connection = %v", err)
	}

	// 3. Rule 1: shared connection or connection with app link is refused for backups.
	apply(t, e, propose(t, e, "Add shared S3", Edit{Ref: connRef("shared-s3"), Spec: s3Conn("shared-bkt", true, resourcev1.ConnectionReach_CONNECTION_REACH_EVERY)}))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Backup on shared",
		Edits:   []Edit{{Ref: destRef("shared-dest"), Spec: destSpec("shared-s3")}},
	}); err == nil || !strings.Contains(err.Error(), "shared connection") {
		t.Fatalf("backup on shared connection = %v", err)
	}

	linkSets := []*resourcev1.LinkSet{{Name: "S3_BUCKET", Part: resourcev1.LinkPart_LINK_PART_BUCKET}}
	apply(t, e, propose(t, e, "Add app S3 and link",
		Edit{Ref: connRef("app-s3"), Spec: s3Conn("app-bkt", false, resourcev1.ConnectionReach_CONNECTION_REACH_EVERY)},
		Edit{Ref: linkRef("storage"), Spec: &resourcev1.Link{Label: "Storage", Connection: "app-s3", Kind: resourcev1.LinkKind_LINK_KIND_S3, Prefix: "site/", Sets: linkSets}},
	))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Backup on linked app connection",
		Edits:   []Edit{{Ref: destRef("app-dest"), Spec: destSpec("app-s3")}},
	}); err == nil || !strings.Contains(err.Error(), "app's key could delete the backups") {
		t.Fatalf("backup on linked connection = %v", err)
	}

	// 4. Creating an install-only connection + destination in ONE change (destination listed first)
	// succeeds via Referrer ordering.
	p := propose(t, e, "Add backup connection and destination",
		Edit{Ref: destRef("r2"), Spec: destSpec("bkp-s3")},
		Edit{Ref: connRef("bkp-s3"), Spec: s3Conn("backups-bkt", false, resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED)},
	)
	apply(t, e, p)

	// 5. Rule 1: linking an app to a backup connection is refused.
	// When the connection doesn't reach the project, it still reads as unusable (no name leak).
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Link unreached backup conn",
		Edits:   []Edit{{Ref: linkRef("bkp-link"), Spec: &resourcev1.Link{Label: "Storage", Connection: "bkp-s3", Kind: resourcev1.LinkKind_LINK_KIND_S3, Sets: linkSets}}},
	}); err == nil || !strings.Contains(err.Error(), "isn't one this project can use") {
		t.Fatalf("link to unreached backup conn = %v", err)
	}
	// Even if an admin widens project reach on bkp-s3, linking an app is still refused in its own words.
	widened := s3Conn("backups-bkt", false, resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS)
	widened.Projects = []string{proj}
	widened.S3.AccessKeyId, widened.S3.SecretKey = "", ""
	apply(t, e, propose(t, e, "Widen reach", Edit{Ref: connRef("bkp-s3"), Spec: widened}))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Link reached backup conn",
		Edits:   []Edit{{Ref: linkRef("bkp-link"), Spec: &resourcev1.Link{Label: "Storage", Connection: "bkp-s3", Kind: resourcev1.LinkKind_LINK_KIND_S3, Sets: linkSets}}},
	}); err == nil || !strings.Contains(err.Error(), "used for backups") {
		t.Fatalf("link to reached backup conn = %v", err)
	}

	// 6. Rule 1: marking bkp-s3 shared while r2 uses it is refused.
	shareEdit := s3Conn("backups-bkt", true, resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED)
	shareEdit.S3.AccessKeyId, shareEdit.S3.SecretKey = "", ""
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Share backup conn",
		Edits:   []Edit{{Ref: connRef("bkp-s3"), Spec: shareEdit}},
	}); err == nil || !strings.Contains(err.Error(), "can't be marked shared") {
		t.Fatalf("marking backup conn shared = %v", err)
	}

	// 7. Deleting or changing kind of bkp-s3 while r2 uses it is refused.
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Delete backup conn",
		Edits:   []Edit{{Ref: connRef("bkp-s3"), Spec: nil}},
	}); err == nil || !strings.Contains(err.Error(), "still uses the connection") {
		t.Fatalf("deleting backup conn = %v", err)
	}
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Retype backup conn",
		Edits: []Edit{{Ref: connRef("bkp-s3"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS},
		}}},
	}); err == nil || !strings.Contains(err.Error(), "another kind of connection") {
		t.Fatalf("retyping backup conn = %v", err)
	}

	// 8. Rule 3: before any backups exist (or with only a failed copy), changing bucket is allowed.
	now := time.Now().UTC()
	failedCopy := store.Backup{
		ID: id.New(id.Backup), EnvironmentID: env, Volume: "data", VolumeID: "vol_test1", ServerID: "srv_1",
		State: store.BackupDone, Actor: SystemActor, StartedAt: now, FinishedAt: &now,
		Destination: "r2", CopyError: "NoSuchBucket",
	}
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		if err := tx.CreateBackup(t.Context(), failedCopy); err != nil {
			return err
		}
		return tx.FinishBackup(t.Context(), failedCopy)
	}); err != nil {
		t.Fatal(err)
	}
	apply(t, e, propose(t, e, "Fix bucket name before any stored backup", Edit{
		Ref:  connRef("bkp-s3"),
		Spec: s3Conn("backups-fixed", false, resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED),
	}))

	// Once a backup is stored on r2, changing endpoint, region, bucket, or path_style is refused.
	storedBackup := store.Backup{
		ID: id.New(id.Backup), EnvironmentID: env, Volume: "data", VolumeID: "vol_test1", ServerID: "srv_1",
		State: store.BackupDone, Actor: SystemActor, StartedAt: now, FinishedAt: &now,
		Destination: "r2", CopyKey: "backups/env/data/bkp.tar.gz.sealed", CopyBytes: 512,
	}
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		if err := tx.CreateBackup(t.Context(), storedBackup); err != nil {
			return err
		}
		if err := tx.FinishBackup(t.Context(), storedBackup); err != nil {
			return err
		}
		return tx.SetBackupCopy(t.Context(), storedBackup)
	}); err != nil {
		t.Fatal(err)
	}
	for field, mutate := range map[string]func(*resourcev1.ConnectionS3){
		"endpoint":   func(s *resourcev1.ConnectionS3) { s.Endpoint = "https://other.example.com" },
		"region":     func(s *resourcev1.ConnectionS3) { s.Region = "eu-central-1" },
		"bucket":     func(s *resourcev1.ConnectionS3) { s.Bucket = "another-bkt" },
		"path_style": func(s *resourcev1.ConnectionS3) { s.PathStyle = false },
	} {
		changed := s3Conn("backups-fixed", false, resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED)
		mutate(changed.S3)
		if _, err := e.Propose(t.Context(), Proposal{
			Summary: "Move " + field,
			Edits:   []Edit{{Ref: connRef("bkp-s3"), Spec: changed}},
		}); err == nil || !strings.Contains(err.Error(), "can't change its endpoint, region, bucket or path style") {
			t.Errorf("changing %s while destination has backups = %v", field, err)
		}
	}

	// Rotating keys on bkp-s3 while r2 has backups IS allowed.
	rotated := s3Conn("backups-fixed", false, resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED)
	rotated.S3.AccessKeyId, rotated.S3.SecretKey = "AKIANEW", "new-secret-key"
	apply(t, e, propose(t, e, "Rotate backup keys", Edit{Ref: connRef("bkp-s3"), Spec: rotated}))

	// 9. Deleting destination + connection together in one change succeeds.
	apply(t, e, propose(t, e, "Remove destination and connection",
		Edit{Ref: connRef("bkp-s3"), Spec: nil},
		Edit{Ref: destRef("r2"), Spec: nil},
	))

	// 10. Updating an inline destination onto a connection keeps its passphrase.
	apply(t, e, propose(t, e, "Add inline dest", Edit{
		Ref: destRef("legacy"),
		Spec: &resourcev1.BackupDestination{
			Endpoint: "https://s3.example.com", Bucket: "legacy-bkt",
			AccessKey: "AKIAOLD", SecretKey: "old-secret", Passphrase: "kept-passphrase-long",
		},
	}))
	movePlan := propose(t, e, "Move legacy to connection",
		Edit{Ref: connRef("legacy-conn"), Spec: s3Conn("legacy-bkt", false, resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED)},
		Edit{Ref: destRef("legacy"), Spec: &resourcev1.BackupDestination{Connection: "legacy-conn"}},
	)
	hasKept := false
	for i := range movePlan.Notes {
		if movePlan.Notes[i].Reason == "kept_passphrase" {
			hasKept = true
		}
	}
	if !hasKept {
		t.Errorf("moving inline dest to connection notes = %v, want kept_passphrase", movePlan.Notes)
	}
	apply(t, e, movePlan)
}

func TestBackupDestinationH1DetachAndRotate(t *testing.T) {
	storetest.Each(t, testBackupDestinationH1DetachAndRotate)
}

func testBackupDestinationH1DetachAndRotate(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	logger := slog.New(slog.DiscardHandler)
	if _, err := Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(
		scope.ProjectType{}, scope.EnvironmentType{},
		connection.Type{}, link.Type{}, variable.Type{}, variable.SecretType{},
		backuppolicy.Type{}, backuppolicy.Destination{},
	)
	e := New(db, kinds, fleet{}, secretstest.Keeper(db, kinds), logger)
	tr := tree(t, db)
	org := tr.Organization.ID
	env := tr.Projects[0].Environments[0].ID

	connRef := func(name string) resource.Ref { return resource.Ref{Kind: connection.Kind, Scope: org, Name: name} }
	destRef := func(name string) resource.Ref {
		return resource.Ref{Kind: backuppolicy.DestinationKind, Scope: org, Name: name}
	}
	linkRef := func(name string) resource.Ref { return resource.Ref{Kind: link.Kind, Scope: env, Name: name} }
	varRef := func(name string) resource.Ref {
		return resource.Ref{Kind: variable.VariableKind, Scope: env, Name: name}
	}
	secRef := func(name string) resource.Ref { return resource.Ref{Kind: variable.SecretKind, Scope: env, Name: name} }

	connSpec := func(ak, sk string) *resourcev1.ConnectionSpec {
		return &resourcev1.ConnectionSpec{
			Kind:  resourcev1.LinkKind_LINK_KIND_S3,
			Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			S3: &resourcev1.ConnectionS3{
				Endpoint: "https://s3.example.com", Region: "us-east-1", Bucket: "app-bkt",
				AccessKeyId: ak, SecretKey: sk, PathStyle: true,
			},
		}
	}
	linkSpec := &resourcev1.Link{
		Label: "Storage", Connection: "uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Prefix: "site/",
		Sets: []*resourcev1.LinkSet{{Name: "S3_BUCKET", Part: resourcev1.LinkPart_LINK_PART_BUCKET}},
	}
	destSpec := &resourcev1.BackupDestination{Connection: "uploads", Prefix: "backups", Passphrase: "a-long-enough-passphrase"}

	apply(t, e, propose(t, e, "Add app connection and link",
		Edit{Ref: connRef("uploads"), Spec: connSpec("AKIAAPP1", "app-secret-1")},
		Edit{Ref: linkRef("storage"), Spec: linkSpec},
	))

	// 1. Detach and adopt in ONE plan without rotating keys is refused.
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Detach and adopt without rotation",
		Edits: []Edit{
			{Ref: linkRef("storage"), Spec: nil},
			{Ref: destRef("r2"), Spec: destSpec},
		},
	}); err == nil || !strings.Contains(err.Error(), appKeyRefusal) {
		t.Fatalf("detach+adopt in one plan = %v, want %q", err, appKeyRefusal)
	}

	// 2. Detach in plan 1 (copying endpoint, bucket and key secret as DisconnectStorage does),
	// then adopt in plan 2 without rotating keys is refused.
	apply(t, e, propose(t, e, "Disconnect storage keeping values",
		Edit{Ref: linkRef("storage"), Spec: nil},
		Edit{Ref: varRef("S3_ENDPOINT"), Spec: &resourcev1.Variable{Value: "https://s3.example.com"}},
		Edit{Ref: varRef("S3_BUCKET"), Spec: &resourcev1.Variable{Value: "app-bkt"}},
		Edit{Ref: secRef("S3_SECRET_KEY"), Spec: &resourcev1.Secret{Value: "app-secret-1"}},
	))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Adopt after detach without rotation",
		Edits:   []Edit{{Ref: destRef("r2"), Spec: destSpec}},
	}); err == nil || !strings.Contains(err.Error(), appKeyRefusal) {
		t.Fatalf("adopt after detach without rotation = %v, want %q", err, appKeyRefusal)
	}

	// 3. Rotate the connection's keys to a dedicated backup key, then adopt succeeds.
	rotPlan := propose(t, e, "Rotate to backup-only key",
		Edit{Ref: connRef("uploads"), Spec: connSpec("AKIABKP2", "backup-secret-2")},
	)
	apply(t, e, rotPlan)
	apply(t, e, propose(t, e, "Adopt after rotation",
		Edit{Ref: destRef("r2"), Spec: destSpec},
	))

	// N1: undoing the rotation (returning to the app-held pair) or editing keys back
	// to the app-held pair while r2 uses the connection is refused with the H1 words,
	// while rotating to a fresh backup-only key still succeeds.
	if _, err := e.Undo(t.Context(), rotPlan.Change, By{Actor: SystemActor}); err == nil || !strings.Contains(err.Error(), appKeyRefusal) {
		t.Fatalf("undo rotation back to app-held key = %v, want %q", err, appKeyRefusal)
	}
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Rotate back to app-held key",
		Edits:   []Edit{{Ref: connRef("uploads"), Spec: connSpec("AKIAAPP1", "app-secret-1")}},
	}); err == nil || !strings.Contains(err.Error(), appKeyRefusal) {
		t.Fatalf("rotate back to app-held key = %v, want %q", err, appKeyRefusal)
	}
	apply(t, e, propose(t, e, "Rotate to a second fresh backup key",
		Edit{Ref: connRef("uploads"), Spec: connSpec("AKIABKP3", "backup-secret-3")},
	))

	// 4. A connection that was never linked but became held by an app's own values
	// after its keys were set is also refused until rotated.
	apply(t, e, propose(t, e, "Add unlinked S3 connection",
		Edit{Ref: connRef("manual"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3: &resourcev1.ConnectionS3{
				Endpoint: "https://manual.example.com", Region: "auto", Bucket: "manual-bkt",
				AccessKeyId: "AKIAMAN1", SecretKey: "manual-secret-1", PathStyle: true,
			},
		}},
	))
	apply(t, e, propose(t, e, "App sets own values naming manual bucket",
		Edit{Ref: varRef("MAN_EP"), Spec: &resourcev1.Variable{Value: "https://manual.example.com"}},
		Edit{Ref: varRef("MAN_BKT"), Spec: &resourcev1.Variable{Value: "manual-bkt"}},
	))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Adopt held connection without rotation",
		Edits:   []Edit{{Ref: destRef("man-dest"), Spec: &resourcev1.BackupDestination{Connection: "manual", Passphrase: "a-long-enough-passphrase"}}},
	}); err == nil || !strings.Contains(err.Error(), appKeyRefusal) {
		t.Fatalf("adopt held connection without rotation = %v", err)
	}
	apply(t, e, propose(t, e, "Rotate manual connection keys",
		Edit{Ref: connRef("manual"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3: &resourcev1.ConnectionS3{
				Endpoint: "https://manual.example.com", Region: "auto", Bucket: "manual-bkt",
				AccessKeyId: "AKIAMAN2", SecretKey: "manual-secret-2", PathStyle: true,
			},
		}},
	))
	apply(t, e, propose(t, e, "Adopt held connection after rotation",
		Edit{Ref: destRef("man-dest"), Spec: &resourcev1.BackupDestination{Connection: "manual", Passphrase: "a-long-enough-passphrase"}},
	))
}

func TestBackupDestinationH2AndH6(t *testing.T) {
	storetest.Each(t, testBackupDestinationH2AndH6)
}

func testBackupDestinationH2AndH6(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	logger := slog.New(slog.DiscardHandler)
	if _, err := Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(
		scope.ProjectType{}, scope.EnvironmentType{},
		connection.Type{}, link.Type{}, variable.Type{}, variable.SecretType{},
		backuppolicy.Type{}, backuppolicy.Destination{},
	)
	e := New(db, kinds, fleet{}, secretstest.Keeper(db, kinds), logger)
	tr := tree(t, db)
	org := tr.Organization.ID
	env := tr.Projects[0].Environments[0].ID

	connRef := func(name string) resource.Ref { return resource.Ref{Kind: connection.Kind, Scope: org, Name: name} }
	destRef := func(name string) resource.Ref {
		return resource.Ref{Kind: backuppolicy.DestinationKind, Scope: org, Name: name}
	}

	// H6: plain HTTP away from loopback is refused when adopting a connection,
	// when editing a backup connection, and for virtual-host on loopback.
	apply(t, e, propose(t, e, "Add HTTP NAS connection",
		Edit{Ref: connRef("nas-http"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3: &resourcev1.ConnectionS3{
				Endpoint: "http://nas.internal:9000", Region: "auto", Bucket: "nas-bkt",
				AccessKeyId: "AKIA1", SecretKey: "secret-1", PathStyle: true,
			},
		}},
		Edit{Ref: connRef("loop-vh"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3: &resourcev1.ConnectionS3{
				Endpoint: "http://127.0.0.1:9000", Region: "auto", Bucket: "loop-bkt",
				AccessKeyId: "AKIA2", SecretKey: "secret-2", PathStyle: false,
			},
		}},
	))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Adopt HTTP NAS",
		Edits:   []Edit{{Ref: destRef("nas-dest"), Spec: &resourcev1.BackupDestination{Connection: "nas-http", Passphrase: "a-long-enough-passphrase"}}},
	}); err == nil || !strings.Contains(err.Error(), "nas-http") || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("adopt HTTP NAS = %v", err)
	}
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Adopt loopback virtual-host HTTP",
		Edits:   []Edit{{Ref: destRef("loop-dest"), Spec: &resourcev1.BackupDestination{Connection: "loop-vh", Passphrase: "a-long-enough-passphrase"}}},
	}); err == nil || !strings.Contains(err.Error(), "loop-vh") || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("adopt loopback virtual-host HTTP = %v", err)
	}

	// Create two HTTPS backup connections and a destination pointing at conn-a.
	apply(t, e, propose(t, e, "Add HTTPS connections and destination",
		Edit{Ref: connRef("conn-a"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3: &resourcev1.ConnectionS3{
				Endpoint: "https://a.example.com", Region: "us-east-1", Bucket: "bucket-a",
				AccessKeyId: "AKIAA", SecretKey: "secret-a", PathStyle: true,
			},
		}},
		Edit{Ref: connRef("conn-b"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3: &resourcev1.ConnectionS3{
				Endpoint: "https://b.example.com", Region: "us-east-1", Bucket: "bucket-b",
				AccessKeyId: "AKIAB", SecretKey: "secret-b", PathStyle: true,
			},
		}},
		Edit{Ref: destRef("r2"), Spec: &resourcev1.BackupDestination{Connection: "conn-a", Passphrase: "a-long-enough-passphrase"}},
	))

	// H6: editing conn-a to plain HTTP while r2 uses it is refused naming conn-a.
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Downgrade conn-a to HTTP",
		Edits: []Edit{{Ref: connRef("conn-a"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3: &resourcev1.ConnectionS3{
				Endpoint: "http://nas.internal:9000", Region: "us-east-1", Bucket: "bucket-a",
				AccessKeyId: "AKIAA", SecretKey: "secret-a", PathStyle: true,
			},
		}}},
	}); err == nil || !strings.Contains(err.Error(), "conn-a") || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("downgrade conn-a to HTTP = %v", err)
	}

	// Record a stored backup on r2.
	now := time.Now().UTC()
	stored := store.Backup{
		ID: id.New(id.Backup), EnvironmentID: env, Volume: "data", VolumeID: "vol_h2", ServerID: "srv_1",
		State: store.BackupDone, Actor: SystemActor, StartedAt: now, FinishedAt: &now,
		Destination: "r2", CopyKey: "env/data/bkp.tar.gz.sealed", CopyBytes: 256,
	}
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		if err := tx.CreateBackup(t.Context(), stored); err != nil {
			return err
		}
		if err := tx.FinishBackup(t.Context(), stored); err != nil {
			return err
		}
		return tx.SetBackupCopy(t.Context(), stored)
	}); err != nil {
		t.Fatal(err)
	}

	// H2: repointing r2 to conn-b (different bucket) is refused.
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Repoint r2 to conn-b",
		Edits:   []Edit{{Ref: destRef("r2"), Spec: &resourcev1.BackupDestination{Connection: "conn-b"}}},
	}); err == nil || !strings.Contains(err.Error(), "can't change its endpoint, region, bucket or path style") {
		t.Fatalf("repoint r2 to conn-b = %v", err)
	}

	// H2: switching r2 back to inline at a different bucket is refused.
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Repoint r2 to inline other bucket",
		Edits: []Edit{{Ref: destRef("r2"), Spec: &resourcev1.BackupDestination{
			Endpoint: "https://other.example.com", Region: "us-east-1", Bucket: "other-bucket",
			AccessKey: "AKIAX", SecretKey: "secret-x",
		}}},
	}); err == nil || !strings.Contains(err.Error(), "can't change its endpoint, region, bucket or path style") {
		t.Fatalf("repoint r2 to inline other bucket = %v", err)
	}

	// H2: switching r2 back to inline at the SAME target (as Undo of conversion does) is allowed.
	sameInline := propose(t, e, "Return r2 to inline same target",
		Edit{Ref: destRef("r2"), Spec: &resourcev1.BackupDestination{
			Endpoint: "https://a.example.com", Region: "us-east-1", Bucket: "bucket-a",
			AccessKey: "AKIAA", SecretKey: "secret-a",
		}},
	)
	apply(t, e, sameInline)

	// H2: deleting r2 and recreating r2 at a different target is refused, while recreating
	// at the same target (ADR 0033 passphrase rotation) stays allowed.
	apply(t, e, propose(t, e, "Delete r2", Edit{Ref: destRef("r2"), Spec: nil}))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Recreate r2 at conn-b",
		Edits:   []Edit{{Ref: destRef("r2"), Spec: &resourcev1.BackupDestination{Connection: "conn-b", Passphrase: "a-new-long-passphrase"}}},
	}); err == nil || !strings.Contains(err.Error(), "can't change its endpoint, region, bucket or path style") {
		t.Fatalf("recreate r2 at conn-b = %v", err)
	}
	apply(t, e, propose(t, e, "Recreate r2 at same target conn-a",
		Edit{Ref: destRef("r2"), Spec: &resourcev1.BackupDestination{Connection: "conn-a", Passphrase: "a-new-long-passphrase"}},
	))

	// H2 same-change retirement: deleting r2 and moving conn-a to another bucket in one
	// change must not let a later recreate of r2 adopt the moved bucket, while restoring
	// conn-a to bucket-a alongside recreating r2 in one change is allowed.
	apply(t, e, propose(t, e, "Retire r2 and move conn-a in one change",
		Edit{Ref: destRef("r2"), Spec: nil},
		Edit{Ref: connRef("conn-a"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3: &resourcev1.ConnectionS3{
				Endpoint: "https://a.example.com", Region: "us-east-1", Bucket: "bucket-moved",
				AccessKeyId: "AKIAA", SecretKey: "secret-a", PathStyle: true,
			},
		}},
	))
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Recreate r2 on moved conn-a",
		Edits:   []Edit{{Ref: destRef("r2"), Spec: &resourcev1.BackupDestination{Connection: "conn-a", Passphrase: "a-third-long-passphrase"}}},
	}); err == nil || !strings.Contains(err.Error(), "can't change its endpoint, region, bucket or path style") {
		t.Fatalf("recreate r2 on conn-a moved in retirement change = %v", err)
	}
	apply(t, e, propose(t, e, "Restore r2 and conn-a original bucket together",
		Edit{Ref: connRef("conn-a"), Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3: &resourcev1.ConnectionS3{
				Endpoint: "https://a.example.com", Region: "us-east-1", Bucket: "bucket-a",
				AccessKeyId: "AKIAA", SecretKey: "secret-a", PathStyle: true,
			},
		}},
		Edit{Ref: destRef("r2"), Spec: &resourcev1.BackupDestination{Connection: "conn-a", Passphrase: "a-third-long-passphrase"}},
	))

	// H2 fail-closed: when backups remain on a destination name whose earlier target
	// cannot be found, recreating that name is refused.
	orphan := store.Backup{
		ID: id.New(id.Backup), EnvironmentID: env, Volume: "data", VolumeID: "vol_h2_orphan", ServerID: "srv_1",
		State: store.BackupDone, Actor: SystemActor, StartedAt: now, FinishedAt: &now,
		Destination: "lost-history", CopyKey: "env/data/orphan.tar.gz.sealed", CopyBytes: 128,
	}
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		if err := tx.CreateBackup(t.Context(), orphan); err != nil {
			return err
		}
		if err := tx.FinishBackup(t.Context(), orphan); err != nil {
			return err
		}
		return tx.SetBackupCopy(t.Context(), orphan)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "Recreate destination with unknown prior target",
		Edits:   []Edit{{Ref: destRef("lost-history"), Spec: &resourcev1.BackupDestination{Connection: "conn-b", Passphrase: "a-long-enough-passphrase"}}},
	}); err == nil || !strings.Contains(err.Error(), "can't verify its earlier storage location") {
		t.Fatalf("recreate destination with unknown prior target = %v", err)
	}

	// M1: MustCreate refuses when a live connection already holds the name.
	if _, err := e.Propose(t.Context(), Proposal{
		Summary: "MustCreate existing connection",
		Edits: []Edit{{
			Ref:        connRef("conn-a"),
			MustCreate: true,
			Spec: &resourcev1.ConnectionSpec{
				Kind: resourcev1.LinkKind_LINK_KIND_S3,
				S3: &resourcev1.ConnectionS3{
					Endpoint: "https://a.example.com", Region: "us-east-1", Bucket: "bucket-a",
					AccessKeyId: "AKIAA", SecretKey: "secret-a", PathStyle: true,
				},
			},
		}},
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("MustCreate on existing connection = %v, want ErrAlreadyExists", err)
	}
}

// L1: every connection/backup refusal must also hold at Apply time when a valid
// plan stays pending while the other side changes before Apply runs.
func TestBackupDestinationConnectionRulesAtApply(t *testing.T) {
	storetest.Each(t, testBackupDestinationConnectionRulesAtApply)
}

func testBackupDestinationConnectionRulesAtApply(t *testing.T, backend store.Backend) {
	db, _ := storetest.Open(t, backend)
	logger := slog.New(slog.DiscardHandler)
	if _, err := Setup(t.Context(), db, logger); err != nil {
		t.Fatal(err)
	}
	kinds := resource.NewRegistry(
		scope.ProjectType{}, scope.EnvironmentType{},
		connection.Type{}, link.Type{}, variable.Type{}, variable.SecretType{},
		backuppolicy.Type{}, backuppolicy.Destination{},
	)
	e := New(db, kinds, fleet{}, secretstest.Keeper(db, kinds), logger)
	tr := tree(t, db)
	org := tr.Organization.ID
	env := tr.Projects[0].Environments[0].ID

	connRef := func(name string) resource.Ref { return resource.Ref{Kind: connection.Kind, Scope: org, Name: name} }
	destRef := func(name string) resource.Ref {
		return resource.Ref{Kind: backuppolicy.DestinationKind, Scope: org, Name: name}
	}
	linkRef := func(name string) resource.Ref { return resource.Ref{Kind: link.Kind, Scope: env, Name: name} }
	s3Conn := func(bucket string) *resourcev1.ConnectionSpec {
		return &resourcev1.ConnectionSpec{
			Kind:  resourcev1.LinkKind_LINK_KIND_S3,
			Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			S3: &resourcev1.ConnectionS3{
				Endpoint: "https://s3.example.com", Region: "us-east-1", Bucket: bucket,
				AccessKeyId: "AKIA1", SecretKey: "secret-1", PathStyle: true,
			},
		}
	}
	destOn := func(conn string) *resourcev1.BackupDestination {
		return &resourcev1.BackupDestination{Connection: conn, Prefix: "backups", Passphrase: "a-long-enough-passphrase"}
	}
	linkSets := []*resourcev1.LinkSet{{Name: "S3_BUCKET", Part: resourcev1.LinkPart_LINK_PART_BUCKET}}
	mustFailApply := func(label string, p *Plan, wantSub string) {
		t.Helper()
		err := e.Apply(t.Context(), p.Change)
		if err == nil || !strings.Contains(err.Error(), wantSub) {
			t.Fatalf("%s at apply time = %v, want %q", label, err, wantSub)
		}
	}
	loadSpec := func(ref resource.Ref) any {
		t.Helper()
		var out any
		if err := db.View(t.Context(), func(tx *store.Tx) error {
			it, err := e.load(t.Context(), tx, ref)
			out = it.current
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Set up free connections c0..c8, an existing link l0 -> c0, and destinations d6 -> c6, d7 -> c7.
	apply(t, e, propose(t, e, "Seed connections",
		Edit{Ref: connRef("c0"), Spec: s3Conn("bkt-0")},
		Edit{Ref: connRef("c1"), Spec: s3Conn("bkt-1")},
		Edit{Ref: connRef("c2"), Spec: s3Conn("bkt-2")},
		Edit{Ref: connRef("c3"), Spec: s3Conn("bkt-3")},
		Edit{Ref: connRef("c4"), Spec: s3Conn("bkt-4")},
		Edit{Ref: connRef("c5"), Spec: s3Conn("bkt-5")},
		Edit{Ref: connRef("c6"), Spec: s3Conn("bkt-6")},
		Edit{Ref: connRef("c7"), Spec: s3Conn("bkt-7")},
		Edit{Ref: connRef("c8"), Spec: s3Conn("bkt-8")},
		Edit{Ref: linkRef("l0"), Spec: &resourcev1.Link{Label: "Storage", Connection: "c0", Kind: resourcev1.LinkKind_LINK_KIND_S3, Sets: linkSets}},
		Edit{Ref: destRef("d6"), Spec: destOn("c6")},
		Edit{Ref: destRef("d7"), Spec: destOn("c7")},
	))

	// 1. Link create proposed while c1 is free, then d1 claims c1 before apply.
	linkCreatePlan := propose(t, e, "Create link to c1", Edit{
		Ref:  linkRef("l1"),
		Spec: &resourcev1.Link{Label: "Storage", Connection: "c1", Kind: resourcev1.LinkKind_LINK_KIND_S3, Sets: linkSets},
	})
	apply(t, e, propose(t, e, "Assign c1 to backups", Edit{Ref: destRef("d1"), Spec: destOn("c1")}))
	mustFailApply("link create", linkCreatePlan, "used for backups")
	if loadSpec(linkRef("l1")) != nil {
		t.Error("conflicting link l1 landed after apply refusal")
	}

	// 2. Link retarget proposed while c2 is free, then d2 claims c2 before apply.
	retargetPlan := propose(t, e, "Retarget l0 to c2", Edit{
		Ref:  linkRef("l0"),
		Spec: &resourcev1.Link{Label: "Storage", Connection: "c2", Kind: resourcev1.LinkKind_LINK_KIND_S3, Sets: linkSets},
	})
	apply(t, e, propose(t, e, "Assign c2 to backups", Edit{Ref: destRef("d2"), Spec: destOn("c2")}))
	mustFailApply("link retarget", retargetPlan, "used for backups")
	if got := loadSpec(linkRef("l0")).(*resourcev1.Link); got.GetConnection() != "c0" {
		t.Errorf("l0 moved to %q after apply refusal", got.GetConnection())
	}

	// 3. Sharing proposed while c3 is free, then d3 claims c3 before apply.
	shareSpec := s3Conn("bkt-3")
	shareSpec.Shared = true
	shareSpec.S3.AccessKeyId, shareSpec.S3.SecretKey = "", ""
	sharePlan := propose(t, e, "Share c3", Edit{Ref: connRef("c3"), Spec: shareSpec})
	apply(t, e, propose(t, e, "Assign c3 to backups", Edit{Ref: destRef("d3"), Spec: destOn("c3")}))
	mustFailApply("sharing", sharePlan, "can't be marked shared")
	if got := loadSpec(connRef("c3")).(*resourcev1.ConnectionSpec); got.GetShared() {
		t.Error("c3 became shared after apply refusal")
	}

	// 4. Delete proposed while c4 is free, then d4 claims c4 before apply.
	delPlan := propose(t, e, "Delete c4", Edit{Ref: connRef("c4"), Spec: nil})
	apply(t, e, propose(t, e, "Assign c4 to backups", Edit{Ref: destRef("d4"), Spec: destOn("c4")}))
	mustFailApply("delete", delPlan, "still uses the connection")
	if loadSpec(connRef("c4")) == nil {
		t.Error("c4 was deleted after apply refusal")
	}

	// 5. Kind change proposed while c5 is free, then d5 claims c5 before apply.
	retypePlan := propose(t, e, "Retype c5 to SMTP", Edit{
		Ref: connRef("c5"),
		Spec: &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_SMTP,
			Smtp: &resourcev1.ConnectionSmtp{Host: "smtp.example.com", Port: 587, Security: resourcev1.SmtpSecurity_SMTP_SECURITY_STARTTLS},
		},
	})
	apply(t, e, propose(t, e, "Assign c5 to backups", Edit{Ref: destRef("d5"), Spec: destOn("c5")}))
	mustFailApply("kind change", retypePlan, "another kind of connection")
	if got := loadSpec(connRef("c5")).(*resourcev1.ConnectionSpec); got.GetKind() != resourcev1.LinkKind_LINK_KIND_S3 {
		t.Errorf("c5 kind changed to %v after apply refusal", got.GetKind())
	}

	// 6. Connection target move proposed while d6 has no backups, then a backup starts on d6 before apply.
	moveConnPlan := propose(t, e, "Move c6 bucket", Edit{Ref: connRef("c6"), Spec: s3Conn("bkt-6-moved")})
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		return tx.CreateBackup(t.Context(), store.Backup{
			ID: id.New(id.Backup), EnvironmentID: env, Volume: "data", VolumeID: "vol_l1a", ServerID: "srv_1",
			State: store.BackupRunning, Actor: SystemActor, StartedAt: time.Now().UTC(), Destination: "d6",
		})
	}); err != nil {
		t.Fatal(err)
	}
	mustFailApply("connection target move", moveConnPlan, "can't change its endpoint, region, bucket or path style")
	if got := loadSpec(connRef("c6")).(*resourcev1.ConnectionSpec); got.GetS3().GetBucket() != "bkt-6" {
		t.Errorf("c6 bucket changed to %q after apply refusal", got.GetS3().GetBucket())
	}

	// 7. Destination repoint proposed while d7 has no backups, then a backup starts on d7 before apply.
	repointDestPlan := propose(t, e, "Repoint d7 to c8", Edit{
		Ref:  destRef("d7"),
		Spec: &resourcev1.BackupDestination{Connection: "c8", Prefix: "backups"},
	})
	if err := db.Update(t.Context(), func(tx *store.Tx) error {
		return tx.CreateBackup(t.Context(), store.Backup{
			ID: id.New(id.Backup), EnvironmentID: env, Volume: "data", VolumeID: "vol_l1b", ServerID: "srv_1",
			State: store.BackupRunning, Actor: SystemActor, StartedAt: time.Now().UTC(), Destination: "d7",
		})
	}); err != nil {
		t.Fatal(err)
	}
	mustFailApply("destination repoint", repointDestPlan, "can't change its endpoint, region, bucket or path style")
	if got := loadSpec(destRef("d7")).(*resourcev1.BackupDestination); got.GetConnection() != "c7" {
		t.Errorf("d7 moved to %q after apply refusal", got.GetConnection())
	}
}
