// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

func TestDockerPushRequiresExactSessionApproval(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		f := startFull(t, backend, nil)
		b := signedIn(t, f)
		const fingerprint = "reviewed-push-fingerprint"
		change := id.New(id.Change)
		// Seed the read-only portion of a reviewed push to test authorization
		// against a stored plan without needing a registry.
		plan, err := json.Marshal(revision.Plan{
			Change: change, Fingerprint: fingerprint,
			Resources: []revision.ResourcePlan{{DockerMaintenance: &v1.DockerMaintenance{
				ImageAction: &v1.DockerImageAction{Push: &v1.DockerImagePush{Destination: "registry.example.test/fixture/image:review"}},
			}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.Revise(t.Context(), func(tx *store.RevisionTx) error {
			return tx.CreateChange(t.Context(), store.Change{
				ID: change, Actor: revision.SystemActor, State: store.ChangePlanned, Fingerprint: fingerprint, Plan: plan, CreatedAt: time.Now(),
			})
		}); err != nil {
			t.Fatal(err)
		}
		client := changeClient(b, b.origin)
		request := &v1.ApplyChangeRequest{Id: change}
		if _, err := client.ApplyChange(t.Context(), connect.NewRequest(request)); reasonOf(err) != "needs_confirmation" {
			t.Fatalf("push without a fingerprint = %v", err)
		}
		request.Fingerprint = fingerprint
		token := createToken(t, b, v1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		withToken := changeClient(bearer{token: token, rt: b.client.Transport}, b.origin)
		if _, err := withToken.ApplyChange(t.Context(), connect.NewRequest(request)); reasonOf(err) != authapi.ReasonNeedsSession {
			t.Fatalf("push with an API token = %v", err)
		}
		_, err = client.ApplyChange(t.Context(), connect.NewRequest(request))
		if reasonOf(err) != authapi.ReasonNeedsStepUp {
			t.Fatalf("push without a passkey = %v", err)
		}
		var rpcErr *connect.Error
		if !errors.As(err, &rpcErr) {
			t.Fatalf("expected a Connect error, got %v", err)
		}
		details := rpcErr.Details()
		if len(details) != 1 {
			t.Fatalf("push didn't return an exact approval target: %v", details)
		}
		value, err := details[0].Value()
		need, ok := value.(*v1.StepUpNeeded)
		if err != nil || !ok || need.GetAction() != v1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE || need.GetTarget() != change+":"+fingerprint {
			t.Fatalf("push approval challenge = %v, %v", value, err)
		}
		request.Proof = stepUpFor(t, b, v1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, change+":previous-review")
		if _, err := client.ApplyChange(t.Context(), connect.NewRequest(request)); reasonOf(err) != authapi.ReasonNeedsStepUp {
			t.Fatalf("push with a passkey for a different fingerprint = %v", err)
		}
	})
}

func TestReviewedDockerPushKeepsCredentialsPrivate(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		ctx := t.Context()
		const user, password, token = "fixture-user", "push-secret-351a929a", "bearer-fixture-b323f27a"
		var logs syncBuffer
		f := startFullLogging(t, backend, nil, slog.New(slog.NewTextHandler(&logs, nil)))
		b := signedIn(t, f)
		engine := f.addDockerServer(t)
		node := f.lastDocker
		serverID := node.id.ServerID
		data, image := dockerArchiveWithAliases(t)
		if _, err := engine.Client().LoadImage(ctx, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var realmMoved, timeoutBlob atomic.Bool
		manifests := map[string]string{}
		var publications, exchanges atomic.Int32
		reg := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v2/":
				realm := "https://" + r.Host + "/token"
				if realmMoved.Load() {
					realm = "https://different-token-host.invalid/token"
				}
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q`, realm))
				w.WriteHeader(http.StatusUnauthorized)
				return
			case "/token":
				exchanges.Add(1)
				u, p, ok := r.BasicAuth()
				if !ok || u != user || p != password || r.URL.Query().Get("scope") != "repository:fixture/private:pull,push" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+token {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if strings.Contains(r.URL.Path, "/blobs/") && r.Method == http.MethodHead {
				if timeoutBlob.Load() {
					<-r.Context().Done()
					return
				}
				w.WriteHeader(http.StatusOK)
				return
			}
			if !strings.HasPrefix(r.URL.Path, "/v2/fixture/private/manifests/") {
				http.NotFound(w, r)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			switch r.Method {
			case http.MethodHead:
				digest := manifests[r.URL.Path]
				if digest == "" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Docker-Content-Digest", digest)
			case http.MethodPut:
				if strings.HasSuffix(r.URL.Path, "/denied") {
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, password+" "+token)
					return
				}
				body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				digest := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
				manifests[r.URL.Path] = digest
				publications.Add(1)
				w.Header().Set("Docker-Content-Digest", digest)
				w.WriteHeader(http.StatusCreated)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		}))
		t.Cleanup(reg.Close)
		node.stop()
		waitConnected(t, f, serverID, false)
		transport := reg.Client()
		transport.Timeout = time.Second
		node.images = agent.ImageReads{Secure: transport}
		node.logger = slog.New(slog.NewTextHandler(&logs, nil))
		node.run(t)
		waitConnected(t, f, serverID, true)

		host := strings.TrimPrefix(reg.URL, "https://")
		changes := changeClient(b, b.origin)
		api := dockwellv1connect.NewDockerServiceClient(b, b.origin, connect.WithProtoJSON())
		assertClean := func(label, text string) {
			t.Helper()
			for _, secret := range []string{password, token, base64.StdEncoding.EncodeToString([]byte(user + ":" + password))} {
				if strings.Contains(text, secret) {
					t.Fatalf("%s contains registry credentials", label)
				}
			}
		}
		applyPermission := func(ch *v1.Change) {
			t.Helper()
			fields := ch.GetPlan().GetResources()[0].GetChanges()
			if len(fields) != 1 || fields[0].GetPath() != "allow_push" {
				t.Fatalf("permission review changed more than push permission: %v", fields)
			}
			fp := ch.GetPlan().GetFingerprint()
			proof := stepUpFor(t, b, v1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, ch.GetId()+":"+fp)
			w := watchWithin(t, changes, ch.GetId(), 20*time.Second)
			if _, err := changes.ApplyChange(ctx, connect.NewRequest(&v1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: fp, Proof: proof})); err != nil {
				t.Fatal(err)
			}
			if done := waitWatchWithin(t, w, 20*time.Second).last(); done.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED {
				t.Fatalf("login permission change failed: %s", done.GetError())
			}
		}
		enable := func() {
			t.Helper()
			change := propose(t, changes, "Allow fixture pushes", loginEdit("push-fixture", &resourcev1.RegistryLogin{AllowPush: proto.Bool(true)}))
			fp := change.GetPlan().GetFingerprint()
			if _, err := changes.ApplyChange(ctx, connect.NewRequest(&v1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: fp})); reasonOf(err) != authapi.ReasonNeedsStepUp {
				t.Fatalf("push permission change took no passkey: %v", err)
			}
			applyPermission(change)
		}
		save := func(value string) {
			t.Helper()
			change := propose(t, changes, "Save push fixture login", loginEdit("push-fixture", &resourcev1.RegistryLogin{
				Registry: host, Username: user, Password: value,
			}))
			// Updating an already-enabled login leaves the permission alone.
			apply(t, changes, change)
		}
		prepare := func(tag string) (*v1.Change, error) {
			t.Helper()
			got, err := api.PlanDockerMaintenance(ctx, connect.NewRequest(&v1.PlanDockerMaintenanceRequest{
				ServerIds: []string{serverID}, Operation: &v1.PlanDockerMaintenanceRequest_Push{Push: &v1.DockerImagePush{
					SourceKey:   &v1.DockerImageSourceKey{ServerId: serverID, ImageId: image},
					Destination: host + "/fixture/private:" + tag,
				}},
			}))
			if err != nil {
				assertClean("preview error", err.Error())
				return nil, err
			}
			assertClean("preview", protojson.Format(got.Msg))
			return got.Msg.GetChange(), nil
		}
		plan := func(tag string) *v1.Change {
			t.Helper()
			ch, err := prepare(tag)
			if err != nil || ch == nil {
				t.Fatalf("push review for %s: %v", tag, err)
			}
			return ch
		}
		proof := func(ch *v1.Change) string {
			t.Helper()
			return stepUpFor(t, b, v1.StepUpAction_STEP_UP_ACTION_APPROVE_CHANGE, ch.GetId()+":"+ch.GetPlan().GetFingerprint())
		}
		request := func(ch *v1.Change, proof string) *connect.Request[v1.ApplyChangeRequest] {
			return connect.NewRequest(&v1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint(), Proof: proof})
		}
		witness := func(ch *v1.Change, proof string) *v1.Change {
			t.Helper()
			w := watchWithin(t, changes, ch.GetId(), 20*time.Second)
			if _, err := changes.ApplyChange(ctx, request(ch, proof)); err != nil {
				t.Fatal(err)
			}
			done := waitWatchWithin(t, w, 20*time.Second).last()
			assertClean("apply and receipts", protojson.Format(done))
			if err := f.store.View(ctx, func(tx *store.Tx) error {
				stored, err := tx.GetChange(ctx, ch.GetId())
				if err != nil {
					return err
				}
				assertClean("stored plan and progress", string(stored.Plan)+string(stored.Progress))
				events, err := tx.ListEvents(ctx, 1000)
				text, _ := json.Marshal(events)
				assertClean("Activity", string(text))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return done
		}
		save(password)
		if _, err := prepare("reviewed"); reasonOf(err) != "docker_push_login" || exchanges.Load() != 0 {
			t.Fatalf("a pull-only login was sent to the registry: %v", err)
		}
		enable()
		first := plan("reviewed")
		oneUse := proof(first)
		if done := witness(first, oneUse); done.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED || publications.Load() != 1 {
			t.Fatalf("push did not publish: %s (%d writes)", done.GetError(), publications.Load())
		}
		client := auth.Client{Site: auth.Site{Origin: b.origin, RPID: "localhost"}}
		var session string
		for _, cookie := range b.cookies() {
			if cookie.Name == auth.LoopbackCookieName {
				session = cookie.Value
			}
		}
		principal, err := f.auth.Authenticate(ctx, client, session)
		if err != nil {
			t.Fatal(err)
		}
		exactTarget := first.GetId() + ":" + first.GetPlan().GetFingerprint()
		if _, err := f.auth.HoldStepUp(ctx, client, principal, oneUse, auth.StepUpApproveChange, exactTarget); !errors.Is(err, auth.ErrNeedsStepUp) {
			t.Fatalf("a spent push proof for the exact plan was accepted: %v", err)
		}

		changed := plan("rotated")
		save(password + "-rotated")
		before := exchanges.Load()
		done := witness(changed, proof(changed))
		if !strings.Contains(done.GetError(), "registry login changed") || exchanges.Load() != before {
			t.Fatalf("rotated login reached the registry: %s", done.GetError())
		}
		save(password)
		changed = plan("realm")
		before = exchanges.Load()
		realmMoved.Store(true)
		done = witness(changed, proof(changed))
		if done.GetSteps()[0].GetDockerImageReceipts()[0].GetReason() != v1.DockerImageReason_DOCKER_IMAGE_REASON_REALM_CHANGED || exchanges.Load() != before {
			t.Fatalf("moved realm result: %s", done.GetError())
		}
		realmMoved.Store(false)
		changed = plan("denied")
		done = witness(changed, proof(changed))
		if done.GetSteps()[0].GetDockerImageReceipts()[0].GetOutcome() != v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_REFUSED ||
			done.GetSteps()[0].GetReason() != "docker_push_login" {
			t.Fatalf("definite manifest refusal: %s", done.GetError())
		}
		changed = plan("timeout")
		timeoutBlob.Store(true)
		done = witness(changed, proof(changed))
		timeoutBlob.Store(false)
		if done.GetSteps()[0].GetReason() != "docker_push_not_published" ||
			done.GetSteps()[0].GetDockerImageReceipts()[0].GetOutcome() != v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_FAILED {
			t.Fatalf("pre-publication timeout: %s", done.GetError())
		}
		mu.Lock()
		manifests["/v2/fixture/private/manifests/conflict"] = "sha256:" + strings.Repeat("a", 64)
		mu.Unlock()
		if _, err := prepare("conflict"); reasonOf(err) != "docker_push_conflict" {
			t.Fatalf("occupied destination: %v", err)
		}
		changed = plan("revoked")
		applyPermission(propose(t, changes, "Disable fixture pushes", loginEdit("push-fixture", &resourcev1.RegistryLogin{AllowPush: proto.Bool(false)})))
		before = exchanges.Load()
		done = witness(changed, proof(changed))
		if !strings.Contains(done.GetError(), "registry login changed") || exchanges.Load() != before {
			t.Fatalf("revoked push permission reached the registry: %s", done.GetError())
		}
		assertClean("agent and control logs", logs.String())
	})
}
