// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/control/backups"
	"github.com/dockwell/dockwell/internal/control/connections"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestBackupsThroughAnS3Connection(t *testing.T) {
	storetest.Each(t, testBackupsThroughAnS3Connection)
}

func testBackupsThroughAnS3Connection(t *testing.T, backend store.Backend) {
	srv := s3test.New(t)
	srv.Buckets = []string{"backups"}
	wasClient := backups.StorageClient
	backups.StorageClient = srv.Client()
	t.Cleanup(func() { backups.StorageClient = wasClient })

	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	bc := backupClient(b, b.origin)
	volID, dir := webWithData(t, e, c)
	writeData(t, dir, "orders.db", "the orders")
	f.lastDocker.backups.HTTP = srv.Client()

	// Create an install-only S3 connection and a backup destination referencing it.
	connSpec := func(keyID, secret string) *resourcev1.ConnectionSpec {
		return &resourcev1.ConnectionSpec{
			DisplayName: "Offsite R2",
			Kind:        resourcev1.LinkKind_LINK_KIND_S3,
			Reach:       resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED,
			S3: &resourcev1.ConnectionS3{
				Endpoint: srv.URL(), Region: "auto", Bucket: "backups",
				AccessKeyId: keyID, SecretKey: secret, PathStyle: true,
			},
		}
	}
	apply(t, c, propose(t, c, "Add backup storage",
		connectionEdit("r2-conn", connSpec("AKIAOLD", "old-secret-value")),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "r2"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{
				Connection: "r2-conn", Prefix: "backups", Passphrase: "a-long-passphrase-for-sealing",
			}}},
		},
	))

	// TestBackupDestination works with a saved connection and leaves no test object behind.
	tested, err := bc.TestBackupDestination(t.Context(), connect.NewRequest(&dockwellv1.TestBackupDestinationRequest{
		Destination: &resourcev1.BackupDestination{Connection: "r2-conn", Prefix: "dw/"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !tested.Msg.GetWrote() || tested.Msg.GetProblem() != dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_UNSPECIFIED {
		t.Fatalf("TestBackupDestination(connection) = %v", tested.Msg)
	}
	if len(srv.Objects()) != 0 {
		t.Fatalf("TestBackupDestination left objects: %v", srv.Objects())
	}

	// Take an on-demand backup and verify it lands sealed under prefix in s3test.
	made, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volID, Destination: "r2"}))
	if err != nil {
		t.Fatal(err)
	}
	b1 := waitCopy(t, bc, made.Msg.GetBackup().GetId())
	if b1.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED {
		t.Fatalf("first backup copy = %v (%s)", b1.GetCopy(), b1.GetCopyError())
	}
	foundUnderPrefix := false
	for path := range srv.Objects() {
		if strings.HasPrefix(path, "/backups/backups/") && strings.HasSuffix(path, ".sealed") {
			foundUnderPrefix = true
		}
	}
	if !foundUnderPrefix {
		t.Fatalf("no sealed object under prefix; objects = %v", srv.Objects())
	}

	// ListBackupDestinations resolves endpoint, bucket and connection name from the connection.
	listed, err := bc.ListBackupDestinations(t.Context(), connect.NewRequest(&dockwellv1.ListBackupDestinationsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if ds := listed.Msg.GetDestinations(); len(ds) != 1 || ds[0].GetConnection() != "r2-conn" ||
		ds[0].GetEndpoint() != srv.URL() || ds[0].GetBucket() != "backups" || ds[0].GetBackups() != 1 || ds[0].GetLastUpload() == nil {
		t.Fatalf("ListBackupDestinations = %v", ds)
	}

	// Rule 3: once a backup is stored on r2, changing endpoint, region, bucket or path_style on r2-conn is refused.
	for name, mutate := range map[string]func(*resourcev1.ConnectionS3){
		"endpoint":   func(s *resourcev1.ConnectionS3) { s.Endpoint = "https://other.example.com" },
		"region":     func(s *resourcev1.ConnectionS3) { s.Region = "eu-west-1" },
		"bucket":     func(s *resourcev1.ConnectionS3) { s.Bucket = "other-bucket" },
		"path_style": func(s *resourcev1.ConnectionS3) { s.PathStyle = false },
	} {
		bad := connSpec("AKIAOLD", "old-secret-value")
		mutate(bad.S3)
		_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
			Summary: "Move " + name,
			Edits:   []*dockwellv1.Edit{connectionEdit("r2-conn", bad)},
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "can't change its endpoint, region, bucket or path style") {
			t.Errorf("changing %s on connection with backups = %v", name, err)
		}
	}

	// Force a failed copy, verify failure standing, then rotate keys on the connection (no destination edit).
	srv.Refuse, srv.RefuseOnly = "AccessDenied", http.MethodPost
	failed, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volID, Destination: "r2"}))
	if err != nil {
		t.Fatal(err)
	}
	bf := waitCopy(t, bc, failed.Msg.GetBackup().GetId())
	if bf.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_FAILED {
		t.Fatalf("expected failed copy, got %v", bf.GetCopy())
	}
	listed, err = bc.ListBackupDestinations(t.Context(), connect.NewRequest(&dockwellv1.ListBackupDestinationsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if listed.Msg.GetDestinations()[0].GetLastFailure() == nil {
		t.Fatal("expected last_failure after refused upload")
	}

	// Rotate keys on the connection after a short tick so rev.CreatedAt > bf.StartedAt.
	time.Sleep(10 * time.Millisecond)
	srv.Refuse, srv.RefuseOnly = "", ""
	apply(t, c, propose(t, c, "Rotate R2 keys", connectionEdit("r2-conn", connSpec("AKIANEW", "new-secret-value"))))

	// Standing resets after the connection's revision.
	listed, err = bc.ListBackupDestinations(t.Context(), connect.NewRequest(&dockwellv1.ListBackupDestinationsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if ds := listed.Msg.GetDestinations(); ds[0].GetLastFailure() != nil {
		t.Errorf("expected failure standing to reset after key rotation, got %v", ds[0].GetLastFailure())
	}

	// Next upload uses the rotated key without any destination edit.
	made2, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volID, Destination: "r2"}))
	if err != nil {
		t.Fatal(err)
	}
	b2 := waitCopy(t, bc, made2.Msg.GetBackup().GetId())
	if b2.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED {
		t.Fatalf("second backup copy = %v (%s)", b2.GetCopy(), b2.GetCopyError())
	}
	// Scheduled backup also resolves the connection and uploads under its prefix.
	sched := policyEdit(5)
	sched.Spec.GetBackupPolicy().Destination = "r2"
	apply(t, c, propose(t, c, "Back up data each night", sched))
	day(t, f.server.backups, time.Now(), 1)
	eventually(t, "scheduled backup copy stored", func() bool {
		for _, one := range backupsOf(t, bc) {
			if one.GetPolicyId() != "" && one.GetCopy() == dockwellv1.BackupCopy_BACKUP_COPY_STORED {
				return true
			}
		}
		return false
	})

	sawOld := slices.ContainsFunc(srv.Authorizations, func(a string) bool { return strings.Contains(a, "Credential=AKIAOLD/") })
	sawNew := slices.ContainsFunc(srv.Authorizations, func(a string) bool { return strings.Contains(a, "Credential=AKIANEW/") })
	if !sawOld || !sawNew {
		t.Errorf("expected both AKIAOLD and AKIANEW in authorizations; sawOld=%v sawNew=%v", sawOld, sawNew)
	}

	// Delete local copy of b1, fetch it back via the rotated connection keys, then delete b1 from both.
	if _, err := bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: b1.GetId(), ServerOnly: true})); err != nil {
		t.Fatal(err)
	}
	if _, err := bc.FetchBackup(t.Context(), connect.NewRequest(&dockwellv1.FetchBackupRequest{Id: b1.GetId()})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "fetched backup back on server", func() bool {
		got, err := bc.GetBackup(t.Context(), connect.NewRequest(&dockwellv1.GetBackupRequest{Id: b1.GetId()}))
		return err == nil && got.Msg.GetBackup().GetOnServer()
	})
	if _, err := bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: b1.GetId()})); err != nil {
		t.Fatal(err)
	}
	for path := range srv.Objects() {
		if strings.Contains(path, b1.GetId()) {
			t.Errorf("DeleteBackup left %s in s3test", path)
		}
	}
}

func TestUseDestinationAsConnection(t *testing.T) {
	storetest.Each(t, testUseDestinationAsConnection)
}

func testUseDestinationAsConnection(t *testing.T, backend store.Backend) {
	s := startSending(t, backend)
	b1 := s.sent(t)
	if b1.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED {
		t.Fatalf("legacy upload = %v (%s)", b1.GetCopy(), b1.GetCopyError())
	}

	// Non-admin is refused UseDestinationAsConnection.
	env := listedEnv(t, s.b)
	proj := envProject(t, s.f, env)
	hold := aMember(t, s.f)
	hold(store.Grant{ProjectID: proj, Role: store.RoleDeveloper})
	_, err := s.bc.UseDestinationAsConnection(t.Context(), connect.NewRequest(&dockwellv1.UseDestinationAsConnectionRequest{Destination: "r2"}))
	needsRole(t, "developer calling UseDestinationAsConnection", err)
	hold(store.Grant{ProjectID: store.Install, Role: store.RoleAdmin})

	// Admin plans UseDestinationAsConnection.
	planned, err := s.bc.UseDestinationAsConnection(t.Context(), connect.NewRequest(&dockwellv1.UseDestinationAsConnectionRequest{
		Destination: "r2",
		DisplayName: "R2 Backups",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if planned.Msg.GetConnection() != "r2" {
		t.Fatalf("chosen connection name = %q, want r2", planned.Msg.GetConnection())
	}
	ch := planned.Msg.GetChange()
	// Keys must never appear in the planned change JSON.
	if raw, _ := protojson.Marshal(ch); strings.Contains(string(raw), "a-secret") || strings.Contains(string(raw), "AKIAEXAMPLE") {
		t.Error("planned UseDestinationAsConnection change leaked credentials")
	}
	// Plan must carry kept_passphrase note.
	hasKept := false
	for _, n := range ch.GetPlan().GetNotes() {
		if n.GetReason() == "kept_passphrase" {
			hasKept = true
		}
	}
	if !hasKept {
		t.Errorf("plan notes = %v, want kept_passphrase", ch.GetPlan().GetNotes())
	}

	applied := apply(t, s.c, ch)
	if applied.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("apply ended %v: %s", applied.GetState(), applied.GetError())
	}

	// Connection is install-only (UNSPECIFIED reach) with path_style=true, and destination points at it.
	cc := dockwellv1connect.NewConnectionServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
	conns, err := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if cl := conns.Msg.GetConnections(); len(cl) != 1 || cl[0].GetName() != "r2" || cl[0].GetDisplayName() != "R2 Backups" ||
		cl[0].GetReach() != resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED || !cl[0].GetPathStyle() {
		t.Fatalf("ListConnections after move = %v", cl)
	}
	dests, err := s.bc.ListBackupDestinations(t.Context(), connect.NewRequest(&dockwellv1.ListBackupDestinationsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if dl := dests.Msg.GetDestinations(); len(dl) != 1 || dl[0].GetConnection() != "r2" || dl[0].GetBucket() != "acme-backups" {
		t.Fatalf("ListBackupDestinations after move = %v", dl)
	}

	// Calling UseDestinationAsConnection a second time is refused as already_connected with next action (L2).
	if _, err := s.bc.UseDestinationAsConnection(t.Context(), connect.NewRequest(&dockwellv1.UseDestinationAsConnectionRequest{Destination: "r2"})); connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "already_connected" || !strings.Contains(err.Error(), "Open r2 on Connections") {
		t.Errorf("second UseDestinationAsConnection = %v", err)
	}

	// Pre-existing backup still opens (sealed_earlier == false) and fetches cleanly.
	got1 := s.get(t, b1.GetId())
	if got1.GetSealedEarlier() {
		t.Error("pre-existing backup marked sealed_earlier after move")
	}
	if _, err := s.bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: b1.GetId(), ServerOnly: true})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.bc.FetchBackup(t.Context(), connect.NewRequest(&dockwellv1.FetchBackupRequest{Id: b1.GetId()})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "pre-existing backup fetched after move", func() bool { return s.get(t, b1.GetId()).GetOnServer() })

	// New backup uploads through the connection.
	b2 := s.sent(t)
	if b2.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED {
		t.Fatalf("new backup after move = %v (%s)", b2.GetCopy(), b2.GetCopyError())
	}

	// UndoChange restores the inline destination and removes the connection cleanly.
	undone, err := s.c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: applied.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if res := apply(t, s.c, undone.Msg.GetChange()); res.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("undo ended %v: %s", res.GetState(), res.GetError())
	}
	conns, err = cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(conns.Msg.GetConnections()) != 0 {
		t.Errorf("connections after undo = %v, want empty", conns.Msg.GetConnections())
	}
	dests, err = s.bc.ListBackupDestinations(t.Context(), connect.NewRequest(&dockwellv1.ListBackupDestinationsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if dl := dests.Msg.GetDestinations(); len(dl) != 1 || dl[0].GetConnection() != "" {
		t.Errorf("destinations after undo = %v", dl)
	}
}

func TestBackupDestinationConnectionVisibilityAndIsolation(t *testing.T) {
	storetest.Each(t, testBackupDestinationConnectionVisibilityAndIsolation)
}

func testBackupDestinationConnectionVisibilityAndIsolation(t *testing.T, backend store.Backend) {
	dir := t.TempDir()
	useCatalogDir(t, dir)
	raw, err := os.ReadFile(filepath.Join("testdata", "ghost-storage.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ghost.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	bc := backupClient(b, b.origin)
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	cat := catalogClient(b)
	env := listedEnv(t, b)
	proj := envProject(t, f, env)

	// Add a service reading S3_BUCKET so ConnectStorage can target it.
	_, _ = webWithData(t, e, c)
	srv := s3test.New(t)

	// Two backup connections: one install-only (unreached), one reaching every project (still backup-only),
	// plus one free app connection reaching every project.
	apply(t, c, propose(t, c, "Add backup and app connections",
		connectionEdit("bkp-private", &resourcev1.ConnectionSpec{
			Kind:  resourcev1.LinkKind_LINK_KIND_S3,
			Reach: resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED,
			S3:    &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "priv-bkt", AccessKeyId: "AKIA1", SecretKey: "secret-1", PathStyle: true},
		}),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "dest-private"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{
				Connection: "bkp-private", Passphrase: "passphrase-one-long-enough",
			}}},
		},
		connectionEdit("bkp-proj", &resourcev1.ConnectionSpec{
			Kind:  resourcev1.LinkKind_LINK_KIND_S3,
			Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			S3:    &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "proj-bkt", AccessKeyId: "AKIA2", SecretKey: "secret-2", PathStyle: true},
		}),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "dest-proj"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{
				Connection: "bkp-proj", Passphrase: "passphrase-two-long-enough",
			}}},
		},
		connectionEdit("app-uploads", &resourcev1.ConnectionSpec{
			Kind:  resourcev1.LinkKind_LINK_KIND_S3,
			Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			S3:    &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "app-bkt", AccessKeyId: "AKIA3", SecretKey: "secret-3", PathStyle: true},
		}),
	))

	// ConnectStorage refuses a backup connection before probing.
	_, err = cc.ConnectStorage(t.Context(), connect.NewRequest(&dockwellv1.ConnectStorageRequest{
		Project: proj, Environment: env, App: "web", Connection: "bkp-proj",
		Sets: map[string]resourcev1.LinkPart{"S3_BUCKET": resourcev1.LinkPart_LINK_PART_BUCKET},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "used for backups") {
		t.Errorf("ConnectStorage on backup connection = %v", err)
	}

	// Catalog install sheet offers app-uploads and never offers bkp-proj (used by a backup destination).
	entry, err := cat.GetCatalogEntry(t.Context(), connect.NewRequest(&dockwellv1.GetCatalogEntryRequest{Id: "ghost"}))
	if err != nil {
		t.Fatal(err)
	}
	var storageChoices []string
	for _, o := range entry.Msg.GetNeeds() {
		if o.GetNeed() == "storage" {
			storageChoices = append(storageChoices, o.GetChoice())
		}
	}
	if slices.Contains(storageChoices, "connection:bkp-proj") || slices.Contains(storageChoices, "connection:bkp-private") {
		t.Errorf("catalog offered a backup connection to an app: %v", storageChoices)
	}
	if !slices.Contains(storageChoices, "connection:app-uploads") {
		t.Errorf("catalog did not offer app-uploads: %v", storageChoices)
	}

	// Admin sees both connections with endpoints and buckets.
	adminListed, err := bc.ListBackupDestinations(t.Context(), connect.NewRequest(&dockwellv1.ListBackupDestinationsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(adminListed.Msg.GetDestinations()) != 2 {
		t.Fatalf("admin listed %v", adminListed.Msg.GetDestinations())
	}
	for _, d := range adminListed.Msg.GetDestinations() {
		if d.GetConnection() == "" || d.GetEndpoint() == "" || d.GetBucket() == "" {
			t.Errorf("admin destination missing fields: %v", d)
		}
	}

	// Non-admin in proj sees connection only on dest-proj (which reaches proj), never on dest-private,
	// and never sees endpoint or bucket on either.
	hold := aMember(t, f)
	hold(store.Grant{ProjectID: proj, Role: store.RoleDeveloper})
	listed, err := bc.ListBackupDestinations(t.Context(), connect.NewRequest(&dockwellv1.ListBackupDestinationsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*dockwellv1.BackupDestinationInfo{}
	for _, d := range listed.Msg.GetDestinations() {
		byName[d.GetName()] = d
	}
	if len(byName) != 2 {
		t.Fatalf("developer listed %v", listed.Msg.GetDestinations())
	}
	if d := byName["dest-private"]; d.GetConnection() != "" || d.GetEndpoint() != "" || d.GetBucket() != "" {
		t.Errorf("dest-private for non-admin = %v", d)
	}
	if d := byName["dest-proj"]; d.GetConnection() != "bkp-proj" || d.GetEndpoint() != "" || d.GetBucket() != "" {
		t.Errorf("dest-proj for non-admin = %v", d)
	}
}

func TestH2RepointingDestinationWithStoredCopyRefused(t *testing.T) {
	storetest.Each(t, testH2RepointingDestinationWithStoredCopyRefused)
}

func testH2RepointingDestinationWithStoredCopyRefused(t *testing.T, backend store.Backend) {
	srv := s3test.New(t)
	srv.Buckets = []string{"bucket-a", "bucket-b"}
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	bc := backupClient(b, b.origin)
	volID, dir := webWithData(t, e, c)
	writeData(t, dir, "orders.db", "the orders")
	f.lastDocker.backups.HTTP = srv.Client()

	apply(t, c, propose(t, c, "Add two connections and destination r2",
		connectionEdit("conn-a", &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3:   &resourcev1.ConnectionS3{Endpoint: srv.URL(), Region: "auto", Bucket: "bucket-a", AccessKeyId: "AKIAA", SecretKey: "secret-a", PathStyle: true},
		}),
		connectionEdit("conn-b", &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3:   &resourcev1.ConnectionS3{Endpoint: srv.URL(), Region: "auto", Bucket: "bucket-b", AccessKeyId: "AKIAB", SecretKey: "secret-b", PathStyle: true},
		}),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "r2"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{
				Connection: "conn-a", Prefix: "backups", Passphrase: "a-long-passphrase-for-sealing",
			}}},
		},
	))

	made, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volID, Destination: "r2"}))
	if err != nil {
		t.Fatal(err)
	}
	b1 := waitCopy(t, bc, made.Msg.GetBackup().GetId())
	if b1.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED {
		t.Fatalf("upload failed: %v (%s)", b1.GetCopy(), b1.GetCopyError())
	}
	// Remove local copy so only the remote copy remains.
	if _, err := bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: b1.GetId(), ServerOnly: true})); err != nil {
		t.Fatal(err)
	}

	// Attempt H2 reproduction: repoint r2 from conn-a to conn-b. Must be refused.
	if _, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Repoint r2 to conn-b",
		Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "r2"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{Connection: "conn-b", Prefix: "backups"}}},
		}},
	})); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "can't change its endpoint, region, bucket or path style") {
		t.Fatalf("repointing r2 to conn-b = %v", err)
	}

	// Original backup still fetches cleanly from bucket-a.
	if _, err := bc.FetchBackup(t.Context(), connect.NewRequest(&dockwellv1.FetchBackupRequest{Id: b1.GetId()})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "original backup fetched", func() bool {
		got, err := bc.GetBackup(t.Context(), connect.NewRequest(&dockwellv1.GetBackupRequest{Id: b1.GetId()}))
		return err == nil && got.Msg.GetBackup().GetOnServer()
	})
}

func TestH6PlainHTTPConnectionRefusedForBackups(t *testing.T) {
	storetest.Each(t, testH6PlainHTTPConnectionRefusedForBackups)
}

func testH6PlainHTTPConnectionRefusedForBackups(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	bc := backupClient(b, b.origin)

	apply(t, c, propose(t, c, "Add plain HTTP NAS connection",
		connectionEdit("nas", &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3:   &resourcev1.ConnectionS3{Endpoint: "http://nas.internal:9000", Region: "auto", Bucket: "backups", AccessKeyId: "AKIA1", SecretKey: "secret-1", PathStyle: true},
		}),
	))

	// TestBackupDestination refuses the HTTP connection and names it.
	if _, err := bc.TestBackupDestination(t.Context(), connect.NewRequest(&dockwellv1.TestBackupDestinationRequest{
		Destination: &resourcev1.BackupDestination{Connection: "nas"},
	})); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "nas") || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("TestBackupDestination(http nas) = %v", err)
	}

	// Proposing a destination naming the HTTP connection is refused and names it.
	if _, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "Use HTTP NAS for backups",
		Edits: []*dockwellv1.Edit{{
			Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "r2"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{Connection: "nas", Passphrase: "a-long-passphrase-for-sealing"}}},
		}},
	})); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "nas") || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("ProposeChange(http nas destination) = %v", err)
	}
}

func TestH3OlderAgentRefusedForVirtualHostBackups(t *testing.T) {
	storetest.Each(t, testH3OlderAgentRefusedForVirtualHostBackups)
}

func testH3OlderAgentRefusedForVirtualHostBackups(t *testing.T, backend store.Backend) {
	srv := s3test.New(t)
	srv.Buckets = []string{"backups"}
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	bc := backupClient(b, b.origin)
	volID, dir := webWithData(t, e, c)
	writeData(t, dir, "orders.db", "the orders")
	f.lastDocker.backups.HTTP = srv.Client()

	// Reconnect hz-1 reporting an older release version (v0.3.0) that ignores virtual_host.
	server := f.lastDocker
	server.stop()
	waitConnected(t, f, server.id.ServerID, false)
	server.hello = func(h *agentv1.Hello) {
		h.Build = &dockwellv1.BuildInfo{Version: "v0.3.0"}
	}
	server.connect(t, f)

	apply(t, c, propose(t, c, "Add virtual-host and path-style backup destinations",
		connectionEdit("aws-vh", &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3:   &resourcev1.ConnectionS3{Endpoint: "https://s3.us-east-1.amazonaws.com", Region: "us-east-1", Bucket: "my-backups", AccessKeyId: "AKIAVH", SecretKey: "secret-vh", PathStyle: false},
		}),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "dest-vh"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{Connection: "aws-vh", Passphrase: "a-long-passphrase-for-sealing"}}},
		},
		connectionEdit("local-ps", &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3:   &resourcev1.ConnectionS3{Endpoint: srv.URL(), Region: "auto", Bucket: "backups", AccessKeyId: "AKIAPS", SecretKey: "secret-ps", PathStyle: true},
		}),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "dest-ps"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{Connection: "local-ps", Passphrase: "a-long-passphrase-for-sealing"}}},
		},
	))

	// Older agent (v0.3.0) is refused for virtual-host backup with exact message.
	_, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volID, Destination: "dest-vh"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "agent_too_old" ||
		!strings.Contains(err.Error(), "Update the agent on hz-1, which sends path style only") {
		t.Fatalf("CreateBackup(virtual_host on v0.3.0) = %v", err)
	}

	// Path-style destination still works on the v0.3.0 agent.
	made, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volID, Destination: "dest-ps"}))
	if err != nil {
		t.Fatal(err)
	}
	b1 := waitCopy(t, bc, made.Msg.GetBackup().GetId())
	if b1.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED {
		t.Fatalf("path-style upload on v0.3.0 = %v (%s)", b1.GetCopy(), b1.GetCopyError())
	}

	// FetchBackup of a virtual-host backup onto the v0.3.0 agent is also refused.
	env := listedEnv(t, b)
	now := time.Now().UTC()
	vhBackup := store.Backup{
		ID: id.New(id.Backup), EnvironmentID: env, Volume: "data", VolumeID: volID, ServerID: f.lastDocker.id.ServerID,
		State: store.BackupDone, Actor: "dockwell", StartedAt: now, FinishedAt: &now,
		Destination: "dest-vh", CopyKey: "env/data/bkp.tar.gz.sealed", CopyBytes: 128, OffServer: true, SHA256: "deadbeef",
	}
	if err := f.store.Update(t.Context(), func(tx *store.Tx) error {
		if err := tx.CreateBackup(t.Context(), vhBackup); err != nil {
			return err
		}
		if err := tx.FinishBackup(t.Context(), vhBackup); err != nil {
			return err
		}
		if err := tx.SetBackupCopy(t.Context(), vhBackup); err != nil {
			return err
		}
		return tx.SetBackupOffServer(t.Context(), vhBackup.ID)
	}); err != nil {
		t.Fatal(err)
	}
	_, err = bc.FetchBackup(t.Context(), connect.NewRequest(&dockwellv1.FetchBackupRequest{Id: vhBackup.ID}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "agent_too_old" ||
		!strings.Contains(err.Error(), "Update the agent on hz-1, which sends path style only") {
		t.Fatalf("FetchBackup(virtual_host on v0.3.0) = %v", err)
	}
}

func TestH5PreflightChecksBackupUseAndFailsClosed(t *testing.T) {
	storetest.Each(t, testH5PreflightChecksBackupUseAndFailsClosed)
}

func testH5PreflightChecksBackupUseAndFailsClosed(t *testing.T, backend store.Backend) {
	srv := s3test.New(t)
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	env := listedEnv(t, b)
	proj := envProject(t, f, env)
	_, _ = webWithData(t, e, c)

	apply(t, c, propose(t, c, "Add backup connection and free connection",
		connectionEdit("bkp-conn", &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Region: "auto", Bucket: "bkp-bkt", AccessKeyId: "AKIA1", SecretKey: "secret-1", PathStyle: true},
		}),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "r2"},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{Connection: "bkp-conn", Passphrase: "a-long-passphrase-for-sealing"}}},
		},
		connectionEdit("free-conn", &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Region: "auto", Bucket: "free-bkt", AccessKeyId: "AKIA2", SecretKey: "secret-2", PathStyle: true},
		}),
		connectionEdit("private-conn", &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED,
			S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Region: "auto", Bucket: "priv-bkt", AccessKeyId: "AKIA3", SecretKey: "secret-3", PathStyle: true},
		}),
	))

	connSvc := connections.New(connections.NewConnections(f.store, f.keeper), f.config.Fleet, f.config.Agents, nil, f.store)

	// CheckBeforeLinking refuses bkp-conn before probing.
	checked, err := connSvc.CheckBeforeLinking(t.Context(), "bkp-conn", f.lastDocker.id.ServerID, env, "web", "web/", false)
	if checked || err == nil || !strings.Contains(err.Error(), "used for backups") {
		t.Fatalf("CheckBeforeLinking(bkp-conn) = %v, %v", checked, err)
	}
	if len(srv.Methods) != 0 {
		t.Fatalf("CheckBeforeLinking sent %d requests to s3test for a backup connection", len(srv.Methods))
	}

	// Corrupt r2's sealed spec in the store so it cannot be decoded: both CheckBeforeLinking
	// and ConnectStorage must fail closed without probing free-conn.
	if backend == store.Postgres {
		storetest.Exec(t, f.db, `DROP TRIGGER revisions_never_change ON revisions`)
		storetest.Exec(t, f.db, `UPDATE revisions SET spec = decode('0a0a', 'hex') WHERE resource_id IN (SELECT id FROM resources WHERE kind = 'backup.destination' AND name = 'r2')`)
	} else {
		storetest.Exec(t, f.db, `DROP TRIGGER revisions_never_change`)
		storetest.Exec(t, f.db, `UPDATE revisions SET spec = x'0a0a' WHERE resource_id IN (SELECT id FROM resources WHERE kind = 'backup.destination' AND name = 'r2')`)
	}
	checked, err = connSvc.CheckBeforeLinking(t.Context(), "free-conn", f.lastDocker.id.ServerID, env, "web", "web/", false)
	if checked || err == nil {
		t.Fatalf("CheckBeforeLinking with unreadable destination = %v, %v (expected fail-closed error)", checked, err)
	}
	_, err = cc.ConnectStorage(t.Context(), connect.NewRequest(&dockwellv1.ConnectStorageRequest{
		Project: proj, Environment: env, App: "web", Connection: "free-conn",
		Sets: map[string]resourcev1.LinkPart{"S3_BUCKET": resourcev1.LinkPart_LINK_PART_BUCKET},
	}))
	if err == nil {
		t.Fatal("ConnectStorage with unreadable destination succeeded, want fail-closed error")
	}

	// N6: reach is checked before scanning backup destinations in the same snapshot,
	// and a caller below install admin gets an error without destination names.
	hold := aMember(t, f)
	hold(store.Grant{ProjectID: proj, Role: store.RoleDeveloper})
	_, err = cc.ConnectStorage(t.Context(), connect.NewRequest(&dockwellv1.ConnectStorageRequest{
		Project: proj, Environment: env, App: "web", Connection: "private-conn",
		Sets: map[string]resourcev1.LinkPart{"S3_BUCKET": resourcev1.LinkPart_LINK_PART_BUCKET},
	}))
	if connect.CodeOf(err) != connect.CodeNotFound || strings.Contains(err.Error(), "r2") {
		t.Fatalf("ConnectStorage on out-of-reach connection = %v (want CodeNotFound without destination names)", err)
	}
	_, err = cc.ConnectStorage(t.Context(), connect.NewRequest(&dockwellv1.ConnectStorageRequest{
		Project: proj, Environment: env, App: "web", Connection: "free-conn",
		Sets: map[string]resourcev1.LinkPart{"S3_BUCKET": resourcev1.LinkPart_LINK_PART_BUCKET},
	}))
	if err == nil || strings.Contains(err.Error(), "r2") {
		t.Fatalf("developer ConnectStorage with unreadable destination = %v (want fail-closed error without destination names)", err)
	}
	if len(srv.Methods) != 0 {
		t.Fatalf("fail-closed preflight still sent %d requests to s3test", len(srv.Methods))
	}
}

func TestM1AndM4UseDestinationAsConnectionEdgeCases(t *testing.T) {
	storetest.Each(t, testM1AndM4UseDestinationAsConnectionEdgeCases)
}

func testM1AndM4UseDestinationAsConnectionEdgeCases(t *testing.T, backend store.Backend) {
	srv := s3test.New(t)
	srv.Buckets = []string{"acme-backups"}
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	c := changeClient(b, b.origin)
	bc := backupClient(b, b.origin)
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	volID, dir := webWithData(t, e, c)
	writeData(t, dir, "orders.db", "the orders")
	f.lastDocker.backups.HTTP = srv.Client()

	// M4: legacy destination with query string and fragment in endpoint, plus pre-existing connection named "r2" (M1).
	apply(t, c, propose(t, c, "Add legacy dest with query/fragment and competing connection",
		connectionEdit("r2", &resourcev1.ConnectionSpec{
			Kind: resourcev1.LinkKind_LINK_KIND_S3,
			S3:   &resourcev1.ConnectionS3{Endpoint: "https://other.example.com", Region: "auto", Bucket: "existing", AccessKeyId: "AKIAEXIST", SecretKey: "existing-secret", PathStyle: true},
		}),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "r2"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{
				Endpoint: srv.URL() + "?old=query#frag", Region: "auto", Bucket: "acme-backups",
				AccessKey: "AKIALEGACY", SecretKey: "legacy-secret", Passphrase: "a-long-passphrase-for-sealing",
			}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "bad-bucket"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{
				Endpoint: srv.URL(), Region: "auto", Bucket: "Bad_Bucket",
				AccessKey: "AKIABAD1", SecretKey: "bad-secret-1", Passphrase: "a-long-passphrase-for-sealing",
			}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "bad-region"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{
				Endpoint: srv.URL(), Region: "US_EAST_1", Bucket: "good-bucket",
				AccessKey: "AKIABAD2", SecretKey: "bad-secret-2", Passphrase: "a-long-passphrase-for-sealing",
			}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "force-query"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{
				Endpoint: srv.URL() + "?", Region: "auto", Bucket: "acme-backups",
				AccessKey: "AKIAFQ", SecretKey: "fq-secret", Passphrase: "a-long-passphrase-for-sealing",
			}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "slash-query"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{
				Endpoint: srv.URL() + "/gateway/?unused=x", Region: "auto", Bucket: "acme-backups",
				AccessKey: "AKIASQ", SecretKey: "sq-secret", Passphrase: "a-long-passphrase-for-sealing",
			}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "slash-empty-query"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{
				Endpoint: srv.URL() + "/gateway/?", Region: "auto", Bucket: "acme-backups",
				AccessKey: "AKIASEQ", SecretKey: "seq-secret", Passphrase: "a-long-passphrase-for-sealing",
			}}},
		},
	))

	// Upload a backup to the legacy query/fragment destination first.
	made, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volID, Destination: "r2"}))
	if err != nil {
		t.Fatal(err)
	}
	b1 := waitCopy(t, bc, made.Msg.GetBackup().GetId())
	if b1.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED {
		t.Fatalf("legacy query/fragment upload = %v (%s)", b1.GetCopy(), b1.GetCopyError())
	}

	// M1: explicit connection name "r2" is refused with already_exists; empty name generates "r2-2".
	if _, err := bc.UseDestinationAsConnection(t.Context(), connect.NewRequest(&dockwellv1.UseDestinationAsConnectionRequest{
		Destination: "r2", Connection: "r2",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "already a connection named r2") {
		t.Fatalf("explicit existing name = %v", err)
	}
	planned, err := bc.UseDestinationAsConnection(t.Context(), connect.NewRequest(&dockwellv1.UseDestinationAsConnectionRequest{
		Destination: "r2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if planned.Msg.GetConnection() != "r2-2" {
		t.Fatalf("generated connection name = %q, want r2-2", planned.Msg.GetConnection())
	}
	applied := apply(t, c, planned.Msg.GetChange())
	if applied.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("apply converted legacy query/fragment dest ended %v: %s", applied.GetState(), applied.GetError())
	}
	// Endpoint was canonicalized without query or fragment.
	conns, err := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	foundCanonical := false
	for _, ci := range conns.Msg.GetConnections() {
		if ci.GetName() == "r2-2" && ci.GetEndpoint() == srv.URL() {
			foundCanonical = true
		}
	}
	if !foundCanonical {
		t.Errorf("r2-2 endpoint not canonicalized to %s: %v", srv.URL(), conns.Msg.GetConnections())
	}
	// Pre-existing backup still fetches cleanly, and Undo restores the inline destination.
	if _, err := bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: b1.GetId(), ServerOnly: true})); err != nil {
		t.Fatal(err)
	}
	if _, err := bc.FetchBackup(t.Context(), connect.NewRequest(&dockwellv1.FetchBackupRequest{Id: b1.GetId()})); err != nil {
		t.Fatal(err)
	}
	eventually(t, "legacy backup fetched after canonicalized conversion", func() bool {
		got, err := bc.GetBackup(t.Context(), connect.NewRequest(&dockwellv1.GetBackupRequest{Id: b1.GetId()}))
		return err == nil && got.Msg.GetBackup().GetOnServer()
	})
	undone, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: applied.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if res := apply(t, c, undone.Msg.GetChange()); res.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("undo of canonicalized conversion ended %v: %s", res.GetState(), res.GetError())
	}

	// M4: unrepresentable bucket and region return clear conversion errors naming the field and fix.
	if _, err := bc.UseDestinationAsConnection(t.Context(), connect.NewRequest(&dockwellv1.UseDestinationAsConnectionRequest{
		Destination: "bad-bucket",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "bucket") || !strings.Contains(err.Error(), "can't be converted") {
		t.Errorf("bad-bucket conversion = %v", err)
	}
	if _, err := bc.UseDestinationAsConnection(t.Context(), connect.NewRequest(&dockwellv1.UseDestinationAsConnectionRequest{
		Destination: "bad-region",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "region") || !strings.Contains(err.Error(), "can't be converted") {
		t.Errorf("bad-region conversion = %v", err)
	}

	// N3: empty-query endpoint without trailing slash converts cleanly (ForceQuery stripped),
	// while endpoints whose trailing slash before "?" changes the signed S3 path are refused
	// with a field-specific endpoint error and left inline.
	fqPlan, err := bc.UseDestinationAsConnection(t.Context(), connect.NewRequest(&dockwellv1.UseDestinationAsConnectionRequest{
		Destination: "force-query",
	}))
	if err != nil {
		t.Fatalf("force-query conversion failed: %v", err)
	}
	if res := apply(t, c, fqPlan.Msg.GetChange()); res.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("apply force-query conversion ended %v: %s", res.GetState(), res.GetError())
	}
	for _, name := range []string{"slash-query", "slash-empty-query"} {
		_, err := bc.UseDestinationAsConnection(t.Context(), connect.NewRequest(&dockwellv1.UseDestinationAsConnectionRequest{
			Destination: name,
		}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "endpoint") || !strings.Contains(err.Error(), "/gateway//acme-backups/") {
			t.Errorf("%s conversion = %v (want endpoint signed-path refusal)", name, err)
		}
	}
	listedDests, err := bc.ListBackupDestinations(t.Context(), connect.NewRequest(&dockwellv1.ListBackupDestinationsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, di := range listedDests.Msg.GetDestinations() {
		if (di.GetName() == "slash-query" || di.GetName() == "slash-empty-query") && di.GetConnection() != "" {
			t.Errorf("refused destination %s was unexpectedly connected to %q", di.GetName(), di.GetConnection())
		}
	}
}

func TestFetchRejectsNewerSealedFormatBeforeDispatch(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s3 := s3test.New(t)
		s3.Buckets = []string{"backups"}
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		e := f.addDockerServer(t)
		c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
		volumeID, dir := webWithData(t, e, c)
		writeData(t, dir, "orders.db", "the orders")
		server := f.lastDocker
		server.backups.HTTP = s3.Client()
		apply(t, c, propose(t, c, "Add off-site backup storage", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.DestinationKind), Name: "offsite"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: &resourcev1.BackupDestination{
				Endpoint: s3.URL(), Region: "auto", Bucket: "backups", AccessKey: "AKIA", SecretKey: "fixture-key",
				Passphrase: "fixture sealed backup passphrase",
			}}},
		}))
		ctx := t.Context()
		made, err := bc.CreateBackup(ctx, connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID, Destination: "offsite"}))
		if err != nil {
			t.Fatal(err)
		}
		backupID := made.Msg.GetBackup().GetId()
		if stored := waitCopy(t, bc, backupID); stored.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED {
			t.Fatalf("copy was not stored: %v", stored)
		}
		const previous = "a previous fetch error"
		if err := f.store.Update(ctx, func(tx *store.Tx) error {
			return tx.SetBackupFetchError(ctx, backupID, previous)
		}); err != nil {
			t.Fatal(err)
		}

		server.stop()
		server.hello = func(h *agentv1.Hello) { h.BackupSealVersion = 0 }
		server.connect(t, f)

		req := &dockwellv1.FetchBackupRequest{Id: backupID}
		_, err = bc.FetchBackup(ctx, connect.NewRequest(req))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || reasonOf(err) != "agent_too_old" ||
			!strings.Contains(err.Error(), "uses sealed format v3, which server "+server.id.ServerName+" can't read") ||
			!strings.Contains(err.Error(), "Update Dockwell") {
			t.Fatalf("fetch to older server: %v", err)
		}
		if err := f.store.View(ctx, func(tx *store.Tx) error {
			got, err := tx.GetBackup(ctx, backupID)
			if err != nil {
				return err
			}
			if got.CopySealVersion != 3 || got.FetchError != previous {
				t.Fatalf("before dispatch: format v%d, fetch error %q", got.CopySealVersion, got.FetchError)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}

		server.stop()
		server.hello = nil
		server.connect(t, f)
		if _, err := bc.FetchBackup(ctx, connect.NewRequest(req)); err != nil {
			t.Fatalf("fetch to supported server: %v", err)
		}
	})
}
