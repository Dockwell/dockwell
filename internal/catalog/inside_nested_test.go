// SPDX-License-Identifier: AGPL-3.0-only

package catalog_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// What the check lets through in a service's environment is rewritten, so the
// install never writes a secret's stand-in where its password belongs; a secret
// inside another value's default, which a template can't hold, is refused, as it was
// before (#570's review, H1).
func TestEveryValueTheCheckLetsThroughIsRewritten(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := c.Entry("postgres")
	for name, tc := range map[string]struct {
		value string
		// refused is what the check says, where it refuses it.
		refused string
	}{
		"nested fallback":  {"${URL_GIVEN:-postgres://u:${POSTGRES_PASSWORD}@db/x}", "inside another value's default"},
		"nested alternate": {"${TLS:+postgres://u:${POSTGRES_PASSWORD}@db/x}", "inside another value's default"},
		// The bare spelling, $NAME, which Compose reads too (#570's recheck, N4).
		"bare nested fallback":  {"${URL_GIVEN:-postgres://u:$POSTGRES_PASSWORD@db/x}", "inside another value's default"},
		"bare nested alternate": {"${TLS:+postgres://u:$POSTGRES_PASSWORD@db/x}", "inside another value's default"},
		"plain then secret":     {"postgres://${USER:-u}:${POSTGRES_PASSWORD}@db/x", ""},
		"bare dollar name":      {"postgres://u:$POSTGRES_PASSWORD@db/x", ""},
		// One URL, quoted or not, or a line of text, is written; anything else isn't
		// (#570's review, L1).
		"a quoted URL":      {"'postgres://u:${POSTGRES_PASSWORD}@db/x'", ""},
		"a DSN on one line": {"host=db user=u password=${POSTGRES_PASSWORD} dbname=x", ""},
		"JSON in its query": {`mysql://root:${POSTGRES_PASSWORD}@db:3306/x?ssl={\"rejectUnauthorized\":false}`, ""},
		"a prefix":          {"app_${POSTGRES_PASSWORD}", ""},
		"a URL after text":  {"${MODE:-a}b}postgres://u:${POSTGRES_PASSWORD}@db/x", "isn't one URL or a line of text"},
		"two URLs":          {"postgres://u:${POSTGRES_PASSWORD}@db/x,redis://:${POSTGRES_PASSWORD}@r/0", "isn't one URL or a line of text"},
		"JSON":              {`{\"url\":\"http://x\",\"key\":\"${POSTGRES_PASSWORD}\"}`, "isn't one URL or a line of text"},
	} {
		e := proto.CloneOf(base)
		e.Compose = strings.Replace(e.GetCompose(), "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}",
			"POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n      URL: \""+tc.value+"\"", 1)
		err := catalog.Check(e)
		if tc.refused != "" {
			if err == nil || !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("%s: the check says %v of %q, want %q", name, err, tc.value, tc.refused)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: refused: %v", name, err)
			continue
		}
		needed, err := catalog.Need(e, nil)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !strings.Contains(needed.Entry.GetCompose(), "URL: ${URL}") {
			t.Errorf("%s: the check lets %q through, and the install's file still has it:\n%s", name, tc.value, needed.Entry.GetCompose())
		}
		// A URL's password is encoded as one; a line of text's stays as it is.
		want := resourcev1.Encoding_ENCODING_UNSPECIFIED
		if strings.Contains(tc.value, "://") {
			want = resourcev1.Encoding_ENCODING_USERINFO
		}
		for _, n := range needed.Written["URL"].GetNames() {
			if n.GetSecret() && n.GetEncoding() != want {
				t.Errorf("%s: %s is written %v, want %v", name, n.GetName(), n.GetEncoding(), want)
			}
		}
	}
}

// $$ is a $ of its own, so $$POSTGRES_PASSWORD names no secret: the check lets it
// through and the install leaves it as it is (#570's recheck, N4).
func TestADollarOfItsOwnNamesNoSecret(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := c.Entry("postgres")
	e := proto.CloneOf(base)
	e.Compose = strings.Replace(e.GetCompose(), "POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}",
		"POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}\n      LITERAL: \"pay $$POSTGRES_PASSWORD\"", 1)
	if err := catalog.Check(e); err != nil {
		t.Fatalf("a $ of its own: %v", err)
	}
	needed, err := catalog.Need(e, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(needed.Inside) != 0 {
		t.Errorf("a $ of its own is written from a template: %v", needed.Inside)
	}
}

// A list's KEY=value is judged by its value, so a URL there is one URL (#570's
// review, L1).
func TestAListsURLIsOneURL(t *testing.T) {
	c, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := c.Entry("postgres")
	e := proto.CloneOf(base)
	e.Compose = strings.Replace(e.GetCompose(), "services:\n", "services:\n  worker:\n    image: busybox:1.37@sha256:"+strings.Repeat("a", 64)+
		"\n    environment:\n      - DATABASE_URL=postgresql://u:${POSTGRES_PASSWORD}@db:5432/x\n", 1)
	if err := catalog.Check(e); err != nil {
		t.Fatalf("a list's URL: %v", err)
	}
	needed, err := catalog.Need(e, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := needed.Written["DATABASE_URL"].GetNames(); len(n) != 1 || n[0].GetEncoding() != resourcev1.Encoding_ENCODING_USERINFO {
		t.Errorf("a list's URL is written %v, want its password as a URL's userinfo", n)
	}
}
