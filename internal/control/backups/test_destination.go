// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
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
	"github.com/dockwell/dockwell/internal/resource/connection"
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
var (
	defaultStorageClient s3.Doer = netguard.Client(netguard.Options{Private: true, MaxBody: 1 << 20, Timeout: TestWait})
	publicStorageClient  s3.Doer = netguard.Client(netguard.Options{Private: false, MaxBody: 1 << 20, Timeout: TestWait})
	StorageClient        s3.Doer = defaultStorageClient
)

// testedDestination is what the activity log keeps of a test: where, never what signs in.
type testedDestination struct {
	Endpoint           string `json:"endpoint"`
	Bucket             string `json:"bucket,omitempty"`
	Wrote              bool   `json:"wrote"`
	Problem            string `json:"problem,omitempty"`
	Server             string `json:"server,omitempty"`
	HostKeyFingerprint string `json:"host_key_fingerprint,omitempty"`
	HostKeyAlgorithm   string `json:"host_key_algorithm,omitempty"`
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
	if d.GetSftp() != nil {
		out, err := h.testSFTPDestination(ctx, d, req.Msg.GetServer(), req.Msg.GetExistingDestination())
		if err != nil {
			return nil, err
		}
		if err := h.recordDestinationTest(ctx, p.Actor(), client.Surface(p), backuppolicy.SFTPConfig(d.GetSftp()).Address(), "", out); err != nil {
			return nil, err
		}
		return connect.NewResponse(out), nil
	}
	var (
		endpoint, region, bucketName, accessKey, secretKey string
		virtualHost                                        bool
		listing                                            bool
		httpClient                                         = StorageClient
	)
	if d.GetConnection() != "" {
		if err := (backuppolicy.Destination{}).Validate(d); err != nil {
			return nil, authapi.Invalid(err)
		}
		var conn *resourcev1.ConnectionSpec
		if err := h.db.View(ctx, func(tx *store.Tx) error {
			var err error
			_, conn, err = h.resolveConnection(ctx, tx, d.GetConnection())
			return err
		}); err != nil {
			return nil, h.fail(ctx, err)
		}
		s3c := conn.GetS3()
		if err := backuppolicy.CheckTransport(s3c.GetEndpoint(), s3c.GetBucket(), s3c.GetPathStyle()); err != nil {
			return nil, authapi.Invalid(fmt.Errorf("the connection %s can't be used for backups: its endpoint %s. Change %s to HTTPS", d.GetConnection(), err.Error(), d.GetConnection()))
		}
		endpoint, region, bucketName = s3c.GetEndpoint(), s3c.GetRegion(), s3c.GetBucket()
		accessKey, secretKey = s3c.GetAccessKeyId(), s3c.GetSecretKey()
		virtualHost = !s3c.GetPathStyle()
		if httpClient == defaultStorageClient && !connection.Private(conn) {
			httpClient = publicStorageClient
		}
	} else {
		// The form's checks, as a plan's would be, with a bucket left out to list them.
		listing = strings.TrimSpace(d.GetBucket()) == ""
		if listing {
			d.Bucket = "-"
		}
		if err := (backuppolicy.Destination{}).Validate(d); err != nil {
			return nil, authapi.Invalid(err)
		}
		if listing {
			d.Bucket = ""
		}
		endpoint, region, bucketName = d.GetEndpoint(), d.GetRegion(), d.GetBucket()
		accessKey, secretKey = d.GetAccessKey(), d.GetSecretKey()
	}
	test, cancel := context.WithTimeout(ctx, TestWait)
	defer cancel()
	words := func(err error) string { return wordsOf(test, err) }
	bucket := &s3.Bucket{
		Endpoint: endpoint, Region: region, Name: bucketName,
		AccessKey: accessKey, SecretKey: secretKey, VirtualHost: virtualHost, Client: httpClient,
	}
	out := &dockwellv1.TestBackupDestinationResponse{}
	names, err := bucket.ListBuckets(test)
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
		if err := bucket.Put(test, key, []byte("Dockwell checks it can write here, and deletes this at once.\n")); err != nil {
			out.Problem, out.Said = problemOf(err), words(err)
			var storage *s3.Error
			if errors.As(err, &storage) {
				out.Region = storage.Region
			}
		} else {
			out.SignedIn, out.Wrote = true, true
			if err := bucket.Delete(test, key); err != nil {
				// Written and left there: a backup's pruning, which deletes, would fail too.
				out.Problem, out.Said, out.TestFile = dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_CANT_DELETE, words(err), key
			}
		}
	}
	if err := h.recordDestinationTest(ctx, p.Actor(), client.Surface(p), hostOf(endpoint), bucketName, out); err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

func (h *Handler) testSFTPDestination(ctx context.Context, d *resourcev1.BackupDestination, server, existing string) (*dockwellv1.TestBackupDestinationResponse, error) {
	if server == "" {
		return nil, authapi.Invalid(errors.New("choose which server should test the SFTP destination"))
	}
	if existing != "" {
		saved, err := h.destination(ctx, existing)
		if err != nil {
			return nil, h.fail(ctx, err)
		}
		defer clear(saved)
		if saved["transport"] != "sftp" {
			return nil, authapi.Invalid(errors.New("the saved destination must use SFTP to reuse its sign-in"))
		}
		port, _ := strconv.ParseUint(saved["port"], 10, 32)
		backuppolicy.KeepSFTPCredentials(&resourcev1.BackupDestinationSftp{
			Host: saved["host"], Port: uint32(port), User: saved["user"], HostKeyFingerprint: saved["host_key_fingerprint"],
			HostKeyAlgorithm: saved["host_key_algorithm"], PrivateKey: saved["private_key"],
			KeyPassphrase: saved["key_passphrase"], Password: saved["password"],
		}, d.GetSftp())
	}
	if err := backuppolicy.ValidateSFTP(d, false); err != nil {
		return nil, authapi.Invalid(err)
	}
	var target store.Server
	if err := h.db.View(ctx, func(tx *store.Tx) error {
		servers, err := tx.ListServers(ctx)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(servers, func(s store.Server) bool { return s.ID == server || s.Name == server })
		if i < 0 {
			return authapi.ReasonError(connect.CodeNotFound, errors.New("there is no such server. Choose a server to run the test"), "not_found")
		}
		target = servers[i]
		return nil
	}); err != nil {
		return nil, h.fail(ctx, err)
	}
	session, err := h.session(target)
	if err != nil {
		return nil, err
	}
	if !session.TestsSFTPDestinations() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("the agent on %s can't test SFTP. Update the agent and try again", target.Name), "agent_too_old")
	}
	values := backuppolicy.SFTPValues(d.GetSftp())
	defer clear(values)
	out, err := session.TestSFTPDestination(ctx, values, TestWait)
	if err != nil {
		return nil, h.agentError(target, err)
	}
	out.Sftp.ServerId, out.Sftp.ServerName = target.ID, target.Name
	return out, nil
}

func (h *Handler) recordDestinationTest(ctx context.Context, actor, surface, endpoint, bucket string, out *dockwellv1.TestBackupDestinationResponse) error {
	// The test is recorded even when the caller went away or the test ran out of time.
	record := context.WithoutCancel(ctx)
	told := testedDestination{Endpoint: endpoint, Bucket: bucket, Wrote: out.GetWrote(), Problem: problemWord(out.GetProblem())}
	sftp := out.GetSftp()
	told.Server = sftp.GetServerName()
	problem := out.GetProblem()
	if problem == dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_HOST_KEY_UNVERIFIED ||
		problem == dockwellv1.BackupDestinationProblem_BACKUP_DESTINATION_PROBLEM_HOST_KEY_CHANGED {
		told.HostKeyFingerprint, told.HostKeyAlgorithm = sftp.GetHostKeyFingerprint(), sftp.GetHostKeyAlgorithm()
	}
	detail, _ := json.Marshal(told)
	return h.db.Update(record, func(tx *store.Tx) error {
		return tx.RecordEvent(record, store.Event{
			ID: id.New(id.Event), At: h.now(), Actor: actor, Action: ActionTestDestination,
			Subject: endpoint, Target: sftp.GetServerId(), Detail: string(detail), Surface: surface,
		})
	})
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
