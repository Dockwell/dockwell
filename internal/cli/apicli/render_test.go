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
		"1 to create · 0 to change · 1 to replace · 0 to destroy\n" +
		"It destroys data or may cut off access, which undo may not bring back.\n" +
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
		"1 to create · 0 to change · 0 to destroy\n",
		"Steps:\n  Done  Created container web on hz-1\n",
		"Undo it within 6 days with dockwell changes undo chg_1.",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("ChangeView doesn't have %q:\n%s", want, buf.String())
		}
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
		{"destructive", &dockwellv1.Plan{Destructive: true}, "destroys data or may cut off access"},
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
