// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/control/backups"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestADestinationFormIsTestedBeforeItsPlan(t *testing.T) {
	storetest.Each(t, testADestinationFormIsTestedBeforeItsPlan)
}

// A form's keys list the buckets they see; given a bucket, a test file is written
// under the prefix and deleted; what's wrong is said as a problem; and the activity
// log has the test, with the endpoint's host and never a key.
func testADestinationFormIsTestedBeforeItsPlan(t *testing.T, backend store.Backend) {
	srv := s3test.New(t)
	srv.Buckets = []string{"backups", "media"}
	was := backups.StorageClient
	backups.StorageClient = srv.Client()
	t.Cleanup(func() { backups.StorageClient = was })

	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	bc := backupClient(b, b.origin)
	test := func(d *resourcev1.BackupDestination) (*dockwellv1.TestBackupDestinationResponse, error) {
		res, err := bc.TestBackupDestination(t.Context(), connect.NewRequest(&dockwellv1.TestBackupDestinationRequest{Destination: d}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	form := func(bucket string) *resourcev1.BackupDestination {
		return &resourcev1.BackupDestination{Endpoint: srv.URL(), Bucket: bucket, Prefix: "dw/", AccessKey: "AKIAEXAMPLE", SecretKey: "s3cret-example"}
	}

	// No bucket yet: the buckets the keys see, for the form to pick from.
	listed, err := test(form(""))
	if err != nil {
		t.Fatal(err)
	}
	if !listed.GetSignedIn() || !listed.GetListed() || !slices.Equal(listed.GetBuckets(), []string{"backups", "media"}) || listed.GetWrote() {
		t.Errorf("listing reads %v", listed)
	}

	// A bucket: a file is written under the prefix, and gone again.
	wrote, err := test(form("backups"))
	if err != nil {
		t.Fatal(err)
	}
	if !wrote.GetWrote() || wrote.GetProblem() != dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_UNSPECIFIED {
		t.Errorf("writing reads %v", wrote)
	}
	if left := srv.Objects(); len(left) != 0 {
		t.Errorf("the test left %v in the bucket", left)
	}

	// A bucket that isn't there.
	missing, err := test(form("nope"))
	if err != nil {
		t.Fatal(err)
	}
	if missing.GetWrote() || missing.GetProblem() != dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_NO_BUCKET {
		t.Errorf("a missing bucket reads %v", missing)
	}

	// Keys the storage refuses.
	srv.Refuse = "InvalidAccessKeyId"
	refused, err := test(form("backups"))
	if err != nil {
		t.Fatal(err)
	}
	if refused.GetProblem() != dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_KEYS_REFUSED || refused.GetWrote() {
		t.Errorf("refused keys read %v", refused)
	}
	// Keys scoped to their bucket may not list, and still write.
	srv.Refuse, srv.RefuseOnly = "AccessDenied", "GET"
	scoped, err := test(form("backups"))
	if err != nil {
		t.Fatal(err)
	}
	if !scoped.GetWrote() || scoped.GetListed() {
		t.Errorf("a key scoped to its bucket reads %v", scoped)
	}
	srv.Refuse, srv.RefuseOnly = "", ""

	// What a plan would refuse is refused here too, before anything is asked.
	if _, err := test(&resourcev1.BackupDestination{Endpoint: "ftp://storage.example.com", AccessKey: "a", SecretKey: "b"}); reasonOf(err) != "invalid" {
		t.Errorf("an endpoint that isn't HTTP: %v", err)
	}

	events, err := dockwellv1connect.NewAuditServiceClient(b, b.origin).ListAuditEvents(t.Context(), connect.NewRequest(&dockwellv1.ListAuditEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var tests int
	for _, e := range events.Msg.GetEvents() {
		if e.GetAction() != backups.ActionTestDestination {
			continue
		}
		tests++
		if strings.Contains(e.GetDetail(), "AKIAEXAMPLE") || strings.Contains(e.GetDetail(), "s3cret") {
			t.Errorf("the activity log keeps a key: %s", e.GetDetail())
		}
	}
	if tests != 5 {
		t.Errorf("the activity log has %d tests, want 5", tests)
	}
}
