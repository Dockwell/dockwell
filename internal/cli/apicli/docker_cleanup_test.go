// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
)

type stubDockerCleanup struct {
	dockwellv1connect.UnimplementedDockerServiceHandler
	dockwellv1connect.UnimplementedServerServiceHandler
	servers     []*dockwellv1.Server
	preview     *dockwellv1.PreviewDockerCleanupResponse
	lastPreview *dockwellv1.PreviewDockerCleanupRequest
	lastPlan    *dockwellv1.PlanDockerMaintenanceRequest
	planChange  *dockwellv1.Change
}

func (s *stubDockerCleanup) ListServers(_ context.Context, _ *connect.Request[dockwellv1.ListServersRequest]) (*connect.Response[dockwellv1.ListServersResponse], error) {
	return connect.NewResponse(&dockwellv1.ListServersResponse{Servers: s.servers}), nil
}

func (s *stubDockerCleanup) PreviewDockerCleanup(_ context.Context, req *connect.Request[dockwellv1.PreviewDockerCleanupRequest]) (*connect.Response[dockwellv1.PreviewDockerCleanupResponse], error) {
	s.lastPreview = req.Msg
	return connect.NewResponse(s.preview), nil
}

func (s *stubDockerCleanup) PlanDockerMaintenance(_ context.Context, req *connect.Request[dockwellv1.PlanDockerMaintenanceRequest]) (*connect.Response[dockwellv1.PlanDockerMaintenanceResponse], error) {
	s.lastPlan = req.Msg
	return connect.NewResponse(&dockwellv1.PlanDockerMaintenanceResponse{Change: s.planChange}), nil
}

func TestDockerCleanupCLIUsesSafeDefaultsAndSupportsOptIn(t *testing.T) {
	anonBytes := uint64(4 << 20)
	namedBytes := uint64(12 << 20)
	danglingBytes := uint64(8 << 20)
	taggedBytes := uint64(16 << 20)
	stub := &stubDockerCleanup{
		servers: []*dockwellv1.Server{{Id: "srv_1", Name: "alpha"}},
		preview: &dockwellv1.PreviewDockerCleanupResponse{
			Snapshot:        "snap-1",
			MaxTargetsTotal: 500,
			ServerLimits: []*dockwellv1.DockerCleanupServerLimits{{
				ServerId: "srv_1", MaxTargets: 200,
				SupportedKinds: []dockwellv1.DockerCleanupKind{
					dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE,
					dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME,
				},
			}},
			Candidates: []*dockwellv1.DockerCleanupTarget{
				{
					ServerId: "srv_1", Kind: dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE,
					EngineId: "sha256:" + strings.Repeat("a", 64),
					Reason:   dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_DANGLING,
					Evidence: "ev1", EstimatedBytes: &danglingBytes,
				},
				{
					ServerId: "srv_1", Kind: dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE,
					EngineId: "sha256:" + strings.Repeat("b", 64), Name: "example/unused:1",
					Reason:   dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED,
					Evidence: "ev2", EstimatedBytes: &taggedBytes,
				},
				{
					ServerId: "srv_1", Kind: dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME,
					EngineId: "local/" + strings.Repeat("c", 64), Name: strings.Repeat("c", 64),
					Reason:   dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_ANONYMOUS,
					Evidence: "ev3", EstimatedBytes: &anonBytes,
				},
				{
					ServerId: "srv_1", Kind: dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME,
					EngineId: "local/named-key", Name: "scratch-data",
					Reason:   dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED,
					Evidence: "ev4", EstimatedBytes: &namedBytes,
				},
			},
		},
		planChange: &dockwellv1.Change{
			Id: "chg_cleanup",
			Plan: &dockwellv1.Plan{
				Fingerprint: "fp1234567890",
				Servers: []*dockwellv1.ServerPlan{{
					ServerId: "srv_1", ServerName: "alpha",
					Steps: []*dockwellv1.PlanStep{{
						Action: dockwellv1.PlanAction_PLAN_ACTION_UPDATE,
						DockerMaintenance: &dockwellv1.DockerMaintenance{
							Targets: []*dockwellv1.DockerCleanupTarget{
								{
									ServerId: "srv_1", Kind: dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE,
									EngineId: "sha256:" + strings.Repeat("a", 64),
									Reason:   dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_DANGLING,
								},
								{
									ServerId: "srv_1", Kind: dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME,
									EngineId: "local/" + strings.Repeat("c", 64), Name: strings.Repeat("c", 64),
									Reason: dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_ANONYMOUS,
								},
							},
						},
					}},
				}},
			},
		},
	}

	mux := http.NewServeMux()
	p1, h1 := dockwellv1connect.NewServerServiceHandler(stub)
	p2, h2 := dockwellv1connect.NewDockerServiceHandler(stub)
	mux.Handle(p1, h1)
	mux.Handle(p2, h2)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	runCLI := func(opts *options, args ...string) (string, error) {
		var out, errBuf bytes.Buffer
		env := &Env{
			Out: &out, Err: &errBuf,
			Connect: func(_ context.Context, _ Settings) (*Client, error) {
				return &Client{URL: srv.URL, HTTP: srv.Client()}, nil
			},
		}
		if opts == nil {
			opts = &options{}
		}
		cmd := dockerCleanupCommand(env, opts)
		cmd.SetArgs(args)
		cmd.SetOut(&out)
		cmd.SetErr(&errBuf)
		err := cmd.Execute()
		return out.String(), err
	}

	// Default safe set: dangling image + anonymous volume only (no tagged image, no named volume).
	out, err := runCLI(nil, "--server", "alpha", "--plan-only")
	if err != nil {
		t.Fatalf("default cleanup plan-only: %v", err)
	}
	gotTargets := stub.lastPlan.GetCleanup().GetTargets()
	if len(gotTargets) != 2 {
		t.Fatalf("default selected %d targets, want 2 (dangling image + anonymous volume): %v", len(gotTargets), gotTargets)
	}
	if !strings.Contains(out, "Remove 2 Docker artifacts on alpha:") {
		t.Fatalf("unexpected plan output:\n%s", out)
	}

	// Opt-in named volumes with --kind volumes --named-volumes.
	out, err = runCLI(&options{json: true}, "--server", "alpha", "--kind", "volumes", "--named-volumes", "--plan-only")
	if err != nil {
		t.Fatalf("volumes opt-in cleanup: %v", err)
	}
	if got := stub.lastPreview.GetFilters().GetKinds(); len(got) != 1 || got[0] != dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME {
		t.Fatalf("preview kinds = %v, want [VOLUME]", got)
	}
	if got := stub.lastPlan.GetCleanup().GetTargets(); len(got) != 2 {
		t.Fatalf("volume targets selected = %d, want 2 (anonymous + named): %v", len(got), got)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("expected valid JSON output: %v\n%s", err, out)
	}
}

func TestDockerCleanupCLILimitFullPlanAndPanelLink(t *testing.T) {
	var candidates []*dockwellv1.DockerCleanupTarget
	var planTargets []*dockwellv1.DockerCleanupTarget
	for i := range 30 {
		sz := uint64((30 - i) << 20)
		tObj := &dockwellv1.DockerCleanupTarget{
			ServerId: "srv_1", Kind: dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE,
			EngineId: "sha256:" + strings.Repeat("a", 62) + string(rune('a'+i/10)) + string(rune('0'+i%10)),
			Reason:   dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_DANGLING,
			Evidence: "ev", EstimatedBytes: &sz,
		}
		candidates = append(candidates, tObj)
		planTargets = append(planTargets, tObj)
	}
	namedBytes := uint64(5 << 20)
	namedVol := &dockwellv1.DockerCleanupTarget{
		ServerId: "srv_1", Kind: dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME,
		EngineId: "local/trailing-db-vol", Name: "trailing-db-vol",
		Reason:   dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED,
		Evidence: "ev-vol", EstimatedBytes: &namedBytes,
	}
	candidates = append(candidates, namedVol)
	planTargets = append(planTargets, namedVol)

	stub := &stubDockerCleanup{
		servers: []*dockwellv1.Server{{Id: "srv_1", Name: "alpha"}},
		preview: &dockwellv1.PreviewDockerCleanupResponse{
			Snapshot:        "snap-2",
			Candidates:      candidates,
			MaxTargetsTotal: 500,
			ServerLimits: []*dockwellv1.DockerCleanupServerLimits{{
				ServerId:   "srv_1",
				MaxTargets: 200,
				SupportedKinds: []dockwellv1.DockerCleanupKind{
					dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_BUILD_CACHE,
					dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE,
					dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_NETWORK,
					dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME,
				},
			}},
		},
		planChange: &dockwellv1.Change{
			Id: "chg_fullplan",
			Plan: &dockwellv1.Plan{
				Destructive: true,
				Fingerprint: "fp-full-plan",
				Resources: []*dockwellv1.PlannedResource{{
					DockerMaintenance: &dockwellv1.DockerMaintenance{Targets: planTargets},
				}},
				Servers: []*dockwellv1.ServerPlan{{
					ServerId: "srv_1", ServerName: "alpha",
					Steps: []*dockwellv1.PlanStep{{
						Action:            dockwellv1.PlanAction_PLAN_ACTION_UPDATE,
						DockerMaintenance: &dockwellv1.DockerMaintenance{Targets: planTargets},
					}},
				}},
			},
		},
	}

	mux := http.NewServeMux()
	p1, h1 := dockwellv1connect.NewServerServiceHandler(stub)
	p2, h2 := dockwellv1connect.NewDockerServiceHandler(stub)
	mux.Handle(p1, h1)
	mux.Handle(p2, h2)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	runCLI := func(args ...string) (string, string, error) {
		var out, errBuf bytes.Buffer
		env := &Env{
			Out: &out, Err: &errBuf,
			Connect: func(_ context.Context, _ Settings) (*Client, error) {
				return &Client{URL: srv.URL, HTTP: srv.Client()}, nil
			},
		}
		cmd := dockerCleanupCommand(env, &options{})
		cmd.SetArgs(args)
		cmd.SetOut(&out)
		cmd.SetErr(&errBuf)
		err := cmd.Execute()
		return out.String(), errBuf.String(), err
	}

	// M6 & L2: --plan-only prints every target (including trailing named volume beyond 25)
	// and prints the panel passkey URL instead of dockwell changes apply.
	out, errOut, err := runCLI("--server", "alpha", "--named-volumes", "--plan-only")
	if err != nil {
		t.Fatalf("full plan-only: %v", err)
	}
	if !strings.Contains(out, "volume trailing-db-vol (named, data is deleted") {
		t.Fatalf("--plan-only omitted trailing named volume warning:\n%s", out)
	}
	if strings.Contains(out, "... and") {
		t.Fatalf("--plan-only truncated targets:\n%s", out)
	}
	if !strings.Contains(errOut, "/changes/chg_fullplan") || !strings.Contains(errOut, "passkey") || strings.Contains(errOut, "dockwell changes apply") {
		t.Fatalf("L2: --plan-only on destructive cleanup should point to panel passkey approval, got:\n%s", errOut)
	}

	// M3: --limit bounds selection (safest + largest first) and prints what stays for next time.
	_, errOut, err = runCLI("--server", "alpha", "--named-volumes", "--limit", "5", "--plan-only")
	if err != nil {
		t.Fatalf("--limit 5: %v", err)
	}
	if got := len(stub.lastPlan.GetCleanup().GetTargets()); got != 5 {
		t.Fatalf("--limit 5 selected %d targets, want 5", got)
	}
	if !strings.Contains(errOut, "Leaving 26 more matching artifacts for the next cleanup:") {
		t.Fatalf("expected deferred summary in stderr, got:\n%s", errOut)
	}
	for _, target := range candidates[5:] {
		if !strings.Contains(errOut, cleanupTargetLine(target)) {
			t.Errorf("missing deferred target %s", target.GetEngineId())
		}
	}

	// M6: compact output (non-plan-only) mentions hidden named volumes and --plan-only.
	compact := dockerMaintenanceWords(&dockwellv1.DockerMaintenance{Targets: planTargets}, "alpha", false)
	joined := strings.Join(compact, "\n")
	if !strings.Contains(joined, "1 named volume whose data will be deleted") || !strings.Contains(joined, "--plan-only") {
		t.Fatalf("compact plan output missing hidden named volume notice or --plan-only hint:\n%s", joined)
	}

	t.Run("old control plane caps the fleet and skips volumes", func(t *testing.T) {
		stub.servers = append(stub.servers, &dockwellv1.Server{Id: "srv_2", Name: "beta"})
		stub.preview.MaxTargetsTotal, stub.preview.ServerLimits = 0, nil
		for i, target := range candidates {
			if i%2 == 1 {
				target.ServerId = "srv_2"
			}
		}
		_, stderr, err := runCLI("--server", "alpha", "--server", "beta", "--named-volumes", "--plan-only")
		if err != nil {
			t.Fatal(err)
		}
		targets := stub.lastPlan.GetCleanup().GetTargets()
		if len(targets) != 12 {
			t.Fatalf("legacy cleanup selected %d targets, want 12 in total", len(targets))
		}
		servers := map[string]bool{}
		for _, target := range targets {
			if target.GetKind() == dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME {
				t.Fatalf("legacy cleanup selected volume %v", target)
			}
			servers[target.GetServerId()] = true
		}
		if len(servers) != 2 || !strings.Contains(stderr, "volume cleanup is skipped") {
			t.Fatalf("legacy cleanup = %v, stderr = %q", targets, stderr)
		}
	})
}

func TestDockerCleanupPinnedNameAndOutcomeLines(t *testing.T) {
	pinnedID := "sha256:" + strings.Repeat("a", 64)
	pinned := &dockwellv1.DockerCleanupTarget{
		Kind:         dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE,
		EngineId:     pinnedID,
		Name:         "busybox:1.36",
		Reason:       dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED,
		ImageDigests: []string{"busybox@" + pinnedID},
	}
	if got := cleanupTargetLine(pinned); !strings.Contains(got, "image busybox:1.36 (unused)") {
		t.Fatalf("pinned target line = %q, want name busybox:1.36", got)
	}
	danglingID := "sha256:" + strings.Repeat("b", 64)
	dangling := &dockwellv1.DockerCleanupTarget{
		Kind:     dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE,
		EngineId: danglingID,
		Name:     danglingID,
		Reason:   dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_DANGLING,
	}
	if got := cleanupTargetLine(dangling); !strings.Contains(got, "image bbbbbbbbbbbb (dangling)") {
		t.Fatalf("dangling target line = %q, want short ID", got)
	}

	receipts := []*dockwellv1.DockerReceipt{
		{Target: dangling, State: dockwellv1.DockerReceiptState_DOCKER_RECEIPT_STATE_REMOVED},
		{Target: pinned, State: dockwellv1.DockerReceiptState_DOCKER_RECEIPT_STATE_CHANGED, Reason: dockwellv1.DockerReceiptReason_DOCKER_RECEIPT_REASON_IN_USE},
		{
			Target: &dockwellv1.DockerCleanupTarget{ServerId: "srv_1", Kind: dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_NETWORK, EngineId: "net1", Name: "old-net"},
			State:  dockwellv1.DockerReceiptState_DOCKER_RECEIPT_STATE_CHANGED,
			Reason: dockwellv1.DockerReceiptReason_DOCKER_RECEIPT_REASON_GONE,
		},
		{
			Target: &dockwellv1.DockerCleanupTarget{ServerId: "srv_1", Kind: dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_BUILD_CACHE, EngineId: "cache1", Name: "cache1"},
			State:  dockwellv1.DockerReceiptState_DOCKER_RECEIPT_STATE_CHANGED,
			Reason: dockwellv1.DockerReceiptReason_DOCKER_RECEIPT_REASON_NEWER,
		},
	}
	pinned.ServerId = "srv_1"
	planChange := &dockwellv1.Change{
		Plan: &dockwellv1.Plan{
			Servers: []*dockwellv1.ServerPlan{{ServerId: "srv_1", ServerName: "alpha"}},
		},
	}
	joined := strings.Join(dockerCleanupOutcomeLines(receipts, dockerCleanupReviewCommandForChange(planChange, receipts)), "\n")
	for _, want := range []string{
		"1 removed · 3 changed since you reviewed. Run dockwell docker cleanup --server alpha --kind images --kind networks --kind build-cache --tagged-images to review again.",
		"- busybox:1.36: now in use",
		"- old-net: already gone",
		"- cache1: newer than preview",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("outcome lines missing %q:\n%s", want, joined)
		}
	}

	// Fleet named-volume and pinned-image flags repeat on the review-again command.
	fleetReceipts := []*dockwellv1.DockerReceipt{
		{
			Target: &dockwellv1.DockerCleanupTarget{ServerId: "srv_1", Kind: dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME, EngineId: "local/v1", Name: "v1", Reason: dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED},
			State:  dockwellv1.DockerReceiptState_DOCKER_RECEIPT_STATE_REMOVED,
		},
		{
			Target: &dockwellv1.DockerCleanupTarget{ServerId: "srv_2", Kind: dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME, EngineId: "local/v2", Name: "v2", Reason: dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED},
			State:  dockwellv1.DockerReceiptState_DOCKER_RECEIPT_STATE_CHANGED,
			Reason: dockwellv1.DockerReceiptReason_DOCKER_RECEIPT_REASON_IN_USE,
		},
	}
	fleetChange := &dockwellv1.Change{
		Id:    "chg_fleet",
		State: dockwellv1.ChangeState_CHANGE_STATE_APPLIED,
		Plan: &dockwellv1.Plan{
			Servers: []*dockwellv1.ServerPlan{
				{ServerId: "srv_1", ServerName: "alpha"},
				{ServerId: "srv_2", ServerName: "beta"},
			},
		},
		Steps: []*dockwellv1.StepProgress{{DockerReceipts: fleetReceipts}},
	}
	if got := dockerCleanupReviewCommandForChange(fleetChange, fleetReceipts); got != "dockwell docker cleanup --server beta --kind volumes --named-volumes" {
		t.Fatalf("fleet review command = %q, want beta + volumes + named-volumes", got)
	}
	var out bytes.Buffer
	r := &runner{
		env:              &Env{Out: &out},
		opts:             &options{},
		cleanupReviewCmd: dockerCleanupReviewCommand([]string{"alpha", "beta"}, []dockwellv1.DockerCleanupKind{dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME}, false, true),
	}
	if err := r.outcome(fleetChange, r.env.now()); err != nil {
		t.Fatalf("outcome: %v", err)
	}
	if !strings.Contains(out.String(), "Run dockwell docker cleanup --server alpha --server beta --kind volumes --named-volumes to review again.") {
		t.Fatalf("runner outcome did not repeat cleanup flags:\n%s", out.String())
	}
}
