// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// parse binds fields of msg's type to flags, parses args, and sets msg.
func parse(t *testing.T, env *Env, msg proto.Message, fields []Field, args ...string) error {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	b, err := bind(fs, env, msg.ProtoReflect().Descriptor(), fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	return b.apply(msg.ProtoReflect(), fs.Args())
}

func TestFlagsFromRequestFields(t *testing.T) {
	env := &Env{Getenv: func(string) string { return "" }, In: strings.NewReader("")}
	fields := []Field{{Path: "name"}, {Path: "scope"}, {Path: "expire_time"}, {Path: "ttl"}}

	var req dockwellv1.CreateTokenRequest
	if err := parse(t, env, &req, fields, "--name", "GitHub Actions deploy", "--scope", "read-write", "--ttl", "90d"); err != nil {
		t.Fatal(err)
	}
	want := &dockwellv1.CreateTokenRequest{
		Name: "GitHub Actions deploy", Scope: dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE,
		Expiration: &dockwellv1.CreateTokenRequest_Ttl{Ttl: durationpb.New(90 * 24 * time.Hour)},
	}
	if !proto.Equal(&req, want) {
		t.Errorf("parsed %v, want %v", &req, want)
	}

	req.Reset()
	if err := parse(t, env, &req, fields, "--scope", "TOKEN_SCOPE_READ_ONLY", "--expire-time", "2026-12-01T09:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if req.GetScope() != dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY || req.GetExpireTime().AsTime() != time.Date(2026, 12, 1, 9, 0, 0, 0, time.UTC) {
		t.Errorf("parsed %v", &req)
	}

	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"--scope", "admin"}, `"admin" isn't one of read-only, read-write`},
		{[]string{"--ttl", "soon"}, `"soon" isn't a duration such as 90d, 12h or 30m`},
		{[]string{"--expire-time", "tomorrow"}, "isn't a time such as 2026-12-01T09:00:00Z"},
		{[]string{"--ttl", "1h", "--expire-time", "2026-12-01T09:00:00Z"}, "--expire-time and --ttl can't go together"},
	} {
		req.Reset()
		err := parse(t, env, &req, fields, tt.args...)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v = %v, want an error with %q", tt.args, err, tt.want)
		}
	}
}

func TestTheArgumentAndNumbers(t *testing.T) {
	env := &Env{Getenv: func(string) string { return "" }}
	var apply dockwellv1.ApplyChangeRequest
	if err := parse(t, env, &apply, []Field{{Path: "id", Arg: true}, {Path: "fingerprint"}}, "chg_1", "--fingerprint", "4f9a"); err != nil {
		t.Fatal(err)
	}
	if apply.GetId() != "chg_1" || apply.GetFingerprint() != "4f9a" {
		t.Errorf("parsed %v", &apply)
	}
	var list dockwellv1.ListChangesRequest
	if err := parse(t, env, &list, []Field{{Path: "page_size"}}, "--page-size", "20"); err != nil || list.GetPageSize() != 20 {
		t.Errorf("--page-size 20 = %v, %v", &list, err)
	}
	if err := parse(t, env, &list, []Field{{Path: "page_size"}}, "--page-size", "many"); err == nil || !strings.Contains(err.Error(), "isn't a whole number") {
		t.Errorf("--page-size many = %v", err)
	}
}

func TestFlagsFromASpec(t *testing.T) {
	secretEnv := map[string]string{"API_KEY": "s3cret"}
	dir := t.TempDir()
	ports := filepath.Join(dir, "ports.json")
	if err := os.WriteFile(ports, []byte(`{"hostPort": 8443, "containerPort": 443}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := &Env{Getenv: func(k string) string { return secretEnv[k] }, In: strings.NewReader(`{"hostPort": 53, "containerPort": 53, "protocol": "PROTOCOL_UDP"}`)}
	fields := []Field{
		{Path: "app.image"},
		{Path: "app.command"},
		{Path: "app.env", Sensitive: true},
		{Path: "app.ports"},
		{Path: "app.restart"},
		{Path: "app.labels"},
	}
	var svc resourcev1.Service
	err := parse(t, env, &svc, fields,
		"--app-image", "ghcr.io/acme/web:1.4.2", "--app-command", "serve", "--app-command", "--port=80",
		"--app-env", "API_KEY", "--app-env", "MODE=fast", "--app-ports", `{"hostPort": 80, "containerPort": 8080}`,
		"--app-ports", "@"+ports, "--app-ports", "-", "--app-restart", "unless-stopped", "--app-labels", "team=web")
	if err != nil {
		t.Fatal(err)
	}
	want := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: "ghcr.io/acme/web:1.4.2", Command: []string{"serve", "--port=80"},
		Env: map[string]string{"API_KEY": "s3cret", "MODE": "fast"},
		Ports: []*resourcev1.PortMapping{
			{HostPort: 80, ContainerPort: 8080},
			{HostPort: 8443, ContainerPort: 443},
			{HostPort: 53, ContainerPort: 53, Protocol: resourcev1.Protocol_PROTOCOL_UDP},
		},
		Restart: resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED,
		Labels:  map[string]string{"team": "web"},
	}}}
	if !proto.Equal(&svc, want) {
		t.Errorf("parsed\n%v\nwant\n%v", &svc, want)
	}

	svc.Reset()
	err = parse(t, env, &svc, fields, "--app-env", "MISSING")
	if err == nil || !strings.Contains(err.Error(), "takes the value of MISSING from your environment, but it isn't set") {
		t.Errorf("a key that isn't set = %v", err)
	}
	err = parse(t, env, &svc, fields, "--app-labels", "team")
	if err == nil || !strings.Contains(err.Error(), "KEY=VALUE") {
		t.Errorf("a label without a value = %v", err)
	}
	err = parse(t, env, &svc, fields, "--app-ports", "{not json")
	var usage UsageError
	if err == nil || !errors.As(err, &usage) || !strings.Contains(err.Error(), "can't read it as a PortMapping in JSON") {
		t.Errorf("bad JSON = %v", err)
	}
}

func TestFlagHelpSaysHowToWriteValues(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	env := &Env{Getenv: func(string) string { return "" }}
	if _, err := bind(fs, env, (&resourcev1.Container{}).ProtoReflect().Descriptor(), []Field{
		{Path: "env", Help: "Environment variables, which may hold secrets.", Sensitive: true},
		{Path: "restart", Help: "What Docker does when the container stops."},
		{Path: "ports", Help: "Ports published on the server."},
	}); err != nil {
		t.Fatal(err)
	}
	usage := fs.FlagUsages()
	for _, want := range []string{
		"--env key=value", "KEY alone to take the value from your environment",
		"One of no, on-failure, unless-stopped, always.",
		"--ports json", "As JSON, or @file",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("flag help doesn't say %q:\n%s", want, usage)
		}
	}
}

// JSON on the command line can't set a secret's value, which shell history
// would keep, and says how to give it. A container's environment is
// configuration: it goes inline, as --env KEY=VALUE does.
func TestInlineJSONRefusesASecretsValueAlone(t *testing.T) {
	file := filepath.Join(t.TempDir(), "edit.json")
	secret := `{"resource":{"name":"DB"},"spec":{"secret":{"value":"s3cret"}}}`
	if err := os.WriteFile(file, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	fields := []Field{{Path: "edits", Help: "The edits."}}
	for what, c := range map[string]struct {
		value string
		in    string
		ok    bool
	}{
		"a secret's value inline":          {value: secret},
		"a secret's value from a file":     {value: "@" + file, ok: true},
		"a secret's value from stdin":      {value: "-", in: secret, ok: true},
		"a container's environment inline": {value: `{"resource":{"name":"web"},"spec":{"container":{"env":{"TOKEN":"x"}}}}`, ok: true},
		"a service's environment inline":   {value: `{"resource":{"name":"web"},"spec":{"service":{"app":{"env":{"TOKEN":"x"}}}}}`, ok: true},
		"a variable's value inline":        {value: `{"resource":{"name":"LOG"},"spec":{"variable":{"value":"info"}}}`, ok: true},
	} {
		env := &Env{Getenv: func(string) string { return "" }, In: strings.NewReader(c.in)}
		var req dockwellv1.ProposeChangeRequest
		err := parse(t, env, &req, fields, "--edits", c.value)
		switch {
		case c.ok && err != nil:
			t.Errorf("%s = %v, want it read", what, err)
		case !c.ok && (err == nil || !strings.Contains(err.Error(), "pass --edits @file, or pipe it in with --edits -") || strings.Contains(err.Error(), "s3cret")):
			t.Errorf("%s = %v, want it refused", what, err)
		}
	}
}

// A CIFS mount's password goes in a volume's o option, so it reaches the CLI as a
// secret's value does: never from a flag's text, where shell history and the
// process list keep it. An option that gives one comes from the environment, and
// JSON that gives one from a file or standard input. Other options, and a
// container's environment, go inline as before. The last check of #70 found both
// routes open.
func TestAVolumesPasswordNeverComesFromAFlagsText(t *testing.T) {
	const password = "cifs-pw-7Hq2mXv9"
	fields := []Field{{Path: "options", Sensitive: true}}
	for what, c := range map[string]struct {
		args []string
		env  map[string]string
		want map[string]string
		says string
	}{
		"o with password=":  {args: []string{"--options", "o=addr=10.0.0.2,username=svc,password=" + password}, says: "Give --options o alone to take its value from o in your environment"},
		"o with pass=":      {args: []string{"--options", "o=username=svc,pass=" + password}, says: "o gives a password"},
		"o with password2=": {args: []string{"--options", "o=username=svc,password2=" + password}, says: "o gives a password"},
		"a plugin's own":    {args: []string{"--options", "Password=" + password}, says: "Password gives a password"},
		"from the environment": {
			args: []string{"--options", "type=cifs", "--options", "o"}, env: map[string]string{"o": "username=svc,password=" + password},
			want: map[string]string{"type": "cifs", "o": "username=svc,password=" + password},
		},
		"an NFS mount inline": {
			args: []string{"--options", "type=nfs", "--options", "o=addr=10.0.0.2,ro", "--options", "device=:/export"},
			want: map[string]string{"type": "nfs", "o": "addr=10.0.0.2,ro", "device": ":/export"},
		},
		"a word like it, inline": {args: []string{"--options", "o=passwd_file=/x,passive"}, want: map[string]string{"o": "passwd_file=/x,passive"}},
	} {
		env := &Env{Getenv: func(k string) string { return c.env[k] }, In: strings.NewReader("")}
		var v resourcev1.Volume
		err := parse(t, env, &v, fields, c.args...)
		switch {
		case c.says != "" && (err == nil || !strings.Contains(err.Error(), c.says) || strings.Contains(err.Error(), password)):
			t.Errorf("%s = %v, want it refused, saying %q and not the password", what, err, c.says)
		case c.says == "" && err != nil:
			t.Errorf("%s = %v, want it read", what, err)
		case c.says == "" && !maps.Equal(v.GetOptions(), c.want):
			t.Errorf("%s read as %v, want %v", what, v.GetOptions(), c.want)
		}
	}
	// A container's environment is configuration, as --env KEY=VALUE has it.
	var ctr resourcev1.Container
	if err := parse(t, &Env{Getenv: func(string) string { return "" }}, &ctr, []Field{{Path: "env", Sensitive: true}}, "--env", "DB=password=x"); err != nil {
		t.Errorf("a container's environment inline = %v", err)
	}

	file := filepath.Join(t.TempDir(), "edit.json")
	share := `{"resource":{"name":"share"},"spec":{"volume":{"options":{"type":"cifs","o":"username=svc,password=` + password + `"}}}}`
	if err := os.WriteFile(file, []byte(share), 0o600); err != nil {
		t.Fatal(err)
	}
	edits := []Field{{Path: "edits"}}
	for what, c := range map[string]struct {
		value, in string
		ok        bool
	}{
		"a volume's password inline":      {value: share},
		"a volume's password from a file": {value: "@" + file, ok: true},
		"a volume's password from stdin":  {value: "-", in: share, ok: true},
		"an NFS volume inline":            {value: `{"resource":{"name":"nfs"},"spec":{"volume":{"options":{"type":"nfs","o":"addr=10.0.0.2"}}}}`, ok: true},
	} {
		env := &Env{Getenv: func(string) string { return "" }, In: strings.NewReader(c.in)}
		var req dockwellv1.ProposeChangeRequest
		err := parse(t, env, &req, edits, "--edits", c.value)
		switch {
		case c.ok && err != nil:
			t.Errorf("%s = %v, want it read", what, err)
		case !c.ok && (err == nil || !strings.Contains(err.Error(), "gives a volume a password") || !strings.Contains(err.Error(), "pass --edits @file, or pipe it in with --edits -") || strings.Contains(err.Error(), password)):
			t.Errorf("%s = %v, want it refused", what, err)
		}
	}
}

// A Compose file can give a volume a CIFS password in its driver_opts, so --compose
// reads a file, or standard input, and refuses such a file given on the command
// line. Any other file goes inline as before. The last check of #70 found no way to
// keep one off the command line.
func TestAComposeFileComesFromAFile(t *testing.T) {
	const password = "cifs-pw-7Hq2mXv9"
	withPassword := "services:\n  app:\n    image: nginx:1.27\n    volumes: [share:/x]\nvolumes:\n  share:\n    driver_opts: {type: cifs, device: //nas/share, o: \"username=svc,password=" + password + "\"}\n"
	plain := "services:\n  app:\n    image: nginx:1.27\n"
	dir := t.TempDir()
	file := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(file, []byte(withPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	fields := []Field{{Path: "compose", FromFile: true}}
	for what, c := range map[string]struct {
		value, in, want string
		says            string
	}{
		"a file":                         {value: "@" + file, want: withPassword},
		"standard input":                 {value: "-", in: withPassword, want: withPassword},
		"inline, with no password":       {value: plain, want: plain},
		"inline, with a volume password": {value: withPassword, says: "the file gives the volume share a password in its driver_opts"},
		"a file that isn't there":        {value: "@" + filepath.Join(dir, "gone.yaml"), says: "can't read"},
	} {
		env := &Env{Getenv: func(string) string { return "" }, In: strings.NewReader(c.in)}
		var req dockwellv1.ImportComposeRequest
		err := parse(t, env, &req, fields, "--compose", c.value)
		switch {
		case c.says != "" && (err == nil || !strings.Contains(err.Error(), c.says) || strings.Contains(err.Error(), password)):
			t.Errorf("%s = %v, want it refused, saying %q", what, err, c.says)
		case c.says == "" && (err != nil || req.GetCompose() != c.want):
			t.Errorf("%s = %q, %v, want %q", what, req.GetCompose(), err, c.want)
		}
	}
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	if _, err := bind(fs, &Env{}, (&dockwellv1.ImportComposeRequest{}).ProtoReflect().Descriptor(), fields); err != nil {
		t.Fatal(err)
	}
	if usage := fs.FlagUsages(); !strings.Contains(usage, "--compose text") || !strings.Contains(usage, "@file to read it from a file, or - from standard input") {
		t.Errorf("--compose's help doesn't say how to read a file:\n%s", usage)
	}
}

func TestSetPath(t *testing.T) {
	req := &dockwellv1.ProposeChangeRequest{Edits: []*dockwellv1.Edit{{Resource: &dockwellv1.ResourceRef{Name: "staging"}}}}
	if err := setPath(req.ProtoReflect(), "edits[0].resource.scope", "shop"); err != nil {
		t.Fatal(err)
	}
	if err := setPath(req.ProtoReflect(), "edits[0].project", "shop"); err != nil {
		t.Fatal(err)
	}
	if err := setPath(req.ProtoReflect(), "edits[0].servers", "hz-2"); err != nil {
		t.Fatal(err)
	}
	e := req.GetEdits()[0]
	if e.GetResource().GetScope() != "shop" || e.GetProject() != "shop" || len(e.GetServers()) != 1 {
		t.Errorf("setPath gave %v", e)
	}
	for _, bad := range []string{"edits[1].project", "edits[0].nothing", "edits[x].project", "summary.x"} {
		if err := setPath(req.ProtoReflect(), bad, "v"); err == nil {
			t.Errorf("setPath(%s) should fail", bad)
		}
	}
}

func TestNormalizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://panel.acme.dev":   "https://panel.acme.dev",
		"https://panel.acme.dev/":  "https://panel.acme.dev",
		"panel.acme.dev":           "https://panel.acme.dev",
		"127.0.0.1:7600":           "http://127.0.0.1:7600",
		"localhost:7600":           "http://localhost:7600",
		"http://127.0.0.1:7600":    "http://127.0.0.1:7600",
		"http://localhost:7600":    "http://localhost:7600",
		"http://[::1]:7600":        "http://[::1]:7600",
		"https://203.0.113.7:8443": "https://203.0.113.7:8443",
	} {
		if got, err := NormalizeURL(in); err != nil || got != want {
			t.Errorf("NormalizeURL(%s) = %q, %v; want %q", in, got, err, want)
		}
	}
	for in, want := range map[string]string{
		"http://panel.acme.dev":          "unencrypted",
		"ftp://panel.acme.dev":           "isn't an http:// or https:// URL",
		"https://panel.acme.dev/v1/x":    "without a path",
		"https://mira:pw@panel.acme.dev": "without a path, query or user",
	} {
		if _, err := NormalizeURL(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("NormalizeURL(%s) = %v, want an error with %q", in, err, want)
		}
	}
}

func TestPanelURL(t *testing.T) {
	for base, want := range map[string]string{
		"http://127.0.0.1:7600":  "http://localhost:7600/changes/chg_1",
		"http://[::1]:7600":      "http://localhost:7600/changes/chg_1",
		"https://panel.acme.dev": "https://panel.acme.dev/changes/chg_1",
	} {
		if got := (&Client{URL: base}).PanelURL("/changes/chg_1"); got != want {
			t.Errorf("PanelURL from %s = %s, want %s", base, got, want)
		}
	}
}

// A field's flag never takes one of the CLI's own: a field named token
// needs (dockwell.v1.flag), and gets the flag it names.
func TestAFieldsFlagNeverTakesTheCLIsOwn(t *testing.T) {
	desc := (&dockwellv1.CheckClaimRequest{}).ProtoReflect().Descriptor()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	_, err := bind(fs, &Env{}, desc, []Field{{Path: "token"}})
	if err == nil || !strings.Contains(err.Error(), "would take the flag --token") || !strings.Contains(err.Error(), "(dockwell.v1.flag)") {
		t.Errorf("a field named token = %v, want it refused", err)
	}
	fs = pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.String("summary", "", "the command's own")
	if _, err := bind(fs, &Env{}, (&dockwellv1.ProposeChangeRequest{}).ProtoReflect().Descriptor(), []Field{{Path: "summary"}}); err == nil {
		t.Error("a field's flag took one the command has already")
	}
	fs = pflag.NewFlagSet("test", pflag.ContinueOnError)
	var req dockwellv1.CheckClaimRequest
	b, err := bind(fs, &Env{}, desc, []Field{{Path: "token", Flag: "claim-token"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Parse([]string{"--claim-token", "abc"}); err != nil {
		t.Fatal(err)
	}
	if err := b.apply(req.ProtoReflect(), nil); err != nil || req.GetToken() != "abc" {
		t.Errorf("--claim-token abc = %q, %v", req.GetToken(), err)
	}
	if f := fs.Lookup("claim-token"); f == nil || f.Annotations[FieldAnnotation][0] != "token" {
		t.Errorf("--claim-token isn't marked as the field token: %+v", f)
	}
}

// How long a change took comes from its own times: from when it started
// applying to when its last step finished, whoever watches and whenever.
func TestTookIsTheChangesOwnTime(t *testing.T) {
	start := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	now := start.Add(3 * time.Hour)
	c := &dockwellv1.Change{
		ApplyTime: timestamppb.New(start),
		Steps: []*dockwellv1.StepProgress{
			{StartTime: timestamppb.New(start), FinishTime: timestamppb.New(start.Add(30 * time.Second))},
			{StartTime: timestamppb.New(start.Add(30 * time.Second)), FinishTime: timestamppb.New(start.Add(42 * time.Second))},
			{}, // skipped
		},
	}
	if d, ok := took(c, time.Time{}, now); !ok || d != 42*time.Second {
		t.Errorf("watched later, took = %s, %v; want 42s", d, ok)
	}
	if d, ok := took(c, now.Add(-time.Second), now); !ok || d != 42*time.Second {
		t.Errorf("applied by this CLI, took = %s, %v; want the change's own 42s", d, ok)
	}
	none := &dockwellv1.Change{ApplyTime: timestamppb.New(start)}
	if _, ok := took(none, time.Time{}, now); ok {
		t.Error("a change with no step times, only watched, has a duration")
	}
	if d, ok := took(none, now.Add(-2*time.Second), now); !ok || d != 2*time.Second {
		t.Errorf("a change with no step times that this CLI applied took %s, %v; want the 2s it waited", d, ok)
	}
}
