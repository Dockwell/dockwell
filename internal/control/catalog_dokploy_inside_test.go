// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/gitfetch/gitfetchtest"
	"github.com/dockwell/dockwell/internal/store"
)

// dokployInside is a Dokploy templates repository with three apps shaped as calcom's,
// plane's and snipe-it's are, each making up a value inside a longer one in its
// environment, on nginx, which the test registry has.
var dokployInside = map[string]string{
	"LICENSE": "MIT License\n\nCopyright (c) 2024 Example\n\nPermission is hereby granted, free of charge, to any person obtaining a copy\n",
	"blueprints/calcom/docker-compose.yml": `services:
  calcom:
    image: nginx:1.27
    environment:
      - DATABASE_URL=${DATABASE_URL}
  db:
    image: nginx:1.27
    environment:
      - POSTGRES_PASSWORD=${POSTGRES_PASSWORD}
`,
	"blueprints/calcom/template.toml": `[variables]
main_domain = "${domain}"
postgres_password = "${password:32}"
database_url = "postgresql://calcom:${postgres_password}@db:5432/calendso"

[config]
env = ["POSTGRES_PASSWORD=${postgres_password}", "DATABASE_URL=${database_url}"]

[[config.domains]]
serviceName = "calcom"
port = 3000
host = "${main_domain}"
`,
	"blueprints/plane/docker-compose.yml": `services:
  api:
    image: nginx:1.27
    environment:
      - POSTGRES_PASSWORD=${POSTGRES_PASSWORD}
      - DATABASE_URL=${DATABASE_URL}
      - AMQP_URL=${AMQP_URL}
`,
	"blueprints/plane/template.toml": `[variables]
main_domain = "${domain}"
postgres_password = "${password:32}"
rabbitmq_password = "${password:24}"

[config]
env = [
  "POSTGRES_PASSWORD=${postgres_password}",
  "DATABASE_URL=postgresql://plane:${postgres_password}@plane-db:5432/plane",
  "AMQP_URL=amqp://plane:${rabbitmq_password}@plane-mq:5672/plane",
]

[[config.domains]]
serviceName = "api"
port = 8000
host = "${main_domain}"
`,
	"blueprints/snipe-it/docker-compose.yml": `services:
  snipe-it:
    image: nginx:1.27
    environment:
      APP_KEY: ${APP_KEY}
`,
	"blueprints/snipe-it/template.toml": `[variables]
main_domain = "${domain}"
app_key = "${base64:32}"

[config]
[[config.domains]]
serviceName = "snipe-it"
port = 80
host = "${main_domain}"

[config.env]
APP_KEY = "base64:${app_key}"
`,
}

// A Dokploy app that makes up a value inside a longer one in its environment, as
// calcom's, plane's and snipe-it's do, installs, and its app reads the value where the
// template put it, whatever characters it holds: a password a person gives of every
// character a URL treats apart parses back out of the URL, and one Dockwell generates
// does too; a key after a prefix, which isn't a URL, is written as it is, + / and =
// alike; and a key Dockwell generates is 32 bytes in base64, as ${base64:32} makes
// (#570). Before, Dokploy's converter left each of them out.
func TestADokployValueMadeUpInsideALongerOneReachesTheApp(t *testing.T) {
	t.Parallel()
	repo := gitfetchtest.New(t)
	head := repo.Commit(dokployInside)
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	srv := f.lastDocker
	srv.stop()
	waitConnected(t, f, srv.id.ServerID, false)
	srv.images = agent.ImageReads{Test: testRegistry(t)}
	srv.run(t)
	waitConnected(t, f, srv.id.ServerID, true)
	c := changeClient(b, b.origin)
	cat := catalogClient(b)
	res, err := cat.SetCatalogSource(t.Context(), connect.NewRequest(&dockwellv1.SetCatalogSourceRequest{
		Name: "dokploy", Repository: repo.URL, Commit: head, Format: resourcev1.CatalogFormat_CATALOG_FORMAT_DOKPLOY,
	}))
	if err != nil {
		t.Fatalf("SetCatalogSource: %v", err)
	}
	apply(t, c, res.Msg.GetChange())

	const hostile = "p@ss:w/rd?#%+&= é$x"
	// 32 bytes whose base64 holds +, / and =.
	key := base64.StdEncoding.EncodeToString([]byte("\xfb\xff\xbf\xfb\xff\xbf\xfb\xff\xbf\xfb\xff\xbf\xfb\xff\xbf\xfb\xff\xbf\xfb\xff\xbf\xfb\xff\xbf\xfb\xff\xbf\xfb\xff\xbf\xfb\xff"))
	if !strings.ContainsAny(key, "+/=") {
		t.Fatalf("the key %q holds none of + / =", key)
	}
	installs := map[string]*dockwellv1.InstallCatalogEntryRequest{
		"calcom":     {Id: "calcom", Values: map[string]string{"PORT": "18301", "POSTGRES_PASSWORD": hostile}},
		"plane":      {Id: "plane", Values: map[string]string{"PORT": "18302", "RABBITMQ_PASSWORD": hostile}},
		"snipe-it":   {Id: "snipe-it", Values: map[string]string{"PORT": "18303", "APP_KEY_SECRET": key}},
		"snipe-it-2": {Id: "snipe-it", Project: "snipe-it-2", Values: map[string]string{"PORT": "18304"}},
	}
	for _, project := range []string{"calcom", "plane", "snipe-it", "snipe-it-2"} {
		apply(t, c, installApp(t, cat, installs[project]))
	}
	listed, err := cat.ListCatalogInstalls(t.Context(), connect.NewRequest(&dockwellv1.ListCatalogInstallsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	envOf := map[string]string{}
	for _, in := range listed.Msg.GetInstalls() {
		envOf[in.GetProject()] = in.GetEnvironmentId()
	}
	read := func(project, service string) map[string]string {
		t.Helper()
		ctr := e.Newest(docker.Project(envOf[project]), service)
		if ctr == nil {
			t.Fatalf("%s's %s doesn't run: %v", project, service, e.Containers())
		}
		out := map[string]string{}
		for _, kv := range ctr.Config.Env {
			k, v, _ := strings.Cut(kv, "=")
			out[k] = v
		}
		return out
	}
	password := func(raw string) string {
		t.Helper()
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("%q isn't a URL: %v", raw, err)
		}
		p, _ := u.User.Password()
		return p
	}

	// calcom: a URL from a variable that holds the password, which a person gave.
	calcom, db := read("calcom", "calcom"), read("calcom", "db")
	if got := password(calcom["DATABASE_URL"]); got != hostile || db["POSTGRES_PASSWORD"] != hostile ||
		!strings.HasPrefix(calcom["DATABASE_URL"], "postgresql://calcom:") || !strings.HasSuffix(calcom["DATABASE_URL"], "@db:5432/calendso") {
		t.Errorf("calcom reads DATABASE_URL %q, which holds %q, and db POSTGRES_PASSWORD %q", calcom["DATABASE_URL"], got, db["POSTGRES_PASSWORD"])
	}
	// plane: a password Dockwell generates in one URL, and one a person gave in
	// another, which nothing else names.
	plane := read("plane", "api")
	if got := password(plane["DATABASE_URL"]); len(got) != 32 || got != plane["POSTGRES_PASSWORD"] {
		t.Errorf("plane reads DATABASE_URL %q, which holds %q, beside POSTGRES_PASSWORD %q", plane["DATABASE_URL"], got, plane["POSTGRES_PASSWORD"])
	}
	if got := password(plane["AMQP_URL"]); got != hostile || !strings.HasSuffix(plane["AMQP_URL"], "@plane-mq:5672/plane") {
		t.Errorf("plane reads AMQP_URL %q, which holds %q", plane["AMQP_URL"], got)
	}
	// snipe-it: a key after a prefix is no URL, so it's written as it is.
	if got := read("snipe-it", "snipe-it")["APP_KEY"]; got != "base64:"+key {
		t.Errorf("snipe-it reads APP_KEY %q, want %q", got, "base64:"+key)
	}
	generated := read("snipe-it-2", "snipe-it")["APP_KEY"]
	raw, ok := strings.CutPrefix(generated, "base64:")
	if decoded, err := base64.StdEncoding.DecodeString(raw); !ok || err != nil || len(decoded) != 32 {
		t.Errorf("snipe-it-2 reads APP_KEY %q, want base64: and 32 bytes in base64", generated)
	}
	// What anyone reads of the installs holds none of the values.
	if s := listed.Msg.String(); strings.Contains(s, "p@ss") || strings.Contains(s, key) || strings.Contains(s, raw) {
		t.Error("an install's record holds a secret")
	}
}
