// SPDX-License-Identifier: AGPL-3.0-only

package catalog_test

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// postgresWith is the built-in PostgreSQL entry with Adminer's ports as given, and
// HTTP_PORT, a second port input, declared public when public says so.
func postgresWith(t *testing.T, ports string, public bool) *catalogv1.Entry {
	t.Helper()
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := c.Entry("postgres")
	e := proto.CloneOf(base)
	// The fixtures are written as an entry's file indents them; the built-in one
	// indents its services two less.
	var lines []string
	for line := range strings.Lines(ports) {
		lines = append(lines, strings.TrimPrefix(line, "  "))
	}
	file := strings.Replace(e.GetCompose(), "    ports:\n      - \"${ADMIN_PORT}:8080\"\n", "    ports:\n"+strings.Join(lines, ""), 1)
	if file == e.GetCompose() {
		t.Fatal("the built-in PostgreSQL entry's Adminer publishes its port otherwise")
	}
	e.Compose = file
	e.Inputs = append(e.Inputs, &catalogv1.Input{Name: "HTTP_PORT", Label: "HTTP port", Type: catalogv1.InputType_INPUT_TYPE_PORT, DefaultValue: "18180", Public: public})
	for _, c := range e.GetCompanions() {
		if c.GetService() == "adminer" {
			c.Inputs = append(c.Inputs, "HTTP_PORT")
		}
	}
	return e
}

// Every port an install publishes answers on the server alone, 127.0.0.1, whatever
// form its file writes it in, but one whose host port is a public input whole,
// which stays on every address (ADR 0101).
func TestAnInstallPublishesEachPortOnTheServerAloneButThePublicOnes(t *testing.T) {
	e := postgresWith(t, `        - "${ADMIN_PORT}:8080"
        - "0.0.0.0:18111:81"
        - "[::]:18112:82"
        - "8443"
        - "${OTHER_PORT:-18113}:83/udp"
        - "127.0.0.1:18114:84"
        - "10.0.0.80:80:85/udp"
        - "0.0.0.0:0:87"
        - target: 85
          published: 18115
        - target: 86
          published: 18116
          host_ip: 0.0.0.0
        - "${HTTP_PORT}:80"
        - target: 443
          published: ${HTTP_PORT}
`, true)
	needed, err := catalog.Need(e, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"127.0.0.1:${ADMIN_PORT}:8080", "127.0.0.1:18111:81", "127.0.0.1:18112:82", "127.0.0.1::8443",
		"127.0.0.1:${OTHER_PORT:-18113}:83/udp", "127.0.0.1:18114:84", "127.0.0.1:80:85/udp", "127.0.0.1:0:87",
		"85 on 127.0.0.1 at 18115", "86 on 127.0.0.1 at 18116",
		"${HTTP_PORT}:80", "443 on  at ${HTTP_PORT}",
	}
	if got := adminerPorts(t, needed.Entry.GetCompose()); !slices.Equal(got, want) {
		t.Errorf("the install's ports:\n%q\nwant\n%q", got, want)
	}
	// An entry with nothing public and every port on the server alone is as it was.
	same := postgresWith(t, "        - \"127.0.0.1:${ADMIN_PORT}:8080\"\n        - \"127.0.0.1:${HTTP_PORT}:80\"\n", false)
	if got, err := catalog.Loopback(same); err != nil || got != same {
		t.Errorf("Loopback rewrote a file with every port on the server alone: %v", err)
	}
}

// adminerPorts are the ports Adminer publishes in a file: a short form as it's
// written, and a long one as "target on host_ip at published".
func adminerPorts(t *testing.T, file string) []string {
	t.Helper()
	var doc struct {
		Services map[string]struct {
			Ports []any `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(file), &doc); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range doc.Services["adminer"].Ports {
		switch p := p.(type) {
		case map[string]any:
			ip, _ := p["host_ip"].(string)
			out = append(out, fmt.Sprintf("%v on %s at %v", p["target"], ip, p["published"]))
		default:
			out = append(out, fmt.Sprint(p))
		}
	}
	return out
}

// The ports an install declares public, as its inputs have them, by service; every
// port it publishes; and those on every address, as an install made before ran
// them.
func TestPublicPortsArePortsByTheirValues(t *testing.T) {
	e := postgresWith(t, "        - \"${ADMIN_PORT}:8080\"\n        - \"${HTTP_PORT}:80\"\n        - \"${HTTP_PORT}:80/udp\"\n", true)
	values := map[string]string{"HTTP_PORT": "80", "ADMIN_PORT": "18100"}
	if got := catalog.PublicPorts(e, values); !maps.EqualFunc(got, map[string][]string{"adminer": {"80", "80/udp"}}, slices.Equal) {
		t.Errorf("public ports %v", got)
	}
	if got := catalog.Published(e, values); !maps.EqualFunc(got, map[string][]string{"adminer": {"18100", "80", "80/udp"}}, slices.Equal) {
		t.Errorf("published %v", got)
	}
	needed, err := catalog.Need(e, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.EveryAddress(needed.Entry, values); !maps.EqualFunc(got, map[string][]string{"adminer": {"80", "80/udp"}}, slices.Equal) {
		t.Errorf("on every address after the install %v", got)
	}
	if got := catalog.EveryAddress(e, values); !maps.EqualFunc(got, map[string][]string{"adminer": {"18100", "80", "80/udp"}}, slices.Equal) {
		t.Errorf("on every address as the file says %v", got)
	}
	if got := catalog.PublicPorts(postgresWith(t, "        - \"${ADMIN_PORT}:8080\"\n        - \"${HTTP_PORT}:80\"\n", false), values); len(got) > 0 {
		t.Errorf("an entry with nothing public has public ports %v", got)
	}
}

// The catalog's check refuses a public port it can't publish on every address on
// purpose: on an input that isn't a port, one no service publishes as its port, or one
// published on an address alone (ADR 0101).
func TestCheckRefusesAPublicPortItCantPublish(t *testing.T) {
	good := postgresWith(t, "        - \"${ADMIN_PORT}:8080\"\n        - \"${HTTP_PORT}:80\"\n", true)
	if err := catalog.Check(good); err != nil {
		t.Fatalf("an entry with a public port: %v", err)
	}
	for name, tc := range map[string]struct {
		e    *catalogv1.Entry
		want string
	}{
		"a public secret": {func() *catalogv1.Entry {
			e := proto.CloneOf(good)
			for _, in := range e.GetInputs() {
				if in.GetName() == "POSTGRES_PASSWORD" {
					in.Public = true
				}
			}
			return e
		}(), "the input POSTGRES_PASSWORD is public, which only a port is"},
		"published inside a longer port": {
			postgresWith(t, "        - \"${ADMIN_PORT}:8080\"\n        - \"1${HTTP_PORT}:80\"\n", true),
			"no service publishes it as its port",
		},
		"published on the server alone": {
			postgresWith(t, "        - \"${ADMIN_PORT}:8080\"\n        - \"127.0.0.1:${HTTP_PORT}:80\"\n", true),
			"adminer publishes it on 127.0.0.1 alone",
		},
	} {
		if err := catalog.Check(tc.e); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// The check refuses a port the rewrite to the server alone can't see where the
// service writes it: a ports list that's an alias, a merge key that brings ports, a
// port that's an alias, and a whole port from a default. Each would answer on every
// address with nobody's approval (#752's review, H1).
func TestCheckRefusesAPortTheRewriteCantSee(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := c.Entry("postgres")
	for name, tc := range map[string]struct{ file, want string }{
		"an alias for the list": {
			"x-ports: &ports\n  - \"18130:5432\"\nservices:\n  db:\n    image: IMAGE\n    ports: *ports\n",
			"db's ports are an alias",
		},
		"a merge key that brings ports": {
			"x-base: &base\n  ports:\n    - \"18131:5432\"\nservices:\n  db:\n    <<: *base\n    image: IMAGE\n",
			"db takes its ports through a merge key",
		},
		"a port that's an alias": {
			"x-port: &port \"18132:5432\"\nservices:\n  db:\n    image: IMAGE\n    ports:\n      - *port\n",
			"db publishes a port through an alias",
		},
		"a whole port from a default": {
			"services:\n  db:\n    image: IMAGE\n    ports:\n      - \"${SPEC:-18123:83}\"\n",
			"db publishes ${SPEC:-18123:83}, a whole port from an input",
		},
		// The re-check's N1: forms the guard refused at plan time with words about a
		// file the person never wrote.
		"an aliased service": {
			"x-db: &db\n  image: IMAGE\n  ports:\n    - \"18130:5432\"\nservices:\n  db: *db\n",
			"db is an alias, *db",
		},
		"services as an alias": {
			"x-s: &s\n  db:\n    image: IMAGE\nservices: *s\n",
			"its services are an alias, *s",
		},
		"a merge key inside a port": {
			"x-p: &p\n  published: 18133\nservices:\n  db:\n    image: IMAGE\n    ports:\n      - <<: *p\n        target: 5432\n",
			"db publishes a port whose << comes through an alias",
		},
		"an anchored host_ip": {
			"x-ip: &ip 0.0.0.0\nservices:\n  db:\n    image: IMAGE\n    ports:\n      - target: 5432\n        published: 18134\n        host_ip: *ip\n",
			"db publishes a port whose host_ip comes through an alias",
		},
		"a whole port from an input": {
			"services:\n  db:\n    image: IMAGE\n    ports:\n      - \"${PORTSPEC}\"\n",
			"db publishes ${PORTSPEC}, a whole port from an input",
		},
	} {
		e := proto.CloneOf(base)
		image := regexp.MustCompile(`image: (\S+)`).FindStringSubmatch(base.GetCompose())[1]
		e.Compose = strings.ReplaceAll(tc.file, "IMAGE", image)
		e.Inputs, e.Companions, e.Databases, e.SignIn, e.Web = nil, nil, nil, nil, nil
		if err := catalog.Check(e); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	// A merge key that brings no ports, and a port already on the server alone with a
	// port Docker picks, pass.
	for name, file := range map[string]string{
		"a merge key with no ports": "x-base: &base\n  restart: always\nservices:\n  db:\n    <<: *base\n    image: IMAGE\n",
		"a port Docker picks":       "services:\n  db:\n    image: IMAGE\n    ports:\n      - \"127.0.0.1::5432\"\n",
	} {
		e := proto.CloneOf(base)
		image := regexp.MustCompile(`image: (\S+)`).FindStringSubmatch(base.GetCompose())[1]
		e.Compose = strings.ReplaceAll(file, "IMAGE", image)
		e.Inputs, e.Companions, e.Databases, e.SignIn, e.Web = nil, nil, nil, nil, nil
		if err := catalog.Check(e); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A secret inside a value and a port with no address in one entry: Need names the
// value's secret first (#570) and writes the port on the server alone last, so the
// file an install runs has both (ADR 0101; #752's review, on landing beside #754).
func TestASecretInsideAValueAndAPortOnTheServerAlone(t *testing.T) {
	e := postgresWith(t, "        - \"${ADMIN_PORT}:8080/tcp\"\n", false)
	e.Compose = strings.Replace(e.GetCompose(), "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}",
		"POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n      URL: postgres://u:${POSTGRES_PASSWORD}@db/x", 1)
	needed, err := catalog.Need(e, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !needed.Inside["URL"] || !strings.Contains(needed.Entry.GetCompose(), "URL: ${URL}") {
		t.Errorf("the value's secret isn't named URL: %v\n%s", needed.Inside, needed.Entry.GetCompose())
	}
	if got := adminerPorts(t, needed.Entry.GetCompose()); !slices.Equal(got, []string{"127.0.0.1:${ADMIN_PORT}:8080/tcp"}) {
		t.Errorf("Adminer publishes %q", got)
	}
}
