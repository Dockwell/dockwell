// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/backup/s3"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/store"
)

// ActionTestDestination is a test of a destination form on the activity log.
const ActionTestDestination = "backup_destination.test"

// TestWait bounds a whole test, so a form never waits on storage that doesn't
// answer. A test of this sets its own.
var TestWait = 20 * time.Second

// StorageClient reaches storage for a test as an alert channel's test reaches its
// far end (alert.TestGuard): the internet, and the private networks an admin may
// keep storage on, as MinIO often is, but never this server's own loopback, which
// a test that says what answered would read; and it follows no redirect, which
// could lead it there. A test sets its own.
var StorageClient s3.Doer = netguard.Client(netguard.Options{Private: true, MaxBody: 1 << 20, Timeout: TestWait})

// testedDestination is what the activity log keeps of a test: where, never what signs in.
type testedDestination struct {
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket,omitempty"`
	Wrote    bool   `json:"wrote"`
	Problem  string `json:"problem,omitempty"`
}

// TestBackupDestination signs in to storage with what a form holds, lists the
// buckets the keys may see, and writes and deletes a test file in the bucket named.
func (h *Handler) TestBackupDestination(ctx context.Context, req *connect.Request[dockwellv1.TestBackupDestinationRequest]) (*connect.Response[dockwellv1.TestBackupDestinationResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	client, _ := authapi.ClientFrom(ctx)
	d, _ := proto.Clone(req.Msg.GetDestination()).(*resourcev1.BackupDestination)
	if d == nil {
		d = &resourcev1.BackupDestination{}
	}
	// The form's checks, as a plan's would be, with a bucket left out to list them.
	listing := strings.TrimSpace(d.GetBucket()) == ""
	if listing {
		d.Bucket = "-"
	}
	if err := (backuppolicy.Destination{}).Validate(d); err != nil {
		return nil, authapi.Invalid(err)
	}
	if listing {
		d.Bucket = ""
	}
	// The record of the test is written whatever the storage did, past its time too.
	record := context.WithoutCancel(ctx)
	ctx, cancel := context.WithTimeout(ctx, TestWait)
	defer cancel()
	words := func(err error) string { return wordsOf(ctx, err) }
	bucket := &s3.Bucket{
		Endpoint: d.GetEndpoint(), Region: d.GetRegion(), Name: d.GetBucket(),
		AccessKey: d.GetAccessKey(), SecretKey: d.GetSecretKey(), Client: StorageClient,
	}
	out := &dockwellv1.TestBackupDestinationResponse{}
	names, err := bucket.ListBuckets(ctx)
	switch {
	case err == nil:
		out.SignedIn, out.Listed, out.Buckets = true, true, names
	case scopedRefusal(err):
		// Keys scoped to one bucket may not list, which says nothing against them: the
		// form asks for the bucket's name, and the write below says whether they work.
		out.SignedIn = true
	case refusedKeys(err):
		out.Problem, out.Said = dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_KEYS_REFUSED, words(err)
	default:
		var storage *s3.Error
		switch {
		case !errors.As(err, &storage):
			out.Problem, out.Said = dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_UNREACHABLE, words(err)
		case listing:
			// The storage answered with an error of its own; with a bucket, the write says more.
			out.Problem, out.Said = dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_STORAGE_ERROR, words(err)
		}
	}
	if !listing && out.Problem == dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_UNSPECIFIED {
		key := testKey(d.GetPrefix())
		if err := bucket.Put(ctx, key, []byte("Dockwell checks it can write here, and deletes this at once.\n")); err != nil {
			out.Problem, out.Said = problemOf(err), words(err)
			var storage *s3.Error
			if errors.As(err, &storage) {
				out.Region = storage.Region
			}
		} else {
			out.SignedIn, out.Wrote = true, true
			if err := bucket.Delete(ctx, key); err != nil {
				// Written and left there: a backup's pruning, which deletes, would fail too.
				out.Problem, out.Said, out.TestFile = dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_CANT_DELETE, words(err), key
			}
		}
	}
	detail, _ := json.Marshal(testedDestination{Endpoint: hostOf(d.GetEndpoint()), Bucket: d.GetBucket(), Wrote: out.GetWrote(), Problem: problemWord(out.GetProblem())})
	if err := h.db.Update(record, func(tx *store.Tx) error {
		return tx.RecordEvent(record, store.Event{
			ID: id.New(id.Event), At: h.now(), Actor: p.Actor(), Action: ActionTestDestination,
			Subject: hostOf(d.GetEndpoint()), Detail: string(detail), Surface: client.Surface(p),
		})
	}); err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

// testKey is a name no backup takes, under the destination's prefix.
func testKey(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := ".dockwell-test-" + hex.EncodeToString(b)
	if p := strings.Trim(prefix, "/"); p != "" {
		return p + "/" + name
	}
	return name
}

// refusedKeys reports storage that didn't take the keys or the signature.
func refusedKeys(err error) bool {
	var e *s3.Error
	return errors.As(err, &e) && (e.Status == http.StatusForbidden || e.Status == http.StatusUnauthorized)
}

// scopedRefusal reports a refusal that says the keys may not list, rather than that
// they're wrong: an access denied, which keys scoped to a bucket get from the root.
func scopedRefusal(err error) bool {
	var e *s3.Error
	return errors.As(err, &e) && e.Code == "AccessDenied"
}

// movedBucket reports a bucket the storage says is in another region than the form's:
// S3 answers 301 PermanentRedirect, with no Location, and the region in a header.
func movedBucket(e *s3.Error) bool {
	return e.Code == "PermanentRedirect" || e.Status == http.StatusMovedPermanently || e.Code == "AuthorizationHeaderMalformed" && e.Region != ""
}

// problemOf says why a write didn't happen.
func problemOf(err error) dockwellv1.BackupDestinationProblem {
	var e *s3.Error
	switch {
	case !errors.As(err, &e):
		return dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_UNREACHABLE
	case movedBucket(e):
		return dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_WRONG_REGION
	case e.Code == "NoSuchBucket" || e.Status == http.StatusNotFound:
		return dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_NO_BUCKET
	case e.Code == "AccessDenied":
		return dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_CANT_WRITE
	case e.Status == http.StatusForbidden || e.Status == http.StatusUnauthorized:
		return dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_KEYS_REFUSED
	}
	return dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_CANT_WRITE
}

// problemWord is a problem's word for the activity log.
func problemWord(p dockwellv1.BackupDestinationProblem) string {
	if p == dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_UNSPECIFIED {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(p.String(), "BACKUP_DESTINATION_PROBLEM_"))
}

// wordsOf says an error without a URL's path or query, which may hold a key: the
// storage's own code for what it refused, such as InvalidAccessKeyId, which holds
// nothing that signs in, and plainly when the test ran out of time.
func wordsOf(ctx context.Context, err error) string {
	var e *s3.Error
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "no answer in time: nothing came back within " + TestWait.String()
	case errors.As(err, &e) && e.Code != "":
		return e.Code
	case errors.As(err, &e):
		return "the storage answered " + http.StatusText(e.Status)
	}
	return netguard.Words(err)
}

// hostOf is an endpoint's host, as lists show a destination.
func hostOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Host
}
