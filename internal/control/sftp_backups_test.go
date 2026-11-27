// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/backup/sftpstore"
	"github.com/dockwell/dockwell/internal/backup/sftpstore/sftptest"
	"github.com/dockwell/dockwell/internal/control/backups"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/store"
)

func TestSFTPBackupsUseTheAgentAndExistingRetention(t *testing.T) {
	t.Parallel()
	eachStoreParallel(t, testSFTPBackupsUseTheAgentAndExistingRetention)
}

func testSFTPBackupsUseTheAgentAndExistingRetention(t *testing.T, backend store.Backend) {
	const password = "example-sftp-credential"
	const passphrase = "example sealed backup passphrase"
	const name = "offsite"
	server, err := sftptest.Listen(t.Context(), "127.0.0.1:0", sftptest.Login{User: "backups", Password: password})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	host, port, err := net.SplitHostPort(server.Address())
	if err != nil {
		t.Fatal(err)
	}
	num, err := strconv.ParseUint(port, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	cfg := sftpstore.Config{Host: host, Port: uint32(num), User: "backups", Path: "/backups", Fingerprint: server.Fingerprint(), Password: password}

	var logs syncBuffer
	f := startFullLogging(t, backend, nil, slog.New(slog.NewTextHandler(&logs, nil)))
	b := signedIn(t, f)
	c, bc := changeClient(b, b.origin), backupClient(b, b.origin)
	ingress := &routesInMemory{routes: map[resource.Ref]*resourcev1.Route{}}
	f.probe = connects
	engine := f.addServerWith(t, func() []resource.Type {
		return []resource.Type{&route.Driver{Ingress: ingress}}
	})
	volumeID, dir := webWithData(t, engine, c)
	body := make([]byte, 256<<10)
	rand.Read(body)
	content := base64.StdEncoding.EncodeToString(body)
	writeData(t, dir, "orders.db", content)

	spec := &resourcev1.BackupDestination{Passphrase: passphrase, Sftp: &resourcev1.BackupDestinationSftp{
		Host: cfg.Host, Port: cfg.Port, User: cfg.User, Path: cfg.Path, HostKeyFingerprint: cfg.Fingerprint, HostKeyAlgorithm: "ssh-ed25519", Password: cfg.Password,
	}}
	plan := propose(t, c, "Send sealed backups to SFTP", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "backup.destination", Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: spec}},
	})
	applied := apply(t, c, plan)
	checkSecrets := func(text string) {
		t.Helper()
		if strings.Contains(text, password) || strings.Contains(text, passphrase) {
			t.Error("a response, log or event contained a destination secret")
		}
	}
	checkSecrets(protojson.Format(plan))
	checkSecrets(protojson.Format(applied))
	listed, err := bc.ListBackupDestinations(t.Context(), connect.NewRequest(&dockwellv1.ListBackupDestinationsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if rows := listed.Msg.GetDestinations(); len(rows) != 1 || rows[0].GetSftp().GetHostKeyFingerprint() != cfg.Fingerprint {
		t.Fatalf("listed destination = %v", rows)
	}
	checkSecrets(protojson.Format(listed.Msg))

	// The selected agent can probe its loopback and reports the key before
	// offering credentials, even when the probe reuses a saved destination.
	check := &dockwellv1.TestBackupDestinationRequest{
		Server: "hz-1",
		Destination: &resourcev1.BackupDestination{Sftp: &resourcev1.BackupDestinationSftp{
			Host: cfg.Host, Port: cfg.Port, User: cfg.User, Path: cfg.Path, Password: cfg.Password,
		}},
	}
	probe, err := bc.TestBackupDestination(t.Context(), connect.NewRequest(check))
	if err != nil {
		t.Fatal(err)
	}
	if probe.Msg.GetProblem() != dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_HOST_KEY_UNVERIFIED ||
		probe.Msg.GetSftp().GetHostKeyFingerprint() != cfg.Fingerprint ||
		probe.Msg.GetSftp().GetServerName() != check.Server || server.AuthAttempts() != 0 {
		t.Fatalf("probe = %v", probe.Msg)
	}
	check.Destination.Sftp.Password = ""
	check.Destination.Sftp.HostKeyFingerprint = cfg.Fingerprint
	check.Destination.Sftp.HostKeyAlgorithm = "ssh-ed25519"
	check.ExistingDestination = name
	probe, err = bc.TestBackupDestination(t.Context(), connect.NewRequest(check))
	if err != nil || !probe.Msg.GetSftp().GetDeleted() {
		t.Fatalf("probe with saved credentials = %v, %v", probe, err)
	}
	checkSecrets(protojson.Format(probe.Msg))
	audit, err := dockwellv1connect.NewAuditServiceClient(b, b.origin).ListAuditEvents(t.Context(), connect.NewRequest(&dockwellv1.ListAuditEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(audit.Msg.GetEvents(), func(e *dockwellv1.AuditEvent) bool {
		return e.GetAction() == backups.ActionTestDestination &&
			strings.Contains(e.GetDetail(), cfg.Fingerprint) && strings.Contains(e.GetDetail(), "host_key_unverified")
	}) {
		t.Error("the destination probe was not recorded")
	}
	checkSecrets(protojson.Format(audit.Msg))

	// A broken upload leaves the backup on its server and the retry in Activity.
	server.CutWriteAt(1 << 15)
	lost, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID, Destination: name}))
	if err != nil {
		t.Fatal(err)
	}
	failed := waitCopy(t, bc, lost.Msg.GetBackup().GetId())
	if !failed.GetOnServer() || failed.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_FAILED ||
		!strings.Contains(failed.GetCopyError(), "Take another backup") {
		t.Fatalf("interrupted upload = %v", failed)
	}
	readEvents := func() *dockwellv1.ListEventsResponse {
		t.Helper()
		events, err := eventClient(b, b.origin).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{
			Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_BACKUP},
		}))
		if err != nil {
			t.Fatal(err)
		}
		return events.Msg
	}
	if !slices.ContainsFunc(readEvents().GetEvents(), func(e *dockwellv1.Event) bool {
		return e.GetAction() == "backup.copy_failed" && strings.Contains(e.GetBackup().GetError(), "Take another backup")
	}) {
		t.Error("Activity omitted the copy failure and retry")
	}

	// A changed pin stops the send before authentication, and Activity names
	// the agent that observed the new fingerprint.
	pinEdit := func(pin string) {
		next := proto.CloneOf(spec)
		next.Passphrase = ""
		next.Sftp.HostKeyFingerprint = pin
		apply(t, c, propose(t, c, "Update the SFTP pin", &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "backup.destination", Name: name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_BackupDestination{BackupDestination: next}},
		}))
	}
	pinEdit("SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	attempts := server.AuthAttempts()
	refused, err := bc.CreateBackup(t.Context(), connect.NewRequest(&dockwellv1.CreateBackupRequest{VolumeId: volumeID, Destination: name}))
	if err != nil {
		t.Fatal(err)
	}
	mismatch := waitCopy(t, bc, refused.Msg.GetBackup().GetId())
	if !mismatch.GetOnServer() || !strings.Contains(mismatch.GetCopyError(), cfg.Fingerprint) || server.AuthAttempts() != attempts {
		t.Fatalf("changed pin copy = %v", mismatch)
	}
	if !slices.ContainsFunc(readEvents().GetEvents(), func(e *dockwellv1.Event) bool {
		return e.GetBackup().GetId() == mismatch.GetId() && e.GetBackup().GetReason() == resource.ReasonHostKeyChanged &&
			e.GetBackup().GetServerName() == check.Server
	}) {
		t.Error("Activity omitted the changed host key or its server")
	}
	pinEdit(cfg.Fingerprint)

	// Each schedule makes three copies, then prunes its oldest one in SFTP.
	engine.Registry["postgres:17"] = fakePostgres
	compose := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	database := apply(t, c, importCompose(t, compose, shopStack, map[string]string{"DB_PASSWORD": "example"}, false))
	var dbRef resource.Ref
	for _, row := range database.GetPlan().GetResources() {
		if ref := row.GetResource(); ref.GetKind() == string(volume.Kind) {
			dbRef = resource.Ref{Kind: volume.Kind, Scope: ref.GetScope(), Name: ref.GetName()}
			break
		}
	}
	if dbRef.Scope == "" {
		t.Fatal("the database plan didn't make a volume")
	}
	writeData(t, engine.VolumeDir(volume.TargetOf(dbRef).Name), "data.sql", "CREATE TABLE notes;")

	pullsAnything(engine)
	siteDir := t.TempDir()
	writeData(t, siteDir, "index.html", "an example website")
	websites := websiteClient(b, b.origin)
	site, err := websites.CreateWebsite(t.Context(), connect.NewRequest(&dockwellv1.CreateWebsiteRequest{
		Domain: "docs.example.com",
		Website: &resourcev1.Website{Serves: &resourcev1.Website_Folder{
			Folder: &resourcev1.WebsiteFolder{Root: siteDir},
		}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	applyReviewed(t, c, site.Msg.GetChange())
	website := listWebsites(t, websites)[0]

	edit := policyEdit(2)
	edit.GetResource().Scope = failed.GetEnvironmentId()
	edit.GetSpec().GetBackupPolicy().Destination = name
	dbPolicy := policyEdit(2)
	dbPolicy.GetResource().Scope, dbPolicy.GetResource().Name = dbRef.Scope, "postgres-daily"
	dbPolicy.GetSpec().GetBackupPolicy().Volume, dbPolicy.GetSpec().GetBackupPolicy().Destination = dbRef.Name, name
	sitePolicy := policyEdit(2)
	sitePolicy.GetResource().Scope, sitePolicy.GetResource().Name = website.GetEnvironmentId(), "website-daily"
	sitePolicy.GetSpec().GetBackupPolicy().Volume, sitePolicy.GetSpec().GetBackupPolicy().Destination = "", name
	sitePolicy.GetSpec().GetBackupPolicy().WebsiteFolder = true
	apply(t, c, propose(t, c, "Keep two SFTP copies per schedule", edit, dbPolicy, sitePolicy))
	now := time.Now()
	for n := 1; n <= 3; n++ {
		day(t, f.server.backups, now, n)
	}
	groups := make(map[string][]*dockwellv1.Backup)
	for _, row := range backupsOf(t, bc) {
		if row.GetCopy() != dockwellv1.BackupCopy_BACKUP_COPY_STORED || row.GetPolicyId() == "" {
			continue
		}
		kind := "volume"
		switch {
		case row.GetDatabase() == "PostgreSQL":
			kind = "database"
		case row.GetWebsiteId() == website.GetId():
			kind = "website"
		}
		groups[kind] = append(groups[kind], row)
	}
	files, _, err := sftpstore.Open(t.Context(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	for _, kind := range []string{"volume", "database", "website"} {
		rows := groups[kind]
		if len(rows) != 2 {
			t.Fatalf("scheduled %s copies = %v, want two", kind, rows)
		}
		for _, row := range rows {
			var backup store.Backup
			if err := f.store.View(t.Context(), func(tx *store.Tx) error {
				var err error
				backup, err = tx.GetBackup(t.Context(), row.GetId())
				return err
			}); err != nil {
				t.Fatal(err)
			}
			body, err := files.Get(t.Context(), backup.CopyKey)
			if err != nil {
				t.Fatalf("SFTP %s retention removed a kept copy: %v", kind, err)
			}
			_ = body.Close()
		}
	}

	backupID := groups["volume"][0].GetId()
	if _, err := bc.DeleteBackup(t.Context(), connect.NewRequest(&dockwellv1.DeleteBackupRequest{Id: backupID, ServerOnly: true})); err != nil {
		t.Fatal(err)
	}
	server.CutReadAt(1 << 15)
	fetch := func() {
		t.Helper()
		if _, err := bc.FetchBackup(t.Context(), connect.NewRequest(&dockwellv1.FetchBackupRequest{Id: backupID})); err != nil {
			t.Fatal(err)
		}
		f.server.backups.Wait()
	}
	get := func() *dockwellv1.Backup {
		t.Helper()
		r, err := bc.GetBackup(t.Context(), connect.NewRequest(&dockwellv1.GetBackupRequest{Id: backupID}))
		if err != nil {
			t.Fatal(err)
		}
		return r.Msg.GetBackup()
	}
	fetch()
	if row := get(); row.GetOnServer() || !strings.Contains(row.GetFetchError(), "fetch it again") {
		t.Fatalf("interrupted fetch = %v", row)
	}
	if !slices.ContainsFunc(readEvents().GetEvents(), func(e *dockwellv1.Event) bool {
		return e.GetAction() == "backup.fetch_failed" && strings.Contains(e.GetBackup().GetError(), "fetch it again")
	}) {
		t.Error("Activity omitted the fetch failure and retry")
	}
	fetch()
	if row := get(); !row.GetOnServer() || row.GetFetchError() != "" {
		t.Fatalf("retried fetch = %v", row)
	}
	writeData(t, dir, "orders.db", "changed since")
	planRestore, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&dockwellv1.RestoreBackupRequest{Id: backupID}))
	if err != nil {
		t.Fatal(err)
	}
	restore := planRestore.Msg.GetChange()
	watcher := watch(t, c, restore.GetId())
	if _, err := c.ApplyChange(t.Context(), connect.NewRequest(&dockwellv1.ApplyChangeRequest{
		Id: restore.GetId(), Fingerprint: restore.GetPlan().GetFingerprint(),
	})); err != nil {
		t.Fatal(err)
	}
	if end := waitWatch(t, watcher).last(); end.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("restore did not apply: %v", end)
	}
	if got := readData(t, dir, "orders.db"); got != content {
		t.Error("the restore didn't bring back the backup's data")
	}
	checkSecrets(protojson.Format(readEvents()))
	checkSecrets(logs.String())
}
