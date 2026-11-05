// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"google.golang.org/protobuf/proto"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

var now = time.Date(2026, 11, 4, 12, 0, 0, 0, time.Local)

func TestPlanView(t *testing.T) {
	plan := &dockwellv1.Plan{
		Fingerprint: "4f9a",
		Summary:     &dockwellv1.PlanSummary{Create: 1, Replace: 1},
		Resources: []*dockwellv1.PlannedResource{
			{
				Id: "env_1", Resource: &dockwellv1.ResourceRef{Kind: "environment", Name: "staging"}, Action: dockwellv1.PlanAction_PLAN_ACTION_CREATE,
				Changes: []*dockwellv1.FieldChange{{Path: "display_name", Op: dockwellv1.FieldOp_FIELD_OP_ADDED, To: "Staging"}},
			},
			{
				Id: "svc_1", Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "web"}, Action: dockwellv1.PlanAction_PLAN_ACTION_REPLACE,
				Changes: []*dockwellv1.FieldChange{
					{Path: "app.image", Op: dockwellv1.FieldOp_FIELD_OP_MODIFIED, From: "ghcr.io/acme/web:1.4.2", To: "ghcr.io/acme/web:1.4.3"},
					{Path: "app.env[API_KEY]", Op: dockwellv1.FieldOp_FIELD_OP_MODIFIED, Sensitive: true},
				},
				HostAccess: []*dockwellv1.HostAccess{
					{Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PATH, Path: "/srv/app/config", ReadOnly: true},
				},
			},
			{
				Id: "vol_1", Resource: &dockwellv1.ResourceRef{Kind: "volume", Name: "media"}, Action: dockwellv1.PlanAction_PLAN_ACTION_NONE,
				HostAccess: []*dockwellv1.HostAccess{
					{Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_VOLUME_MOUNT, Type: "nfs", Address: "10.0.0.2", Path: ":/media"},
				},
			},
			{
				Id: "net_1", Resource: &dockwellv1.ResourceRef{Kind: "network", Name: "lan"}, Action: dockwellv1.PlanAction_PLAN_ACTION_NONE,
				HostAccess: []*dockwellv1.HostAccess{
					{Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_NETWORK_DRIVER, Driver: "macvlan", Path: "eth0"},
				},
			},
		},
		Servers: []*dockwellv1.ServerPlan{{
			ServerId: "srv_1", ServerName: "hz-1", Offline: true, Downtime: durationpb.New(40 * time.Second),
			UntilHealthy: durationpb.New(110 * time.Second),
			Steps: []*dockwellv1.PlanStep{{
				ResourceId: "svc_1", Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "web"}, Action: dockwellv1.PlanAction_PLAN_ACTION_REPLACE,
				Effect: dockwellv1.StepEffect_STEP_EFFECT_RECREATE, Downtime: durationpb.New(40 * time.Second), DestroysData: true,
				UntilHealthy: durationpb.New(110 * time.Second), KeptPaths: []string{"/var/lib/postgresql/data"}, Restores: true, HealthUnknown: true,
				Warnings: []dockwellv1.PlanWarning{dockwellv1.PlanWarning_PLAN_WARNING_MAY_LOCK_OUT},
				HostAccess: []*dockwellv1.HostAccess{
					{Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PATH, Path: "/srv/app/config", ReadOnly: true},
					{Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_DOCKER_SOCKET, Path: "/var/run/docker.sock"},
				},
			}},
		}},
		UndoWindow:  durationpb.New(7 * 24 * time.Hour),
		Destructive: true,
		HostAccess:  true,
	}
	var buf bytes.Buffer
	PlanView(&buf, plan)
	want := "" +
		"  + environment staging  Create\n" +
		"      + display_name     Staging\n" +
		"\n" +
		"On hz-1, offline: its steps are planned from the state it last reported, and the change can't apply until it reconnects · about 40 s of downtime, and up to 1 min 50 s until healthy\n" +
		"  ± service web           Replace · recreated · about 40 s downtime, and up to 1 min 50 s until healthy\n" +
		"      ~ app.image         ghcr.io/acme/web:1.4.2 → ghcr.io/acme/web:1.4.3\n" +
		"      ~ app.env[API_KEY]  (hidden)\n" +
		"      Takes out of use the data its image keeps at /var/lib/postgresql/data. The server keeps it for 7 days, and undoing this mounts it again.\n" +
		"      Brings back the data the server kept. If the server no longer has it, applying fails, and nothing is made empty.\n" +
		"      Gives it access to hz-1: the server path /srv/app/config (read-only) and Docker's socket at /var/run/docker.sock, which gives control of Docker on the server, the same as root. A person approves this.\n" +
		"      Its image isn't on the server yet, so its own health check isn't known, and it may take longer to be healthy.\n" +
		"      May cut off remote access to hz-1, such as SSH.\n" +
		"\n" +
		"1 to create · 0 to change · 1 to replace · 0 to delete\n" +
		"It destroys data, may cut off access, or lets two containers write to one place, which undo may not bring back.\n" +
		"It reaches more of a server, or what isn't its own yet, which a person approves:\n" +
		"  service web: the server path /srv/app/config (read-only)\n" +
		"  volume media: the nfs mount 10.0.0.2:/media (read-write)\n" +
		"  network lan: a macvlan network on eth0, which reaches past the server's firewall\n"
	if buf.String() != want {
		t.Errorf("PlanView =\n%s\nwant\n%s", buf.String(), want)
	}
	if strings.Contains(buf.String(), "s3cret") {
		t.Error("a sensitive value showed")
	}
}

// A person reading a plan learns that it takes the server's public ports, and
// what DNS says, in words.
func TestARoutesPlanSaysWhatItDoesToTheServer(t *testing.T) {
	route := &dockwellv1.ResourceRef{Kind: "route", Name: "www"}
	plan := &dockwellv1.Plan{Servers: []*dockwellv1.ServerPlan{{ServerName: "hz-1", Steps: []*dockwellv1.PlanStep{{
		Resource: route, Action: dockwellv1.PlanAction_PLAN_ACTION_CREATE,
		Ingress: &dockwellv1.IngressEffect{
			Change: dockwellv1.IngressChange_INGRESS_CHANGE_STARTS, Proxy: "Caddy", Ports: []string{"80/tcp", "443/tcp", "443/udp"}, PanelDomain: "panel.example.com",
		},
		DomainCheck: &dockwellv1.DomainCheck{Domain: "app.example.com", State: dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_ELSEWHERE, Found: []string{"198.51.100.1"}, Here: []string{"203.0.113.9"}},
	}}}}}
	var out bytes.Buffer
	PlanView(&out, plan)
	for _, want := range []string{
		"Caddy isn't running on hz-1, so this route starts it, and it takes ports 80/tcp, 443/tcp and 443/udp",
		"panel.example.com, moves behind Caddy",
		"it keeps its present certificate until Caddy has its own",
		"the SSH tunnel keeps working",
		"DNS for app.example.com points at 198.51.100.1, not this server (203.0.113.9)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the plan doesn't say %q:\n%s", want, out.String())
		}
	}

	for _, tc := range []struct {
		check *dockwellv1.DomainCheck
		want  string
	}{
		{&dockwellv1.DomainCheck{Domain: "a.example.com", State: dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_NO_RECORD, Here: []string{"203.0.113.9"}}, "has no DNS record yet"},
		{&dockwellv1.DomainCheck{Domain: "a.example.com", State: dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_CANT_TELL, Found: []string{"1.2.3.4"}, Here: []string{"10.0.0.1"}}, "can't tell"},
		{&dockwellv1.DomainCheck{Domain: "a.example.com", State: dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_POINTS_HERE}, ""},
		{nil, ""},
	} {
		got := domainWords(tc.check)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("domainWords(%v) = %q, want %q", tc.check, got, tc.want)
		}
	}
	stops := ingressWords(&dockwellv1.IngressEffect{Change: dockwellv1.IngressChange_INGRESS_CHANGE_STOPS, Proxy: "Caddy", Ports: []string{"80/tcp"}}, "hz-1")
	if len(stops) != 1 || !strings.Contains(stops[0], "Its certificates stay") {
		t.Errorf("stopping says %v", stops)
	}
}

func TestChangeView(t *testing.T) {
	c := &dockwellv1.Change{
		Id: "chg_1", Summary: "Add a staging environment", State: dockwellv1.ChangeState_CHANGE_STATE_APPLIED,
		ProposedBy: &dockwellv1.Actor{Kind: dockwellv1.ActorKind_ACTOR_KIND_USER, Name: "Mira Chen"}, ProposedVia: dockwellv1.Surface_SURFACE_PANEL,
		AppliedBy: &dockwellv1.Actor{Kind: dockwellv1.ActorKind_ACTOR_KIND_TOKEN, Name: "deploy"}, AppliedVia: dockwellv1.Surface_SURFACE_CLI,
		CreateTime: timestamppb.New(now.Add(-time.Hour)), ApplyTime: timestamppb.New(now.Add(-3 * time.Minute)),
		UndoDeadline: timestamppb.New(now.Add(7*24*time.Hour - 3*time.Minute)),
		Plan:         &dockwellv1.Plan{Summary: &dockwellv1.PlanSummary{Create: 1}},
		Steps: []*dockwellv1.StepProgress{
			{State: dockwellv1.StepState_STEP_STATE_DONE, Message: "Created container web on hz-1"},
		},
	}
	var buf bytes.Buffer
	ChangeView(&buf, c, now)
	for _, want := range []string{
		"chg_1  Add a staging environment\n",
		"Applied 3 min ago by the API token deploy from the CLI.\n",
		"1 to create · 0 to change · 0 to delete\n",
		"Steps:\n  Done  Created container web on hz-1\n",
		"Undo it within 6 days with dockwell changes undo chg_1.",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("ChangeView doesn't have %q:\n%s", want, buf.String())
		}
	}
}

// A failure reads in plain words, and what the server said is there too, for
// whoever digs in.
func TestChangeViewKeepsWhatTheServerSaid(t *testing.T) {
	c := &dockwellv1.Change{
		Id: "chg_1", Summary: "Run nginx:9", State: dockwellv1.ChangeState_CHANGE_STATE_FAILED,
		Error: "nginx:9 isn't on its registry, so it can't be pulled. Check the name and tag, then try again.",
		Plan:  &dockwellv1.Plan{Summary: &dockwellv1.PlanSummary{Create: 1}},
		Steps: []*dockwellv1.StepProgress{{
			State:   dockwellv1.StepState_STEP_STATE_FAILED,
			Message: "nginx:9 isn't on its registry, so it can't be pulled. Check the name and tag, then try again.",
			Detail:  "hz-1: can't create service web in env_01: manifest unknown",
		}},
	}
	var buf bytes.Buffer
	ChangeView(&buf, c, now)
	for _, want := range []string{
		"It failed: nginx:9 isn't on its registry, so it can't be pulled. Check the name and tag, then try again.\n",
		"What the server said: hz-1: can't create service web in env_01: manifest unknown.\n",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("ChangeView doesn't have %q:\n%s", want, buf.String())
		}
	}
}

// A failed change says how its rollback went, from what the API says of it,
// and never that what ran before is back unless it is.
func TestChangeViewSaysHowTheRollbackWent(t *testing.T) {
	deploy := &dockwellv1.Plan{
		Summary: &dockwellv1.PlanSummary{Replace: 1},
		Resources: []*dockwellv1.PlannedResource{{
			Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "web"}, Action: dockwellv1.PlanAction_PLAN_ACTION_REPLACE,
		}},
	}
	rule := &dockwellv1.Plan{
		Summary: &dockwellv1.PlanSummary{Change: 1},
		Resources: []*dockwellv1.PlannedResource{{
			Resource: &dockwellv1.ResourceRef{Kind: "firewall.rule", Name: "http"}, Action: dockwellv1.PlanAction_PLAN_ACTION_UPDATE,
		}},
	}
	first := &dockwellv1.Plan{Summary: &dockwellv1.PlanSummary{Create: 2}}
	rolling := proto.Clone(deploy).(*dockwellv1.Plan)
	rolling.Servers = []*dockwellv1.ServerPlan{{Steps: []*dockwellv1.PlanStep{{
		Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "web"}, Rollout: &dockwellv1.RolloutPlan{Rolling: true},
	}}}}
	const (
		applied  = dockwellv1.RollbackState_ROLLBACK_STATE_APPLIED
		applying = dockwellv1.RollbackState_ROLLBACK_STATE_APPLYING
		waiting  = dockwellv1.RollbackState_ROLLBACK_STATE_WAITING
		failed   = dockwellv1.RollbackState_ROLLBACK_STATE_FAILED
	)
	for _, tt := range []struct {
		name string
		plan *dockwellv1.Plan
		s    *dockwellv1.RollbackStatus
		want []string
		not  string
	}{
		{"put back", deploy, &dockwellv1.RollbackStatus{State: applied, Steps: 1, Restored: []string{"web"}}, []string{"Dockwell put back what ran before, in chg_2, and it runs again. It was down while that happened."}, ""},
		{"it rolled, and what ran never stopped", deploy, &dockwellv1.RollbackStatus{State: applied, Steps: 1}, []string{"What ran never stopped. Dockwell removed what this made, in chg_2."}, "was down"},
		{"it rolled, and something had to be made again", rolling, &dockwellv1.RollbackStatus{State: applied, Steps: 1, Restored: []string{"web"}}, []string{"Dockwell put back what ran before, in chg_2."}, "was down"},
		{"put back around what runs", deploy, &dockwellv1.RollbackStatus{State: applied, Steps: 1, Restored: []string{"data"}}, []string{"Dockwell put back what was there before, in chg_2."}, "runs again"},
		{"put back, and nothing runs", rule, &dockwellv1.RollbackStatus{State: applied, Steps: 1, Restored: []string{"http"}}, []string{"Dockwell put back what was there before, in chg_2."}, "runs again"},
		{"nothing to put back", deploy, &dockwellv1.RollbackStatus{State: applied}, []string{"It stopped before it changed anything on the servers, so what was there before still is (chg_2)."}, "put back"},
		{"a first deploy, cleared away", first, &dockwellv1.RollbackStatus{State: applied, Steps: 2}, []string{"Dockwell cleared away what it had made, in chg_2."}, "before"},
		{"a first deploy, with nothing to clear", first, &dockwellv1.RollbackStatus{State: applied}, []string{"It stopped before it changed anything on the servers (chg_2)."}, "still is"},
		{"running", deploy, &dockwellv1.RollbackStatus{State: applying, Steps: 1}, []string{"Dockwell is putting back what was there before, in chg_2. Follow it with dockwell changes watch chg_2."}, "runs again"},
		{"a first deploy, running", first, &dockwellv1.RollbackStatus{State: applying, Steps: 1}, []string{"Dockwell is clearing away what it made, in chg_2."}, "before"},
		{"waiting", deploy, &dockwellv1.RollbackStatus{State: waiting, Steps: 1, WaitingFor: []string{"hz-1"}}, []string{"Dockwell puts back what was there before, in chg_2, when it can reach hz-1. It can't tell what is running there until then."}, "runs again"},
		{"a first deploy, waiting", first, &dockwellv1.RollbackStatus{State: waiting, Steps: 1, WaitingFor: []string{"hz-1", "hz-2"}}, []string{"Dockwell clears away what it made, in chg_2, when it can reach hz-1, hz-2."}, "before"},
		{"failed", deploy, &dockwellv1.RollbackStatus{State: failed, Steps: 1, Error: "http can't be taken away", Detail: "raw"}, []string{
			"Putting back what was there before failed too, in chg_2: http can't be taken away. What the server said: raw.",
			"The servers may be left between the two, so check what runs, then deploy what should, or fix it by hand.",
		}, "runs again"},
		{"a first deploy, failed", first, &dockwellv1.RollbackStatus{State: failed, Steps: 1, Error: "http can't be taken away"}, []string{
			"Clearing away what it made failed too, in chg_2: http can't be taken away.",
			"Parts of it may still be on the servers, so check what is there, then try again, or remove them by hand.",
		}, "between the two"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.s.Id = "chg_2"
			c := &dockwellv1.Change{
				Id: "chg_1", State: dockwellv1.ChangeState_CHANGE_STATE_FAILED, Error: "it failed.", RolledBackBy: "chg_2", RollbackStatus: tt.s,
				Plan: tt.plan,
			}
			var buf bytes.Buffer
			ChangeView(&buf, c, now)
			for _, want := range tt.want {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("ChangeView doesn't have %q:\n%s", want, buf.String())
				}
			}
			if strings.Contains(buf.String(), "rolled back") || tt.not != "" && strings.Contains(buf.String(), tt.not) {
				t.Errorf("ChangeView says what it can't know:\n%s", buf.String())
			}
		})
	}
}

// With no word from the API on how it went, it says where to find out.
func TestChangeViewDoesntGuessAtARollback(t *testing.T) {
	var buf bytes.Buffer
	ChangeView(&buf, &dockwellv1.Change{Id: "chg_1", State: dockwellv1.ChangeState_CHANGE_STATE_FAILED, RolledBackBy: "chg_2"}, now)
	if want := "Dockwell made chg_2 to roll it back. See how that went with dockwell changes get chg_2."; !strings.Contains(buf.String(), want) {
		t.Errorf("ChangeView doesn't have %q:\n%s", want, buf.String())
	}
}

func TestTables(t *testing.T) {
	var buf bytes.Buffer
	Render(&buf, &dockwellv1.ListServersResponse{Servers: []*dockwellv1.Server{
		{
			Id: "srv_1", Name: "hz-1", ControlServer: true, Connection: dockwellv1.Connection_CONNECTION_CONNECTED,
			LastSeenTime: timestamppb.New(now.Add(-2 * time.Second)),
			Inventory:    &dockwellv1.ServerInventory{OsName: "Ubuntu 24.04.1 LTS"},
			Stats:        &dockwellv1.ServerStats{CpuPercent: 12.4, MemoryUsedBytes: 1288490188, MemoryTotalBytes: 4 << 30},
		},
		{Id: "srv_2", Name: "hz-2", Connection: dockwellv1.Connection_CONNECTION_PENDING},
	}}, now)
	want := "" +
		"Name            ID     Connection  OS                  CPU  Memory       Last seen\n" +
		"hz-1 (control)  srv_1  Connected   Ubuntu 24.04.1 LTS  12%  1.2 of 4 GB  2 s ago\n" +
		"hz-2            srv_2  Pending     -                   -    -            never\n"
	if buf.String() != want {
		t.Errorf("servers =\n%s\nwant\n%s", buf.String(), want)
	}

	buf.Reset()
	Render(&buf, &dockwellv1.ListTokensResponse{Tokens: []*dockwellv1.Token{{
		Id: "tok_1", Name: "deploy", Hint: "dwt_4f9K…x9Ta", Scope: dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE,
		CreateTime: timestamppb.New(now.Add(-26 * time.Hour)), ExpireTime: timestamppb.New(now.Add(89 * 24 * time.Hour)),
	}}}, now)
	for _, want := range []string{"ID     Name    Hint           Scope       Created  Expires", "Read write", "3 Nov", "on 1 Feb 2027"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("tokens don't show %q:\n%s", want, buf.String())
		}
	}

	buf.Reset()
	Render(&buf, &dockwellv1.ListTokensResponse{}, now)
	if buf.String() != "No tokens.\n" {
		t.Errorf("no tokens = %q", buf.String())
	}
}

func TestJSONIsTheAPIs(t *testing.T) {
	var buf bytes.Buffer
	msg := &dockwellv1.ListServersResponse{Servers: []*dockwellv1.Server{{Id: "srv_1", ControlServer: true, Connection: dockwellv1.Connection_CONNECTION_CONNECTED}}}
	if err := JSON(&buf, msg, true); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"servers\": [\n    {\n      \"id\": \"srv_1\",\n      \"connection\": \"CONNECTION_CONNECTED\",\n      \"controlServer\": true\n    }\n  ]\n}\n"
	if buf.String() != want {
		t.Errorf("JSON =\n%s\nwant\n%s", buf.String(), want)
	}
	buf.Reset()
	if err := JSON(&buf, msg, false); err != nil {
		t.Fatal(err)
	}
	if strings.Count(buf.String(), "\n") != 1 || !json.Valid(buf.Bytes()) {
		t.Errorf("a JSON line = %q", buf.String())
	}
}

func TestWords(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "1 s ago", 40 * time.Second: "40 s ago", 3 * time.Minute: "3 min ago", 2 * time.Hour: "2 h ago",
		-5 * time.Second: "1 s ago",
	} {
		if got := Ago(now.Add(-d), now); got != want {
			t.Errorf("Ago(%s) = %s, want %s", d, got, want)
		}
	}
	if got := Ago(time.Date(2026, 10, 12, 9, 0, 0, 0, time.Local), now); got != "12 Oct" {
		t.Errorf("Ago a month ago = %s", got)
	}
	if got := Ago(time.Date(2025, 10, 12, 9, 0, 0, 0, time.Local), now); got != "12 Oct 2025" {
		t.Errorf("Ago last year = %s", got)
	}
	for d, want := range map[time.Duration]string{
		40 * time.Second: "40 s", 12 * time.Minute: "12 min", time.Hour: "1 hour", 5 * time.Hour: "5 hours", 41 * 24 * time.Hour: "41 days",
	} {
		if got := Span(d); got != want {
			t.Errorf("Span(%s) = %s, want %s", d, got, want)
		}
	}
	for n, want := range map[uint64]string{
		512: "512 B", 512 << 20: "512 MB", 2254857830: "2.1 GB", 8 << 30: "8 GB", 18 << 30: "18 GB", 1048550: "1 MB",
	} {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %s, want %s", n, got, want)
		}
	}
	if got := BytesOf(7945689497, 8<<30); got != "7.4 of 8 GB" {
		t.Errorf("BytesOf = %s", got)
	}
	if got := Elapsed(20 * time.Millisecond); got != "under 0.1 s" {
		t.Errorf("Elapsed = %s", got)
	}
	if got := In(now.Add(3*time.Hour), now); got != "in 3 hours" {
		t.Errorf("In = %s", got)
	}
}

// Each thing that needs a person is said as what it is: a domain isn't more
// access for a container.
func TestWhatNeedsAPersonSaysWhatItIs(t *testing.T) {
	access := func(kinds ...dockwellv1.HostAccessKind) *dockwellv1.Plan {
		p := &dockwellv1.Plan{HostAccess: true}
		for _, k := range kinds {
			p.Resources = append(p.Resources, &dockwellv1.PlannedResource{HostAccess: []*dockwellv1.HostAccess{{Kind: k, Path: "app.example.com", Detail: "Caddy"}}})
		}
		return p
	}
	for _, tc := range []struct {
		name string
		plan *dockwellv1.Plan
		want string
	}{
		{"a takeover", access(dockwellv1.HostAccessKind_HOST_ACCESS_KIND_DOMAIN), "takes a domain another environment held"},
		{"the panel", access(dockwellv1.HostAccessKind_HOST_ACCESS_KIND_PANEL_DOMAIN), "moves the panel's domain behind Caddy"},
		{"a server path", access(dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PATH), "gives a container more access to its server"},
		{"destructive", &dockwellv1.Plan{Destructive: true}, "destroys data, may cut off access, or lets two containers write to one place"},
		{"both", access(dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PATH, dockwellv1.HostAccessKind_HOST_ACCESS_KIND_DOMAIN, dockwellv1.HostAccessKind_HOST_ACCESS_KIND_DOMAIN), "gives a container more access to its server and takes a domain another environment held"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := whyPerson(tc.plan); got != tc.want {
				t.Errorf("whyPerson = %q, want %q", got, tc.want)
			}
		})
	}
	step := reachWords([]*dockwellv1.HostAccess{{Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_PANEL_DOMAIN, Path: "panel.example.com", Detail: "Caddy"}}, "hz-1")
	if !strings.Contains(step, "the panel's domain, panel.example.com, which moves behind Caddy") || strings.Contains(step, "access to hz-1") {
		t.Errorf("the step says %q", step)
	}
	step = reachWords([]*dockwellv1.HostAccess{{Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_DOMAIN, Path: "app.example.com", Detail: "production"}}, "hz-1")
	if !strings.Contains(step, "which the environment production held") || strings.Contains(step, "access to hz-1") {
		t.Errorf("the step says %q", step)
	}
}

// A plan says what would take the proxy's ports back at the next reboot, with what
// to do, and doesn't refuse.
func TestARoutesPlanSaysWhatComesBackAfterAReboot(t *testing.T) {
	plan := &dockwellv1.Plan{Servers: []*dockwellv1.ServerPlan{{ServerName: "hz-1", Steps: []*dockwellv1.PlanStep{{
		Resource: &dockwellv1.ResourceRef{Kind: "route", Name: "www"}, Action: dockwellv1.PlanAction_PLAN_ACTION_CREATE,
		ReturningContainers: []string{"1Panel-openresty-abc12", "old-nginx"},
	}}}}}
	var out bytes.Buffer
	PlanView(&out, plan)
	for _, want := range []string{
		"Docker starts 1Panel-openresty-abc12 and old-nginx again when hz-1 next restarts",
		"docker update --restart=no 1Panel-openresty-abc12 old-nginx",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the plan doesn't say %q:\n%s", want, out.String())
		}
	}
}

// What a plan's notes say reaches the terminal as it does the panel.
func TestPlanViewSaysItsNotes(t *testing.T) {
	var b strings.Builder
	PlanView(&b, &dockwellv1.Plan{Notes: []*dockwellv1.PlanNote{{Reason: "volumes_stay", Message: "web keeps what is in data as it is"}}})
	if !strings.Contains(b.String(), "Note: web keeps what is in data as it is.\n") {
		t.Errorf("the plan reads:\n%s", b.String())
	}
}

// A plan says where it lands above its steps, once there's a choice of it.
func TestPlanViewSaysWhereItLands(t *testing.T) {
	for _, c := range []struct {
		places []*dockwellv1.PlanPlace
		want   string
	}{
		{nil, ""},
		{[]*dockwellv1.PlanPlace{{Project: "shop"}}, "In project shop.\n\n"},
		{[]*dockwellv1.PlanPlace{{Environment: "staging"}}, "In environment staging.\n\n"},
		{[]*dockwellv1.PlanPlace{{Project: "shop", Environment: "staging"}}, "In project shop, environment staging.\n\n"},
		{
			[]*dockwellv1.PlanPlace{{Project: "shop", Environment: "staging"}, {Project: "shop", Environment: "production"}},
			"In project shop, environment staging.\nIn project shop, environment production.\n\n",
		},
	} {
		var buf bytes.Buffer
		PlanView(&buf, &dockwellv1.Plan{Places: c.places})
		if got := buf.String(); !strings.HasPrefix(got, c.want+"0 to create") {
			t.Errorf("for %v, PlanView = %q, want it to start with %q", c.places, got, c.want)
		}
	}
}

// Every kind of access the API can name has words in the CLI, or a person would
// approve what it can't say.
func TestEveryAccessKindHasWords(t *testing.T) {
	values := dockwellv1.HostAccessKind(0).Descriptor().Values()
	for i := range values.Len() {
		kind := dockwellv1.HostAccessKind(values.Get(i).Number())
		if kind == dockwellv1.HostAccessKind_HOST_ACCESS_KIND_UNSPECIFIED {
			continue
		}
		if got := accessWords([]*dockwellv1.HostAccess{{Kind: kind, Path: "x"}})[0]; strings.HasPrefix(got, "access(") {
			t.Errorf("%s has no words in the CLI: %s", kind, got)
		}
	}
}

// The CLI can't apply what is as good as root, and still says what the panel does
// of it: that it is, and where a symlink leads, as it does of an ordinary folder.
func TestTheCLISaysWhatIsAsGoodAsRootAndWhereAPathLeads(t *testing.T) {
	for want, a := range map[string]*dockwellv1.HostAccess{
		"the server path /etc (read-write), which is as good as root":                                                                               {Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PATH, Path: "/etc", SameAsRoot: true},
		"the server path /srv/conf, which leads to /etc (read-only), which is as good as root":                                                      {Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PATH, Path: "/srv/conf", ReadOnly: true, LeadsTo: "/etc", SameAsRoot: true},
		"the server path /srv/current, which leads to /srv/9 (read-write)":                                                                          {Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PATH, Path: "/srv/current", LeadsTo: "/srv/9"},
		"the server path /srv/site (read-only)":                                                                                                     {Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PATH, Path: "/srv/site", ReadOnly: true},
		"the server path /srv/s, which leads to Docker's socket at /run/docker.sock, which gives control of Docker on the server, the same as root": {Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_DOCKER_SOCKET, Path: "/srv/s", LeadsTo: "/run/docker.sock", SameAsRoot: true},
	} {
		if got := accessWords([]*dockwellv1.HostAccess{a})[0]; got != want {
			t.Errorf("it reads %q, want %q", got, want)
		}
	}
}

// A pull with nothing else to do doesn't say it replaces for certain.
func TestPlanViewSaysAPullReplacesOnlyIfNewer(t *testing.T) {
	step := func(changes ...*dockwellv1.FieldChange) *dockwellv1.Plan {
		return &dockwellv1.Plan{Servers: []*dockwellv1.ServerPlan{{ServerName: "hz-1", Steps: []*dockwellv1.PlanStep{{
			Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "web"}, Action: dockwellv1.PlanAction_PLAN_ACTION_REPLACE,
			Effect: dockwellv1.StepEffect_STEP_EFFECT_RECREATE, Pull: true, Changes: changes,
		}}}}}
	}
	var out bytes.Buffer
	PlanView(&out, step())
	if !strings.Contains(out.String(), "Replace if newer") || strings.Contains(out.String(), "recreated") {
		t.Errorf("a pull with nothing else to do reads:\n%s", out.String())
	}
	out.Reset()
	PlanView(&out, step(&dockwellv1.FieldChange{Path: "app.image", From: "a@sha256:1", To: "a"}))
	if strings.Contains(out.String(), "if newer") {
		t.Errorf("a pull with a change to make replaces for certain, and reads:\n%s", out.String())
	}
}

// What a pull may not do isn't counted as done, or offered back.
func TestAPlanSaysWhatAPullOnlyMayDo(t *testing.T) {
	now := time.Now()
	planOf := func(replace int32, st ...*dockwellv1.PlanStep) *dockwellv1.Change {
		return &dockwellv1.Change{
			Id: "chg_1", State: dockwellv1.ChangeState_CHANGE_STATE_PLANNED,
			Plan: &dockwellv1.Plan{
				Summary:    &dockwellv1.PlanSummary{Replace: replace},
				UndoWindow: durationpb.New(7 * 24 * time.Hour),
				Servers:    []*dockwellv1.ServerPlan{{ServerName: "hz-1", Steps: st}},
			},
		}
	}
	web := &dockwellv1.ResourceRef{Kind: "service", Name: "web"}
	replace := dockwellv1.PlanAction_PLAN_ACTION_REPLACE
	restart := &dockwellv1.FieldChange{Path: "app.restart", From: "always", To: ""}
	for _, tc := range []struct {
		name          string
		change        *dockwellv1.Change
		want, wantNot []string
	}{
		{
			name:    "a pull with nothing else to do",
			change:  planOf(1, &dockwellv1.PlanStep{Resource: web, Action: replace, Pull: true}),
			want:    []string{"1 to replace if newer", "Undo for 7 days if this replaces anything.", "Replace if newer"},
			wantNot: []string{"once applied"},
		},
		{
			name:   "a pull with a restart policy to put right",
			change: planOf(1, &dockwellv1.PlanStep{Resource: web, Action: replace, Pull: true, Changes: []*dockwellv1.FieldChange{restart}}),
			want:   []string{"1 to replace if newer", "Update, and replace if newer", "Undo for 7 days if this replaces anything."},
		},
		{
			name:   "a pull with a limit to put right",
			change: planOf(1, &dockwellv1.PlanStep{Resource: web, Action: replace, Pull: true, Changes: []*dockwellv1.FieldChange{{Path: "app.resources.memory", Op: dockwellv1.FieldOp_FIELD_OP_MODIFIED, From: "1", To: "2"}}}),
			want:   []string{"1 to replace if newer", "Update, and replace if newer"},
		},
		{
			name:    "a pull with a limit to lift, which takes a new container",
			change:  planOf(1, &dockwellv1.PlanStep{Resource: web, Action: replace, Pull: true, Changes: []*dockwellv1.FieldChange{{Path: "app.resources.memory", Op: dockwellv1.FieldOp_FIELD_OP_REMOVED, From: "1"}}}),
			want:    []string{"1 to replace ·"},
			wantNot: []string{"if newer"},
		},
		{
			name:    "a pull with its image to change as well",
			change:  planOf(1, &dockwellv1.PlanStep{Resource: web, Action: replace, Pull: true, Changes: []*dockwellv1.FieldChange{{Path: "app.image", From: "a@sha256:1", To: "a"}}}),
			want:    []string{"1 to replace ·", "Undo for 7 days once applied."},
			wantNot: []string{"if newer", "if this replaces"},
		},
		{
			name:    "a replace with no pull",
			change:  planOf(1, &dockwellv1.PlanStep{Resource: web, Action: replace}),
			want:    []string{"1 to replace ·", "Undo for 7 days once applied."},
			wantNot: []string{"if newer"},
		},
		{
			name: "on two servers, where one has to replace",
			change: planOf(1, &dockwellv1.PlanStep{Resource: web, Action: replace, Pull: true},
				&dockwellv1.PlanStep{Resource: web, Action: replace, Pull: true, Changes: []*dockwellv1.FieldChange{{Path: "app.env[A]"}}}),
			want:    []string{"1 to replace ·", "Undo for 7 days once applied."},
			wantNot: []string{"1 to replace if newer"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			ChangeView(&out, tc.change, now)
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("it doesn't say %q:\n%s", w, out.String())
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(out.String(), w) {
					t.Errorf("it says %q:\n%s", w, out.String())
				}
			}
		})
	}
}

// A value anyone can read is the most a note can say, short of stopping anyone, so
// it stands out from one that isn't.
func TestPlanViewWarnsOfAValueInPlainSight(t *testing.T) {
	var buf bytes.Buffer
	PlanView(&buf, &dockwellv1.Plan{Notes: []*dockwellv1.PlanNote{
		{Reason: "exposed_variable", Message: "REDIS_PASSWORD from your .env is in redis's command"},
		{Reason: "exposed_more", Message: "The file writes more variables in plain sight"},
		{Reason: "no_build", Message: "web has build beside image"},
	}})
	for _, want := range []string{
		"Warning: REDIS_PASSWORD from your .env is in redis's command.\n",
		"Warning: The file writes more variables in plain sight.\n",
		"Note: web has build beside image.\n",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("PlanView doesn't say %q:\n%s", want, buf.String())
		}
	}
}

// A plain route's plan says what that means, and a port on the server and a proxy
// to believe say what a person approves, each as what it is.
func TestARoutesPlanSaysWhatIsUnencryptedAndWhatItReaches(t *testing.T) {
	plan := &dockwellv1.Plan{HostAccess: true, Servers: []*dockwellv1.ServerPlan{{ServerName: "hz-1", Steps: []*dockwellv1.PlanStep{{
		Resource: &dockwellv1.ResourceRef{Kind: "route", Name: "www"}, Action: dockwellv1.PlanAction_PLAN_ACTION_CREATE, Unencrypted: true,
		Answers:    &dockwellv1.RouteAnswers{Reach: dockwellv1.RouteReach_ROUTE_REACH_THIS_SERVER, Plain: true},
		ServerPort: &dockwellv1.ServerPortCheck{Port: 8080, Listening: "nginx"},
		Ingress:    &dockwellv1.IngressEffect{Change: dockwellv1.IngressChange_INGRESS_CHANGE_STARTS, Proxy: "Caddy", Ports: []string{"80/tcp"}},
		HostAccess: []*dockwellv1.HostAccess{
			{Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PORT, Path: "127.0.0.1:8080", Detail: "app.example.com"},
			{Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_TRUSTED_PROXY, Path: "cloudflare"},
		},
	}}}}}
	var out bytes.Buffer
	PlanView(&out, plan)
	for _, want := range []string{
		"Traffic for this route reaches hz-1 unencrypted, as TLS ends before it. That's fine on a tunnel or a private network, and not on the open internet",
		"so this route starts it, and it takes ports 80/tcp",
		"It answers only what runs on hz-1 itself, such as a tunnel on its own network",
		"That lets what runs on hz-1 itself reach port 8080",
		"Listening there now: nginx.",
		"Gives it access to hz-1: the port 127.0.0.1:8080 on the server itself, past its containers, at app.example.com",
		"Takes in cloudflare as a proxy it believes on where a request came from",
		"A person approves this",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the plan doesn't say %q:\n%s", want, out.String())
		}
	}
}

// What a plan says beside its steps, the CLI says too.
func TestPlanViewSaysNotes(t *testing.T) {
	var buf bytes.Buffer
	PlanView(&buf, &dockwellv1.Plan{Notes: []*dockwellv1.PlanNote{{Reason: "no_build", Message: "web has build beside image, and Dockwell builds nothing."}}})
	if !strings.Contains(buf.String(), "Note: web has build beside image, and Dockwell builds nothing.\n") {
		t.Errorf("PlanView =\n%s", buf.String())
	}
}

// Who a route answers, and what a port on the server gets from that, each way.
func TestARoutesPlanSaysWhoItAnswers(t *testing.T) {
	proxies := &dockwellv1.RouteAnswers{Reach: dockwellv1.RouteReach_ROUTE_REACH_PROXIES, Proxies: []string{"127.0.0.1/32"}, Plain: true}
	for _, tc := range []struct {
		name string
		a    *dockwellv1.RouteAnswers
		port *dockwellv1.ServerPortCheck
		want []string
	}{
		{
			"this server", &dockwellv1.RouteAnswers{Reach: dockwellv1.RouteReach_ROUTE_REACH_THIS_SERVER, Plain: true}, nil,
			[]string{"It answers only what runs on hz-1 itself, such as a tunnel on its own network. Anyone else gets what a name with no route gets."},
		},
		{"proxies", proxies, nil, []string{"It answers only 127.0.0.1/32. Anyone else gets what a name with no route gets."}},
		{
			"anyone, plain", &dockwellv1.RouteAnswers{Reach: dockwellv1.RouteReach_ROUTE_REACH_ANYONE, Plain: true}, nil,
			[]string{"It answers anyone who can reach hz-1 on port 80, over plain HTTP, around whatever is in front of it."},
		},
		{"a port, with a certificate, and what's there", &dockwellv1.RouteAnswers{Reach: dockwellv1.RouteReach_ROUTE_REACH_ANYONE}, &dockwellv1.ServerPortCheck{Port: 5432, Listening: "postgres"}, []string{
			"That lets anyone on the internet reach port 5432, and what's there sees each caller as this server, 127.0.0.1, which an app that trusts localhost will believe.",
			"Listening there now: postgres.",
		}},
		{"a port through proxies, nothing there", proxies, &dockwellv1.ServerPortCheck{Port: 8080, NothingListening: true}, []string{
			"It answers only 127.0.0.1/32. Anyone else gets what a name with no route gets.",
			"That lets whoever comes through 127.0.0.1/32 reach port 8080, and what's there sees each caller as this server, 127.0.0.1, which an app that trusts localhost will believe.",
			"Nothing listens on port 8080 yet, so this answers 502 until something does.",
		}},
		{"a port, when the server couldn't say", &dockwellv1.RouteAnswers{Reach: dockwellv1.RouteReach_ROUTE_REACH_ANYONE}, &dockwellv1.ServerPortCheck{Port: 8080}, []string{
			"That lets anyone on the internet reach port 8080, and what's there sees each caller as this server, 127.0.0.1, which an app that trusts localhost will believe.",
		}},
		{"nothing to say", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := answersWords(tc.a, tc.port, "hz-1"); !slices.Equal(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// What a plan says of how a step replaces a service: what it risks, and what follows
// if it fails, which isn't the same for a restart, as it has nothing to roll back.
func TestRolloutWordsSayWhatFollows(t *testing.T) {
	soak, within := durationpb.New(5*time.Second), durationpb.New(90*time.Second)
	for name, tc := range map[string]struct {
		plan    *dockwellv1.RolloutPlan
		restart bool
		stopped bool
		want    []string
		not     []string
	}{
		"a deploy that rolls": {
			plan: &dockwellv1.RolloutPlan{Rolling: true, Soak: soak, HealthyWithin: within},
			want: []string{"has stayed so for 5 s", "Dockwell rolls this back"},
		},
		"a restart that rolls": {
			plan: &dockwellv1.RolloutPlan{Rolling: true, Soak: soak, HealthyWithin: within}, restart: true,
			want: []string{"the new one goes, and what runs stays"}, not: []string{"rolls this back"},
		},
		"what someone stopped": {
			plan: &dockwellv1.RolloutPlan{Rolling: true, Soak: soak, HealthyWithin: within, Others: []string{"web-1"}, EarlyNetworks: []string{"back"}}, stopped: true,
			want: []string{"It has to stay healthy for 5 s", "Dockwell rolls this back"}, not: []string{"goes on serving", "in flight", "beside the old one"},
		},
		"what someone stopped, where a replace has to stop first": {
			plan: &dockwellv1.RolloutPlan{Ports: []string{"8080/tcp"}, Volumes: []string{"data"}, HostNetwork: true, Chosen: true}, stopped: true, restart: true,
			not: []string{"stops first", "stop first", "Two containers"},
		},
		"a restart of what someone stopped": {
			plan: &dockwellv1.RolloutPlan{Rolling: true, Soak: soak, HealthyWithin: within}, stopped: true, restart: true,
			want: []string{"what was stopped stays stopped"}, not: []string{"what runs", "rolls this back"},
		},
		"a restart that stops first": {
			plan: &dockwellv1.RolloutPlan{ImageData: []string{"/data"}}, restart: true, want: []string{"a restart has nothing to go back to"},
		},
		"what it can't read":     {plan: &dockwellv1.RolloutPlan{CantTell: []string{"nfs-share"}}, want: []string{"can't read where nfs-share really lands"}},
		"a route edited by hand": {plan: &dockwellv1.RolloutPlan{EditedRoutes: []string{"www"}}, want: []string{"The route www was edited by hand", "keep the old one's address"}},
		"two writers, as chosen": {
			plan: &dockwellv1.RolloutPlan{Rolling: true, Chosen: true, ImageData: []string{"/data"}, HealthyWithin: within},
			want: []string{"two containers may write to /data", "a person approves"},
		},
		"other services, early": {plan: &dockwellv1.RolloutPlan{Rolling: true, HealthyWithin: within, EarlyNetworks: []string{"back"}}, want: []string{"On back, other services can reach the new container by its service's name before"}},
		"other services, early, by other names": {
			plan: &dockwellv1.RolloutPlan{Rolling: true, HealthyWithin: within, EarlyNetworks: []string{"back", "edge"}, EarlyNames: []string{"db"}},
			want: []string{"On back, edge, other services can reach the new container by its service's name and by db before"},
		},
		"the server's own network": {plan: &dockwellv1.RolloutPlan{HostNetwork: true}, want: []string{"listens on the server's ports itself, so two can't run at once, and the old one stops first"}},
		"a volume dropped": {
			plan: &dockwellv1.RolloutPlan{Rolling: true, HealthyWithin: within, Dropped: []*dockwellv1.DroppedMount{{Source: "cache-data", Target: "/data", EmptyVolume: true}, {Source: "/srv/x", Target: "/x"}}},
			want: []string{"cache-data stops being what the service sees at /data, and its image puts an empty volume there", "/srv/x stops being what the service sees at /x."},
		},
	} {
		t.Run(name, func(t *testing.T) {
			said := strings.Join(rolloutWords(tc.plan, tc.restart, tc.stopped), " ")
			for _, w := range tc.want {
				if !strings.Contains(said, w) {
					t.Errorf("it says %q, want %q", said, w)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(said, w) {
					t.Errorf("it says %q, and shouldn't say %q", said, w)
				}
			}
		})
	}
}

// Taking a domain off HTTPS says what stops, and that's a person's to approve.
func TestAPlanSaysWhatTakingADomainOffHTTPSDoes(t *testing.T) {
	got := reachWords([]*dockwellv1.HostAccess{{Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_HTTPS_OFF, Path: "live.example.com"}}, "hz-1")
	for _, want := range []string{"HTTPS on live.example.com, which stops", "https:// addresses get an error", "browsers that saw HSTS", "A person approves this"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q doesn't say %q", got, want)
		}
	}
	if strings.Contains(got, "access to hz-1") {
		t.Errorf("%q reads as access to the server", got)
	}
}

func TestARoutesPlanSaysWhatPortsItTakesAndFrees(t *testing.T) {
	for change, want := range map[dockwellv1.IngressChange]string{
		dockwellv1.IngressChange_INGRESS_CHANGE_TAKES_PORTS: "This is the first route on hz-1 that takes a certificate, so Caddy takes ports 443/tcp and 443/udp too, and nothing it serves stops.",
		dockwellv1.IngressChange_INGRESS_CHANGE_FREES_PORTS: "No route on hz-1 takes a certificate after this, so Caddy frees ports 443/tcp and 443/udp. Its routes that take none go on answering.",
	} {
		got := ingressWords(&dockwellv1.IngressEffect{Change: change, Proxy: "Caddy", Ports: []string{"443/tcp", "443/udp"}}, "hz-1")
		if len(got) != 1 || got[0] != want {
			t.Errorf("%v says %q, want %q", change, got, want)
		}
	}
}

// Behind a tunnel, a domain points somewhere else, and what DNS says there leads to
// the setting as well as to moving DNS.
func TestDNSThatPointsElsewhereNamesTheSetting(t *testing.T) {
	for _, state := range []dockwellv1.DomainCheckState{dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_ELSEWHERE, dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_CANT_TELL} {
		got := domainWords(&dockwellv1.DomainCheck{Domain: "app.example.com", State: state, Found: []string{"198.51.100.1"}, Here: []string{"10.0.0.2"}})
		if !strings.Contains(got, "set tls to off on this route") {
			t.Errorf("%v says %q", state, got)
		}
	}
}

func TestARouteMovingSaysWhichAppItLeaves(t *testing.T) {
	if got := movesWords(&dockwellv1.RouteMove{Where: "app.example.com", From: "api", To: "nginx"}); got != "app.example.com leaves api, and goes to nginx." {
		t.Errorf("it says %q", got)
	}
	if got := movesWords(&dockwellv1.RouteMove{Where: "app.example.com", From: "api", ToPort: 8080}); got != "app.example.com leaves api, and goes to port 8080 on the server." {
		t.Errorf("to a port, it says %q", got)
	}
	if got := movesWords(nil); got != "" {
		t.Errorf("no move says %q", got)
	}
}

// A plan's "no downtime" says nothing of a route going, so its line does.
func TestARouteGoingSaysWhatAVisitorFinds(t *testing.T) {
	for find, want := range map[dockwellv1.VisitorsFind]string{
		dockwellv1.VisitorsFind_VISITORS_FIND_REFUSED:        "gets a refused connection",
		dockwellv1.VisitorsFind_VISITORS_FIND_TLS_ERROR:      "browser's connection error",
		dockwellv1.VisitorsFind_VISITORS_FIND_NOT_FOUND:      "gets a 404",
		dockwellv1.VisitorsFind_VISITORS_FIND_REST:           "what the rest of app.example.com serves",
		dockwellv1.VisitorsFind_VISITORS_FIND_NOTHING_BEHIND: "whatever it shows for that",
		dockwellv1.VisitorsFind_VISITORS_FIND_UNSPECIFIED:    "stops reaching it",
	} {
		got := goesWords(&dockwellv1.RouteGoes{Where: "app.example.com/api", Domain: "app.example.com", Find: find})
		if !strings.Contains(got, want) || !strings.Contains(got, "app.example.com/api") {
			t.Errorf("%v says %q, want %q", find, got, want)
		}
	}
	if got := goesWords(nil); got != "" {
		t.Errorf("no route going says %q", got)
	}
}

// Changing a route reloads the proxy, and what that costs reaches every site on the
// server, so a plan says so. A step that starts or stops the proxy doesn't reload it,
// and a rollout, which moves traffic with no reload, isn't a route's step.
func TestAPlanSaysAReloadCostsEverySite(t *testing.T) {
	route := func(action dockwellv1.PlanAction, change dockwellv1.IngressChange) *dockwellv1.PlanStep {
		return &dockwellv1.PlanStep{
			Resource: &dockwellv1.ResourceRef{Kind: "route", Name: "www"}, Action: action,
			Ingress: &dockwellv1.IngressEffect{Change: change, Proxy: "Caddy"},
		}
	}
	for name, tc := range map[string]struct {
		step *dockwellv1.PlanStep
		want bool
	}{
		"a route that changes": {route(dockwellv1.PlanAction_PLAN_ACTION_UPDATE, dockwellv1.IngressChange_INGRESS_CHANGE_UNSPECIFIED), true},
		"a route added":        {&dockwellv1.PlanStep{Resource: &dockwellv1.ResourceRef{Kind: "route", Name: "www"}, Action: dockwellv1.PlanAction_PLAN_ACTION_CREATE}, true},
		"the first route":      {route(dockwellv1.PlanAction_PLAN_ACTION_CREATE, dockwellv1.IngressChange_INGRESS_CHANGE_STARTS), false},
		"the last route":       {route(dockwellv1.PlanAction_PLAN_ACTION_DELETE, dockwellv1.IngressChange_INGRESS_CHANGE_STOPS), false},
		"a route as it is":     {route(dockwellv1.PlanAction_PLAN_ACTION_NONE, dockwellv1.IngressChange_INGRESS_CHANGE_UNSPECIFIED), false},
		"a service":            {&dockwellv1.PlanStep{Resource: &dockwellv1.ResourceRef{Kind: "service", Name: "web"}, Action: dockwellv1.PlanAction_PLAN_ACTION_REPLACE}, false},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			PlanView(&buf, &dockwellv1.Plan{Servers: []*dockwellv1.ServerPlan{{ServerName: "hz-1", Steps: []*dockwellv1.PlanStep{tc.step}}}})
			if got := strings.Contains(buf.String(), "This reloads the proxy on hz-1"); got != tc.want {
				t.Errorf("it says, want it said %t:\n%s", tc.want, buf.String())
			}
		})
	}
}

// The CLI says what a check said on a failed deploy and on its rollback, as it was said.
func TestAChangeSaysWhatACheckSaid(t *testing.T) {
	const said = "app runs, but fails its health check: GET /healthz on port 8080 answered 503 Service Unavailable. Fix what it answers, or the check, then try again."
	var failed strings.Builder
	ChangeView(&failed, &dockwellv1.Change{Id: "chg_1", State: dockwellv1.ChangeState_CHANGE_STATE_FAILED, Error: said, Plan: &dockwellv1.Plan{}}, time.Now())
	if !strings.Contains(failed.String(), "It failed: "+said+"\n") {
		t.Errorf("a failed deploy reads:\n%s", failed.String())
	}
	var back strings.Builder
	ChangeView(&back, &dockwellv1.Change{Id: "chg_2", State: dockwellv1.ChangeState_CHANGE_STATE_APPLIED, Reverts: "chg_1", Rollback: true, Because: said, Plan: &dockwellv1.Plan{}}, time.Now())
	if !strings.Contains(back.String(), "Dockwell made this change because of what failed: "+said+"\n") {
		t.Errorf("its rollback reads:\n%s", back.String())
	}
}

// A plan's step says what's so of it: a restart has no rollback, and nothing goes on
// serving from what's stopped.
func TestAPlanSaysWhatsSoOfItsStep(t *testing.T) {
	rollout := &dockwellv1.RolloutPlan{Rolling: true, Soak: durationpb.New(5 * time.Second), HealthyWithin: durationpb.New(time.Minute), Others: []string{"web-1"}}
	view := func(st *dockwellv1.PlanStep) string {
		st.Resource, st.Rollout, st.Action = &dockwellv1.ResourceRef{Kind: "service", Name: "web"}, rollout, dockwellv1.PlanAction_PLAN_ACTION_REPLACE
		var b strings.Builder
		PlanView(&b, &dockwellv1.Plan{Servers: []*dockwellv1.ServerPlan{{ServerName: "hz-1", Steps: []*dockwellv1.PlanStep{st}}}})
		return b.String()
	}
	if got := view(&dockwellv1.PlanStep{Restart: "chg_1"}); !strings.Contains(got, "the new one goes, and what runs stays") || strings.Contains(got, "rolls this back") {
		t.Errorf("a restart's plan reads:\n%s", got)
	}
	stopped := &dockwellv1.PlanStep{Changes: []*dockwellv1.FieldChange{{Path: "state", From: "exited", To: "running"}}}
	if got := view(stopped); !strings.Contains(got, "It has to stay healthy") || strings.Contains(got, "goes on serving") || strings.Contains(got, "in flight") {
		t.Errorf("a stopped container's plan reads:\n%s", got)
	}
}
