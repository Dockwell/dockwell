// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/compose"
)

// The cases the first review of #302 probed, each held to what it should come to.

func plainApp(file string, fields ...Field) App {
	return App{ID: "plain", Name: "Plain", Version: "1.0", Compose: []byte(file), Fields: fields}
}

func convertPlain(t *testing.T, c *Converter, file string, fields ...Field) Result {
	t.Helper()
	if _, ok := c.Overlays["plain"]; !ok {
		c.Overlays["plain"] = written("Stands in", "ai")
	}
	return c.Convert(t.Context(), plainApp(file, fields...))
}

// Nothing that reaches the server converts until a person has looked: what a service
// reaches, and what its volumes and networks reach, as the plan counts them.
func TestNothingThatReachesItsServerConvertsUnlooked(t *testing.T) {
	for name, tc := range map[string]struct{ file, reaches string }{
		"a volume that binds /":           {"services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - root:/host\nvolumes:\n  root:\n    driver: local\n    driver_opts:\n      type: none\n      o: bind\n      device: /\n", "volume root: control of Docker, through /"},
		"a volume that binds Docker's":    {"services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - run:/var/run\nvolumes:\n  run:\n    driver_opts:\n      type: none\n      o: bind\n      device: /var/run\n", "volume run: control of Docker, through /var/run"},
		"an NFS volume":                   {"services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - share:/share\nvolumes:\n  share:\n    driver_opts:\n      type: nfs\n      o: addr=192.0.2.10,rw\n      device: \":/export\"\n", "volume share: a mount nfs 192.0.2.10 :/export"},
		"a plugin's volume":               {"services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - plugin:/p\nvolumes:\n  plugin:\n    driver: local-persist\n    driver_opts:\n      mountpoint: /etc\n", "volume plugin: a mount local-persist /etc"},
		"a macvlan network":               {"services:\n  a:\n    image: example/a:1.0\n    networks: [lan]\nnetworks:\n  lan:\n    driver: macvlan\n    driver_opts:\n      parent: eth0\n", "network lan: the macvlan network driver eth0"},
		"a bridge that binds elsewhere":   {"services:\n  a:\n    image: example/a:1.0\n    networks: [n]\nnetworks:\n  n:\n    driver_opts:\n      com.docker.network.bridge.host_binding_ipv4: 0.0.0.0\n", "network n: the bridge network driver com.docker.network.bridge.host_binding_ipv4=0.0.0.0"},
		"privileged through a merge key":  {"x-base: &base\n  privileged: true\nservices:\n  a:\n    <<: *base\n    image: example/a:1.0\n", "a: every privilege"},
		"a capability":                    {"services:\n  a:\n    image: example/a:1.0\n    cap_add: [SYS_ADMIN]\n", "a: the capability"},
		"/ through an alias":              {"x-v: &v\n  - /:/host\nservices:\n  a:\n    image: example/a:1.0\n    volumes: *v\n", "a: control of Docker, through /"},
		"a folder a person names":         {"services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - ${PANEL_DATA}:/data\n", "a: a server path a person names, ${DATA}"},
		"Docker's group":                  {"services:\n  a:\n    image: example/a:1.0\n    group_add: [docker]\n", "a:"},
		"/etc/localtime, writable":        {"services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - /etc/localtime:/etc/localtime\n", "a: the server path /etc/localtime"},
		"a network named as the server's": {"services:\n  a:\n    image: example/a:1.0\n    networks: [n]\nnetworks:\n  n:\n    name: bridge\n", ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := convertPlain(t, converter(), tc.file, Field{Name: "PANEL_DATA", Kind: Text, Default: "./data"})
			if r.Entry != nil {
				t.Fatalf("converted with nobody looking:\n%s", r.Entry.GetCompose())
			}
			if tc.reaches != "" && (r.Class != ClassReaches || !strings.Contains(r.LeftOut, tc.reaches)) {
				t.Errorf("left out as %s: %q, want it to name %q", r.Class, r.LeftOut, tc.reaches)
			}
		})
	}
}

// What the Compose reader refuses stays out, in words for whoever converts a catalog.
func TestWhatDockwellCantRunStaysOut(t *testing.T) {
	for name, file := range map[string]string{
		"devices":                "services:\n  a:\n    image: example/a:1.0\n    devices: [/dev/sda]\n",
		"pid host":               "services:\n  a:\n    image: example/a:1.0\n    pid: host\n",
		"security_opt":           "services:\n  a:\n    image: example/a:1.0\n    security_opt: [apparmor:unconfined]\n",
		"sysctls":                "services:\n  a:\n    image: example/a:1.0\n    sysctls: {net.ipv4.ip_forward: 1}\n",
		"network_mode service":   "services:\n  a:\n    image: example/a:1.0\n  b:\n    image: example/b:1.0\n    network_mode: service:a\n",
		"include":                "include: [other.yml]\nservices:\n  a:\n    image: example/a:1.0\n",
		"profiles":               "services:\n  a:\n    image: example/a:1.0\n    profiles: [x]\n",
		"extends through merge":  "x-b: &b\n  extends: {service: z, file: /etc/passwd}\nservices:\n  a:\n    <<: *b\n    image: example/a:1.0\n",
		"an external volume":     "services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - v:/v\nvolumes:\n  v:\n    external: true\n",
		"its own folder":         "services:\n  a:\n    image: example/a:1.0\n    user: root\n    volumes:\n      - .:/app\n",
		"its own folder, with /": "services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - ./:/app\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := convertPlain(t, converter(), file)
			if r.Entry != nil {
				t.Fatalf("converted:\n%s", r.Entry.GetCompose())
			}
			if strings.Contains(r.LeftOut, "to import it") {
				t.Errorf("tells a catalog's person to import something: %q", r.LeftOut)
			}
		})
	}
	// A folder of its own, bound in long form, and the time zone read-only, are fine.
	for name, file := range map[string]string{
		"a folder, in long form":   "services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - type: bind\n        source: ./data\n        target: /data\n",
		"the time zone, read-only": "services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - type: bind\n        source: /etc/localtime\n        target: /etc/localtime\n        read_only: true\n",
	} {
		if r := convertPlain(t, converter(), file); r.Entry == nil {
			t.Errorf("%s was left out: %s", name, r.LeftOut)
		}
	}
}

// Every port that converts ends on loopback, however the source wrote it.
func TestEveryPortEndsOnLoopback(t *testing.T) {
	fields := []Field{{Name: "PANEL_APP_PORT_HTTP", Kind: Port, Default: "8080", Web: true}, {Name: "PANEL_SPEC", Kind: Text, Default: "8080:80"}}
	for name, file := range map[string]string{
		"three parts":                     "services:\n  a:\n    image: example/a:1.0\n    ports:\n      - 0.0.0.0:8080:80\n",
		"IPv6, any":                       "services:\n  a:\n    image: example/a:1.0\n    ports:\n      - \"[::]:8080:80\"\n",
		"IPv6, loopback":                  "services:\n  a:\n    image: example/a:1.0\n    ports:\n      - \"::1:8080:80\"\n",
		"a range":                         "services:\n  a:\n    image: example/a:1.0\n    ports:\n      - 8000-8010:8000-8010\n",
		"UDP":                             "services:\n  a:\n    image: example/a:1.0\n    ports:\n      - 53:53/udp\n",
		"the container's alone":           "services:\n  a:\n    image: example/a:1.0\n    ports:\n      - \"80\"\n",
		"a number":                        "services:\n  a:\n    image: example/a:1.0\n    ports:\n      - 80\n",
		"long, no address":                "services:\n  a:\n    image: example/a:1.0\n    ports:\n      - target: 80\n        published: 8080\n",
		"long, ::":                        "services:\n  a:\n    image: example/a:1.0\n    ports:\n      - target: 80\n        published: \"8080\"\n        host_ip: \"::\"\n",
		"long, an address in published":   "services:\n  a:\n    image: example/a:1.0\n    ports:\n      - target: 80\n        published: \"0.0.0.0:8080\"\n",
		"through an alias":                "x-p: &p\n  - 8080:80\nservices:\n  a:\n    image: example/a:1.0\n    ports: *p\n",
		"through a merge key":             "x-p: &p\n  ports: [\"8080:80\"]\nservices:\n  a:\n    <<: *p\n    image: example/a:1.0\n",
		"an address from a variable":      "services:\n  a:\n    image: example/a:1.0\n    ports:\n      - ${HOST_IP:-0.0.0.0}:8080:80\n",
		"the whole of it from a variable": "services:\n  a:\n    image: example/a:1.0\n    ports:\n      - ${PANEL_SPEC}\n",
		"one port two ways":               "services:\n  a:\n    image: example/a:1.0\n    ports:\n      - \"${PANEL_APP_PORT_HTTP}:80\"\n      - \"${PANEL_APP_PORT_HTTP}:443\"\n",
		"a protocol from a variable":      "services:\n  a:\n    image: example/a:1.0\n    ports:\n      - \"${PANEL_APP_PORT_HTTP}:80/${PROTO:-tcp}\"\n",
		"expose":                          "services:\n  a:\n    image: example/a:1.0\n    expose: [\"80\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			r := convertPlain(t, converter(), file, fields...)
			if r.Entry == nil {
				return
			}
			stack, filled := readEntry(t, r.Entry)
			_ = filled
			for _, s := range stack.Services {
				for _, p := range s.Spec.GetApp().GetPorts() {
					if p.GetHostIp() != "127.0.0.1" {
						t.Errorf("publishes %d on %q:\n%s", p.GetContainerPort(), p.GetHostIp(), r.Entry.GetCompose())
					}
				}
			}
		})
	}
}

func readEntry(t *testing.T, e *catalogv1.Entry) (*compose.Stack, *catalog.Filled) {
	t.Helper()
	given := map[string]string{}
	for _, in := range e.GetInputs() {
		if catalog.Asks(in) && in.GetDefaultValue() == "" && !in.GetOptional() {
			given[in.GetName()] = standIn(in)
		}
	}
	filled, err := catalog.Fill(e, given, catalog.Suggest(e, nil))
	if err != nil {
		t.Fatal(err)
	}
	stack, err := compose.Read(t.Context(), compose.Input{File: []byte(e.GetCompose()), Variables: filled.Variables})
	if err != nil {
		t.Fatal(err)
	}
	return stack, filled
}

// What the rewrite renames, and what it leaves.
func TestTheRewriteRenamesHostsAndNothingElse(t *testing.T) {
	gen := Field{Name: "PANEL_DB_PASSWORD", Kind: Generated}
	for name, tc := range map[string]struct {
		file       string
		want, gone []string
		leftOut    string
	}{
		"a service's underscore, and not a path": {
			file: "services:\n  act_runner:\n    image: example/act_runner:1.0\n    command: [\"/usr/local/bin/act_runner\", \"daemon\"]\n    volumes:\n      - ./data:/data/act_runner\n    environment:\n      - CONFIG_FILE=/etc/act_runner/config.yaml\n      - SELF=http://act_runner:8080\n",
			want: []string{"act-runner:", "/usr/local/bin/act_runner", "/data/act_runner", "/etc/act_runner/config.yaml", "SELF=http://act-runner:8080", "image: example/act_runner:1.0@"},
		},
		"two folders, two volumes": {
			file: "services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - ./data-db:/x\n      - ./data/db:/y\n",
			want: []string{"- data-db:/x", "- data-db-2:/y"},
		},
		"a folder inside another": {
			file:    "services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - ./data:/data\n  b:\n    image: example/b:1.0\n    volumes:\n      - ./data/sub:/sub\n",
			leftOut: "it binds ./data/sub inside ./data",
		},
		"volumes through an alias": {
			file: "x-v: &v\n  - ./data:/data\nservices:\n  a:\n    image: example/a:1.0\n    volumes: *v\n",
			want: []string{"- data:/data", "data: {}"},
		},
		"networks through an alias": {
			file: "x-n: &n\n  - backend\nservices:\n  a:\n    image: example/a:1.0\n    networks: *n\nnetworks:\n  backend: {}\n",
			want: []string{"networks:\n      - backend"},
		},
		"a container's own name": {
			file: "services:\n  app:\n    image: example/a:1.0\n    environment:\n      - DB_HOST=mydb\n      - NOTE=mydb_backup\n  db:\n    image: example/db:1.0\n    container_name: mydb\n",
			want: []string{"DB_HOST=db", "NOTE=mydb_backup"},
			gone: []string{"container_name"},
		},
		"a variable with no braces": {
			file: "services:\n  a:\n    image: example/a:1.0\n    environment:\n      - PASS=$PANEL_DB_PASSWORD\n",
			want: []string{"PASS=${DB_PASSWORD}"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := convertPlain(t, converter(), tc.file, gen)
			if tc.leftOut != "" {
				if r.Entry != nil || !strings.Contains(r.LeftOut, tc.leftOut) {
					t.Errorf("%q, want it left out as %q", r.LeftOut, tc.leftOut)
				}
				return
			}
			if r.Entry == nil {
				t.Fatal(r.LeftOut)
			}
			for _, w := range tc.want {
				if !strings.Contains(r.Entry.GetCompose(), w) {
					t.Errorf("its file doesn't say %q:\n%s", w, r.Entry.GetCompose())
				}
			}
			for _, g := range tc.gone {
				if strings.Contains(r.Entry.GetCompose(), g) {
					t.Errorf("its file still says %q:\n%s", g, r.Entry.GetCompose())
				}
			}
		})
	}
}

// Aliases that stand for more than a file says stay out, rather than fill memory.
func TestAliasesThatStandForTooMuch(t *testing.T) {
	var b strings.Builder
	b.WriteString("x-0: &a0 [x, x, x, x, x, x, x, x, x, x]\n")
	for i := 1; i < 9; i++ {
		b.WriteString("x-" + string(rune('0'+i)) + ": &a" + string(rune('0'+i)) + " [")
		for j := range 10 {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteString("*a" + string(rune('0'+i-1)))
		}
		b.WriteString("]\n")
	}
	b.WriteString("services:\n  a:\n    image: example/a:1.0\n")
	r := convertPlain(t, converter(), b.String())
	if r.Entry != nil || r.Class != ClassFile {
		t.Errorf("%s: %q", r.Class, r.LeftOut)
	}
}

func youngApp(versions ...string) App {
	var newest *App
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i]
		app := App{ID: "a", Name: "A", Version: v, Path: "apps/a/" + v, Compose: []byte("services:\n  a:\n    image: example/a:" + v + "\n")}
		app.Older = newest
		newest = &app
	}
	return *newest
}

// A version under 14 days old never takes an entry back, or across a major version:
// an earlier entry stays as it is, and with none, only a version of the newest's
// major stands in.
func TestAYoungVersionNeverGoesBackOrAcrossAMajor(t *testing.T) {
	youngAt := func(images ...string) *fakeRegistry {
		f := &fakeRegistry{built: map[string]time.Time{}}
		for _, i := range images {
			f.built[i] = now.Add(-2 * 24 * time.Hour)
		}
		return f
	}
	c := converter()
	c.Overlays["a"] = written("Stands in", "ai")
	first := c.Convert(t.Context(), App{ID: "a", Name: "A", Version: "2.0", Path: "apps/a/2.0", Compose: []byte("services:\n  a:\n    image: example/a:2.0\n")})
	if first.Entry == nil {
		t.Fatal(first.LeftOut)
	}

	// The newest is young, and the earlier entry stays, rather than 1.9 in its place.
	c2 := converter()
	c2.Overlays["a"] = written("Stands in", "ai")
	c2.Previous, c2.PreviousDates = map[string]*catalogv1.Entry{"a": first.Entry}, map[string]string{"a": "2026-10-01"}
	c2.Registry = youngAt("example/a:2.1")
	r := c2.Convert(t.Context(), youngApp("2.1", "1.9"))
	if r.Entry != first.Entry || r.Held == "" || r.Class != ClassYoung || r.Date != "2026-10-01" {
		t.Errorf("with 2.1 young: %s release %d, held %q, %s", r.Entry.GetVersion(), r.Entry.GetRelease(), r.Held, r.Class)
	}

	// With no earlier entry, 18.6 is young, and 13.23 never stands in for it, but 18.5 does.
	c3 := converter()
	c3.Overlays["a"] = written("Stands in", "ai")
	c3.Registry = youngAt("example/a:18.6")
	if r := c3.Convert(t.Context(), youngApp("18.6", "13.23")); r.Entry != nil || !r.Young || r.Class != ClassYoung {
		t.Errorf("13.23 stood in for 18.6: %v, %q", r.Entry.GetVersion(), r.LeftOut)
	}
	if r := c3.Convert(t.Context(), youngApp("18.6", "18.5", "13.23")); r.Entry.GetVersion() != "18.5" {
		t.Errorf("18.5 didn't stand in for 18.6: %v, %q", r.Entry.GetVersion(), r.LeftOut)
	}

	// An entry at 13.23 stays there when 18.6 is old enough, as old data may not survive it.
	c4 := converter()
	c4.Overlays["a"] = written("Stands in", "ai")
	thirteen := c4.Convert(t.Context(), youngApp("13.23"))
	c4.Previous = map[string]*catalogv1.Entry{"a": thirteen.Entry}
	if r := c4.Convert(t.Context(), youngApp("18.6")); r.Entry.GetVersion() != "13.23" || r.Class != ClassMajor || !strings.Contains(r.Held, "#248") {
		t.Errorf("13.23 moved to %s: %q", r.Entry.GetVersion(), r.Held)
	}
	// And it never goes back.
	if r := c4.Convert(t.Context(), youngApp("13.22")); r.Entry.GetVersion() != "13.23" || r.Class != ClassBack {
		t.Errorf("13.23 went back to %s: %q", r.Entry.GetVersion(), r.Held)
	}
	// A 0.x minor is a major.
	if majorOf("0.31.0") == majorOf("0.30.4") || majorOf("v2.1") != majorOf("2.9") || majorOf("13.23-alpine") != "13-alpine" {
		t.Errorf("majors: %q %q %q %q %q", majorOf("0.31.0"), majorOf("0.30.4"), majorOf("v2.1"), majorOf("2.9"), majorOf("13.23-alpine"))
	}
}

// A tag pushed again waits its own 14 days: entries keep the build they had.
func TestATagPushedAgainWaits(t *testing.T) {
	old := "sha256:" + strings.Repeat("1", 64)
	c := converter()
	c.Overlays["a"] = written("Stands in", "ai")
	c.Pins = map[string]Pinned{"example/a:2.0": {Digest: old, Built: now.Add(-90 * 24 * time.Hour)}}
	c.Registry = &fakeRegistry{built: map[string]time.Time{"example/a:2.0": now.Add(-time.Hour)}}
	app := App{ID: "a", Name: "A", Version: "2.0", Compose: []byte("services:\n  a:\n    image: example/a:2.0\n")}
	r := c.Convert(t.Context(), app)
	if r.Entry == nil || !strings.Contains(r.Entry.GetCompose(), "example/a:2.0@"+old) {
		t.Fatalf("it didn't keep the build it had: %q\n%s", r.LeftOut, r.Entry.GetCompose())
	}
	if p := c.Pins["example/a:2.0"]; p.Digest != old || p.Next != digestOf("example/a:2.0") {
		t.Errorf("pins %+v", p)
	}
	// Fourteen days on, entries move to it.
	c.Now = now.Add(15 * 24 * time.Hour)
	r = c.Convert(t.Context(), app)
	if !strings.Contains(r.Entry.GetCompose(), "@"+digestOf("example/a:2.0")) || c.Pins["example/a:2.0"].Next != "" {
		t.Errorf("it didn't move to the new build once it was 14 days old:\n%s\n%+v", r.Entry.GetCompose(), c.Pins["example/a:2.0"])
	}
}

// A build that says no believable date, as a reproducible one says the epoch, counts
// from when the converter first saw it. One the source pins by digest is held too.
func TestABuildWithNoDateCountsFromFirstSight(t *testing.T) {
	for name, built := range map[string]time.Time{"the epoch": time.Unix(1, 0).UTC(), "the future": now.Add(48 * time.Hour)} {
		c := converter()
		c.Overlays["a"] = written("Stands in", "ai")
		c.Registry = &fakeRegistry{built: map[string]time.Time{"example/a:1.0": built}}
		r := c.Convert(t.Context(), App{ID: "a", Name: "A", Version: "1.0", Compose: []byte("services:\n  a:\n    image: example/a:1.0\n")})
		if r.Entry != nil || !r.Young || !c.Pins["example/a:1.0"].Built.Equal(now) {
			t.Errorf("%s: %q, pinned %+v", name, r.LeftOut, c.Pins["example/a:1.0"])
		}
	}
	c := converter()
	c.Overlays["a"] = written("Stands in", "ai")
	pinned := "example/a:1.0@sha256:" + strings.Repeat("2", 64)
	c.Registry = &fakeRegistry{built: map[string]time.Time{pinned: now.Add(-time.Hour)}}
	if r := c.Convert(t.Context(), App{ID: "a", Name: "A", Version: "1.0", Compose: []byte("services:\n  a:\n    image: " + pinned + "\n")}); r.Entry != nil || !r.Young {
		t.Errorf("an image the source pins skipped the hold: %q", r.LeftOut)
	}
}

// An entry runs on amd64 and arm64 both, as the owner's servers do.
func TestAnAmd64ImageAloneStaysOut(t *testing.T) {
	c := converter()
	c.Registry = &fakeRegistry{platforms: map[string][]string{"example/a:1.0": {"linux/amd64"}}}
	r := convertPlain(t, c, "services:\n  a:\n    image: example/a:1.0\n")
	if r.Entry != nil || r.Class != ClassArch || !strings.Contains(r.LeftOut, "not linux/arm64") {
		t.Errorf("%s: %q", r.Class, r.LeftOut)
	}
	if a := readFixture(t)["amd64only"]; a.Class != ClassArch || !strings.Contains(a.LeftOut, "1Panel says it runs on amd64 alone") {
		t.Errorf("1Panel's own word: %s, %q", a.Class, a.LeftOut)
	}
}

// A secret 1Panel types as text, with a default it would share with everyone, is a
// secret: generated where 1Panel makes one up, and typed by a person where it doesn't.
func TestASecretTypedAsTextIsASecret(t *testing.T) {
	r := convertFixture(t, converter(), "agent")
	if r.Entry == nil {
		t.Fatal(r.LeftOut)
	}
	in := inputsOf2(r.Entry)
	if k := in["AGENT_KEY"]; k.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET || k.GetGenerate() != 32 || k.GetDefaultValue() != "" {
		t.Errorf("AGENT_KEY is %v, want a generated secret with no default", k)
	}
	if k := in["REGISTRATION_TOKEN"]; k.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET || k.GetGenerate() != 0 || k.GetDefaultValue() != "" {
		t.Errorf("REGISTRATION_TOKEN is %v, want a secret a person types", k)
	}
	// A port of another server is no port of this one, and Dockwell won't move it.
	if p := in["SERVER_PORT"]; p.GetType() != catalogv1.InputType_INPUT_TYPE_TEXT || strings.Contains(p.GetLabel(), "on this server") || p.GetDefaultValue() != "10051" {
		t.Errorf("SERVER_PORT is %v", p)
	}
	// An app that isn't a website has no page to open, and no domain in its first steps.
	if r.Entry.GetWeb() != nil || strings.Contains(r.Entry.GetFirstSteps(), "domain") || strings.Contains(in["PORT"].GetHint(), "domain") {
		t.Errorf("a tool reads as a website: %v, %q, %q", r.Entry.GetWeb(), r.Entry.GetFirstSteps(), in["PORT"].GetHint())
	}
}

func inputsOf2(e *catalogv1.Entry) map[string]*catalogv1.Input {
	out := map[string]*catalogv1.Input{}
	for _, in := range e.GetInputs() {
		out[in.GetName()] = in
	}
	return out
}

// An overlay's input that names a type is the input: it can make a text input a
// generated secret, and take away the default 1Panel shares.
func TestAnOverlayCanMakeASecret(t *testing.T) {
	c := converter()
	o := written("Stands in", "ai")
	o.Entry.Inputs = []*catalogv1.Input{{Name: "AES_PWD", Type: catalogv1.InputType_INPUT_TYPE_SECRET, Generate: 32}}
	c.Overlays["plain"] = o
	r := convertPlain(t, c, "services:\n  a:\n    image: example/a:1.0\n    environment:\n      - AES_KEY=${AES_PWD}\n", Field{Name: "AES_PWD", Label: "AES password", Kind: Text, Default: "AESPWD"})
	if r.Entry == nil {
		t.Fatal(r.LeftOut)
	}
	if in := r.Entry.GetInputs()[0]; in.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET || in.GetGenerate() != 32 || in.GetDefaultValue() != "" || in.GetLabel() != "AES password" {
		t.Errorf("after the overlay: %v", in)
	}
	o.Entry.Inputs = []*catalogv1.Input{{Name: "GONE", Label: "x"}}
	if r := convertPlain(t, c, "services:\n  a:\n    image: example/a:1.0\n"); r.Entry != nil || r.Class != ClassOverlay {
		t.Errorf("an overlay naming an input the app doesn't have: %s, %q", r.Class, r.LeftOut)
	}
}

// What a person looked at is what converts: an app that reaches more, or less, waits
// for them to look again.
func TestHostAccessIsWhatAPersonLookedAt(t *testing.T) {
	file := "services:\n  ddns:\n    image: example/ddns:1.0\n    network_mode: host\n    privileged: true\n    volumes:\n      - /:/host\n      - /var/run/docker.sock:/var/run/docker.sock\n"
	app := App{ID: "ddns", Name: "DDNS", Version: "1.0", Compose: []byte(file)}
	c := converter()
	c.Overlays["ddns"] = Overlay{
		Entry:   &catalogv1.Entry{Summary: "Updates DNS", Category: "networking", HostAccess: "The server's own network, to read its address"},
		Reaches: []string{"ddns: the server's own network"},
	}
	r := c.Convert(t.Context(), app)
	if r.Entry != nil || r.Class != ClassReaches || !strings.Contains(r.LeftOut, `"ddns: control of Docker, through /var/run/docker.sock"`) {
		t.Fatalf("%s: %q", r.Class, r.LeftOut)
	}
	c.Overlays["ddns"] = Overlay{Entry: c.Overlays["ddns"].Entry, Reaches: []string{
		"ddns: control of Docker, through /", "ddns: control of Docker, through /var/run/docker.sock", "ddns: every privilege", "ddns: the server's own network",
	}}
	if r := c.Convert(t.Context(), app); r.Entry == nil {
		t.Errorf("what a person looked at didn't convert: %s", r.LeftOut)
	}
	c.Overlays["ddns"] = Overlay{Entry: c.Overlays["ddns"].Entry, Reaches: append(c.Overlays["ddns"].Reaches, "ddns: the capability NET_ADMIN")}
	if r := c.Convert(t.Context(), app); r.Entry != nil || !strings.Contains(r.LeftOut, `it no longer reaches "ddns: the capability NET_ADMIN"`) {
		t.Errorf("an app that reaches less: %q", r.LeftOut)
	}
}

// A run over some apps alone touches no other's entry, and its report merges into the
// last one. Dockwell's own entries are the binary's, whatever DOCKWELL_CATALOG_DIR names.
func TestARunOverSomeAppsLeavesTheRest(t *testing.T) {
	c := converter()
	var results []Result
	for _, id := range []string{"notes", "my-app"} {
		results = append(results, convertFixture(t, c, id))
	}
	dir := t.TempDir()
	if err := Write(dir, c.Source, results, nil, c.Pins, true); err != nil {
		t.Fatal(err)
	}
	previous, _, _, err := ReadPrevious(dir)
	if err != nil {
		t.Fatal(err)
	}
	again := []Result{{ID: "notes", Path: "apps/notes/1.2.0", LeftOut: "it runs scripts", Class: ClassScripts}}
	if err := Write(dir, c.Source, again, previous, c.Pins, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "my-app.yaml")); err != nil {
		t.Errorf("a run over notes alone removed my-app: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.yaml")); err == nil {
		t.Error("notes, which this run left out, is still there")
	}
	report, err := os.ReadFile(filepath.Join(dir, ReportFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(report), "| my-app | 0.5.0 | 1 |") || !strings.Contains(string(report), "2 apps, 1 converted") {
		t.Errorf("the report doesn't merge:\n%s", report)
	}

	t.Setenv(catalog.DirEnv, dir)
	own, err := catalog.BuiltIn("")
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(own.All(), func(e *catalogv1.Entry) bool { return e.GetId() == "my-app" }) {
		t.Error("BuiltIn reads DOCKWELL_CATALOG_DIR")
	}
}

// The converter reads only regular files inside the checkout, and none over MaxFile.
func TestItReadsOnlyFilesInsideTheCheckout(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "apps", "linked", "1.0.0")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.yml")
	if err := os.WriteFile(outside, []byte("services:\n  a:\n    image: example/a:1.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(root, "apps", "linked", "data.yml"), []byte("name: Linked\nadditionalProperties:\n  key: linked\n"), 0o600))
	must(os.WriteFile(filepath.Join(app, "data.yml"), []byte("additionalProperties: {}\n"), 0o600))
	must(os.Symlink(outside, filepath.Join(app, "docker-compose.yml")))
	big := filepath.Join(root, "apps", "big", "1.0.0")
	must(os.MkdirAll(big, 0o755))
	must(os.WriteFile(filepath.Join(root, "apps", "big", "data.yml"), []byte("name: Big\n"), 0o600))
	must(os.WriteFile(filepath.Join(big, "data.yml"), []byte("additionalProperties: {}\n"), 0o600))
	must(os.WriteFile(filepath.Join(big, "docker-compose.yml"), make([]byte, MaxFile+1), 0o600))
	apps, err := OnePanel(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range apps {
		switch a.ID {
		case "linked":
			if a.Compose != nil || a.Class != ClassFile {
				t.Errorf("a link out of the checkout was read: %s, %q", a.Class, a.LeftOut)
			}
		case "big":
			if a.Compose != nil || !strings.Contains(a.LeftOut, "is over 1024 KiB") {
				t.Errorf("a file over the cap was read: %q", a.LeftOut)
			}
		}
	}
}

// Every app in the store is in the report, and a folder kept with a placeholder is no
// file it ships.
func TestEveryAppIsCounted(t *testing.T) {
	// A catalog that's published pins each image as it's converted, so a version that
	// moves stays out of it; a server converts it, as an install pins it (ADR 0085).
	if r := convertFixture(t, converter(), "onlylatest"); r.Entry != nil || r.Class != ClassNoVersion || !strings.Contains(r.LeftOut, "its only version is latest") {
		t.Errorf("an app with only latest: %s, %q", r.Class, r.LeftOut)
	}
	if r := convertFixture(t, converter(), "keep"); r.Entry == nil {
		t.Errorf("data/.gitkeep counted as a file it ships: %s", r.LeftOut)
	}
}

// The report is a table whatever an ID or a reason holds, and counts every reason in
// a class of its own.
func TestTheReportHoldsWhatItSays(t *testing.T) {
	rows := []Row{
		{ID: "a|b\nc", Path: "apps/a`b", Class: ClassFile, Why: "a reason | with a bar\nand a line"},
		{ID: "ok", Path: "apps/ok/1", Version: "1", Release: 2},
		{ID: "held", Path: "apps/held/13", Version: "13", Release: 1, Held: "it moves to 18", Class: ClassMajor},
	}
	report := string(Report(converter().Source, rows, nil, nil))
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(line, "| a") && strings.Count(strings.ReplaceAll(line, `\|`, ""), "|") != 4 {
			t.Errorf("a row breaks its table: %q", line)
		}
	}
	for _, want := range []string{"3 apps, 1 converted, 1 kept at an earlier release, 1 left out", "| Its files can't be read | 1 |", "## Kept at an earlier release", "| held | 13 | 1 | it moves to 18 |"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report doesn't say %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "Something else") {
		t.Errorf("a reason has no class:\n%s", report)
	}
	if got := forCatalog("the service a uses `init`, which Dockwell can't run yet. Remove it from the Compose file to import it"); got != "the service a uses `init`, which Dockwell can't run yet" {
		t.Errorf("forCatalog = %q", got)
	}
}

// A pin's line says nothing after it, as the catalog reads a pin by its line, and a
// secret is written plainly, whatever default or message the source gave it.
func TestAPinAndASecretArePlain(t *testing.T) {
	file := "services:\n  a:\n    image: example/a:1.0 # released last spring\n    environment:\n      - TOKEN=${TOKEN:?Set the token}\n      - KEY=${KEY:-shared-by-everyone}\n"
	r := convertPlain(t, converter(), file, Field{Name: "TOKEN", Kind: Secret}, Field{Name: "KEY", Kind: Generated})
	if r.Entry == nil {
		t.Fatal(r.LeftOut)
	}
	for _, gone := range []string{"released last spring", "Set the token", "shared-by-everyone"} {
		if strings.Contains(r.Entry.GetCompose(), gone) {
			t.Errorf("its file still says %q:\n%s", gone, r.Entry.GetCompose())
		}
	}
	if !strings.Contains(r.Entry.GetCompose(), "TOKEN=${TOKEN}") || !strings.Contains(r.Entry.GetCompose(), "KEY=${KEY}") {
		t.Errorf("its file:\n%s", r.Entry.GetCompose())
	}
}
