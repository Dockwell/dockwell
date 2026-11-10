// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/control/backups"
	"github.com/dockwell/dockwell/internal/netguard"
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
	// Said as the storage's own code, which holds nothing that signs in.
	if refused.GetSaid() != "InvalidAccessKeyId" {
		t.Errorf("refused keys say %q", refused.GetSaid())
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
	// Asked to list, such keys are signed in and list nothing, for the form to ask for the bucket.
	scopedList, err := test(form(""))
	if err != nil {
		t.Fatal(err)
	}
	if !scopedList.GetSignedIn() || scopedList.GetListed() || scopedList.GetProblem() != dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_UNSPECIFIED {
		t.Errorf("listing with a key scoped to its bucket reads %v", scopedList)
	}
	srv.Refuse, srv.RefuseOnly = "", ""

	// Storage that answers a listing with an error of its own says so.
	srv.Refuse, srv.RefuseStatus = "InternalError", 500
	broken, err := test(form(""))
	if err != nil {
		t.Fatal(err)
	}
	if broken.GetProblem() != dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_STORAGE_ERROR || broken.GetSaid() != "InternalError" {
		t.Errorf("a listing the storage fails reads %v", broken)
	}
	srv.Refuse, srv.RefuseStatus = "", 0

	// A bucket in another region than the form's: S3's 301 with no Location is no write.
	srv.MovedTo = "eu-west-2"
	moved, err := test(form("backups"))
	if err != nil {
		t.Fatal(err)
	}
	if moved.GetWrote() || moved.GetProblem() != dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_WRONG_REGION || moved.GetRegion() != "eu-west-2" {
		t.Errorf("a bucket in another region reads %v", moved)
	}
	srv.MovedTo = ""

	// Keys that may write and not delete leave the test file, and say which.
	srv.Refuse, srv.RefuseOnly = "AccessDenied", "DELETE"
	kept, err := test(form("backups"))
	if err != nil {
		t.Fatal(err)
	}
	if !kept.GetWrote() || kept.GetProblem() != dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_CANT_DELETE ||
		!strings.HasPrefix(kept.GetTestFile(), "dw/.dockwell-test-") || kept.GetSaid() != "AccessDenied" {
		t.Errorf("a file that couldn't be deleted reads %v", kept)
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
	if tests != 9 {
		t.Errorf("the activity log has %d tests, want 9", tests)
	}
}

// Storage that takes the connection and never answers runs a test to its end: the
// person is told no answer came in time, and the test is still on the activity log.
func TestADestinationTestThatRunsOutOfTimeSaysSo(t *testing.T) {
	// Set before the control plane starts, and put back only once it has stopped.
	waited := backups.TestWait
	backups.TestWait = 300 * time.Millisecond
	t.Cleanup(func() { backups.TestWait = waited })
	srv := s3test.New(t)
	srv.Hold = make(chan struct{})
	t.Cleanup(func() { close(srv.Hold) })
	was := backups.StorageClient
	backups.StorageClient = srv.Client()
	t.Cleanup(func() { backups.StorageClient = was })

	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	res, err := backupClient(b, b.origin).TestBackupDestination(t.Context(), connect.NewRequest(&dockwellv1.TestBackupDestinationRequest{
		Destination: &resourcev1.BackupDestination{Endpoint: srv.URL(), Bucket: "backups", AccessKey: "AKIAEXAMPLE", SecretKey: "s3cret-example"},
	}))
	if err != nil {
		t.Fatalf("a test that ran out of time: %v", err)
	}
	if res.Msg.GetProblem() != dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_UNREACHABLE || !strings.HasPrefix(res.Msg.GetSaid(), "no answer in time") {
		t.Errorf("a test that ran out of time reads %v", res.Msg)
	}
	events, err := dockwellv1connect.NewAuditServiceClient(b, b.origin).ListAuditEvents(t.Context(), connect.NewRequest(&dockwellv1.ListAuditEventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(events.Msg.GetEvents(), func(e *dockwellv1.AuditEvent) bool { return e.GetAction() == backups.ActionTestDestination }) {
		t.Error("a test that ran out of time isn't on the activity log")
	}
}

// A test reaches storage as an alert channel's test reaches its far end: never the
// control server's own loopback, which a test that says what answered would read.
func TestADestinationTestDoesntReachTheControlServersLoopback(t *testing.T) {
	if _, guarded := backups.StorageClient.(*netguard.HTTP); !guarded {
		t.Fatalf("the storage client is %T, not netguard's, which follows no redirect", backups.StorageClient)
	}
	var asked atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		fmt.Fprint(w, "<ListAllMyBucketsResult><Buckets></Buckets></ListAllMyBucketsResult>")
	}))
	t.Cleanup(local.Close)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	for _, bucket := range []string{"", "backups"} {
		res, err := backupClient(b, b.origin).TestBackupDestination(t.Context(), connect.NewRequest(&dockwellv1.TestBackupDestinationRequest{
			Destination: &resourcev1.BackupDestination{Endpoint: local.URL, Bucket: bucket, AccessKey: "AKIAEXAMPLE", SecretKey: "s3cret-example"},
		}))
		if err != nil {
			t.Fatal(err)
		}
		if res.Msg.GetSignedIn() || res.Msg.GetProblem() != dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_UNREACHABLE {
			t.Errorf("testing %s, bucket %q, reads %v", local.URL, bucket, res.Msg)
		}
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("the control server's loopback was asked %d times", n)
	}
}
