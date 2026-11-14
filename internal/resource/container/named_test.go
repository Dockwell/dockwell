// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
)

// Where a taken-on container's values are kept by name (ADR 0096): a secret's
// wherever it stands, any other's only as a whole word or the whole of what
// follows =, never where the program's name goes, and in a script only one the
// shell reads as the word it is. What can't be named stays, and shows as before.
func TestWhereAValueIsKeptByName(t *testing.T) {
	for name, c := range map[string]struct {
		env              map[string]string
		command, test    []string
		wantCmd, wantTst []string
		shows            []string
	}{
		"a password in a shell's check": {
			env:     map[string]string{"MYSQL_ROOT_PASSWORD": "k8Jd2mQx"},
			test:    []string{"CMD-SHELL", "mysqladmin ping -h localhost -uroot -pk8Jd2mQx"},
			wantTst: []string{"CMD-SHELL", "mysqladmin ping -h localhost -uroot -p${MYSQL_ROOT_PASSWORD}"},
		},
		"a password in a check's list": {
			env:     map[string]string{"REDIS_PASSWORD": "k8Jd2mQx"},
			test:    []string{"CMD", "redis-cli", "-a", "k8Jd2mQx", "ping"},
			wantTst: []string{"CMD", "redis-cli", "-a", "${REDIS_PASSWORD}", "ping"},
		},
		"a short password": {
			env:     map[string]string{"DB_PASS": "s3cr7"},
			command: []string{"serve", "--db-pass=s3cr7"},
			wantCmd: []string{"serve", "--db-pass=${DB_PASS}"},
		},
		// Renaming the user would change the program that runs, so it's refused, as
		// before.
		"an ordinary value where the program's name goes": {
			env:     map[string]string{"POSTGRES_USER": "postgres"},
			command: []string{"postgres", "-c", "max_connections=200"},
			wantCmd: []string{"postgres", "-c", "max_connections=200"},
			shows:   []string{"the value of POSTGRES_USER in its command"},
		},
		"ordinary values as words of a script": {
			env:     map[string]string{"POSTGRES_USER": "chatwoot", "POSTGRES_DB": "chatwoot_production"},
			test:    []string{"CMD-SHELL", `pg_isready -U chatwoot -d "chatwoot_production"`},
			wantTst: []string{"CMD-SHELL", `pg_isready -U ${POSTGRES_USER} -d "${POSTGRES_DB}"`},
		},
		"an ordinary value after =": {
			env:     map[string]string{"APP_USER": "chatwoot"},
			command: []string{"serve", "--user=chatwoot"},
			wantCmd: []string{"serve", "--user=${APP_USER}"},
		},
		"an ordinary value inside a word": {
			env:     map[string]string{"APP_USER": "chatwoot"},
			command: []string{"serve", "--url=postgres://chatwoot@db/x"},
			wantCmd: []string{"serve", "--url=postgres://chatwoot@db/x"},
			shows:   []string{"the value of APP_USER in its command"},
		},
		"an ordinary value where a script's command goes": {
			env:     map[string]string{"APP_BIN": "shop-server"},
			test:    []string{"CMD-SHELL", "true && shop-server --check"},
			wantTst: []string{"CMD-SHELL", "true && shop-server --check"},
			shows:   []string{"the value of APP_BIN in its health check"},
		},
		// A secret that shares text with an ordinary value: the longer goes first, and
		// neither shows.
		"a secret beside an ordinary value it contains": {
			env:     map[string]string{"SHOP_USER": "shopadmin", "ADMIN_PASSWORD": "shopadmin123"},
			command: []string{"serve", "--user=shopadmin", "--password=shopadmin123"},
			wantCmd: []string{"serve", "--user=${SHOP_USER}", "--password=${ADMIN_PASSWORD}"},
		},
		"a secret inside an ordinary value": {
			env:     map[string]string{"SITE": "hunter22-blog", "ADMIN_PASSWORD": "hunter22"},
			command: []string{"serve", "--site=hunter22-blog", "--pw=hunter22"},
			wantCmd: []string{"serve", "--site=${SITE}", "--pw=${ADMIN_PASSWORD}"},
		},
		"a $ of its own beside a password": {
			env:     map[string]string{"REDIS_PASSWORD": "k8Jd2mQx"},
			test:    []string{"CMD-SHELL", `test -n "$HOME" && redis-cli -a k8Jd2mQx ping`},
			wantTst: []string{"CMD-SHELL", `test -n "$$HOME" && redis-cli -a ${REDIS_PASSWORD} ping`},
		},
		// The shell would read it as more than the word it is.
		"a password a script can't hold as a word": {
			env:     map[string]string{"REDIS_PASSWORD": "pa$$;word"},
			test:    []string{"CMD-SHELL", "redis-cli -a 'pa$$;word' ping"},
			wantTst: []string{"CMD-SHELL", "redis-cli -a 'pa$$;word' ping"},
			shows:   []string{"the value of REDIS_PASSWORD in its health check"},
		},
		"a secret where the program's name goes": {
			env:     map[string]string{"RUN_TOKEN": "tok3nvalue"},
			command: []string{"tok3nvalue", "--x"},
			wantCmd: []string{"${RUN_TOKEN}", "--x"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := &resourcev1.Container{Image: "x:1", Env: c.env, Command: slices.Clone(c.command)}
			if c.test != nil {
				spec.Healthcheck = &resourcev1.Healthcheck{Test: slices.Clone(c.test)}
			}
			nameValues(spec)
			if !slices.Equal(spec.GetCommand(), c.wantCmd) || !slices.Equal(spec.GetHealthcheck().GetTest(), c.wantTst) {
				t.Errorf("named %q and %q, want %q and %q", spec.GetCommand(), spec.GetHealthcheck().GetTest(), c.wantCmd, c.wantTst)
			}
			if got := valuesShown(spec); !slices.Equal(got, c.shows) {
				t.Errorf("shows %q, want %q", got, c.shows)
			}
			if len(c.shows) == 0 {
				if err := Validate(spec); err != nil {
					t.Errorf("the named spec isn't valid: %v", err)
				}
			}
		})
	}
}

// A container taken on with its values kept by name reads back as its spec, names
// and never values, from the agent's record of it: Docker can't label what runs.
func TestATakenOnContainerReadsBackByName(t *testing.T) {
	e := dockertest.New(t)
	e.AddLocalImage("mariadb:11", dockertest.Image{Config: docker.Config{Cmd: []string{"mariadbd"}}})
	c := e.Client()
	record := filepath.Join(t.TempDir(), "adopted.json")
	c.Adopted = &docker.Adopted{Path: record}
	d := &Docker{Client: c, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1}
	const pw = "k8Jd2mQxR4"
	id, err := c.CreateContainer(t.Context(), "legacy_db", &docker.CreateRequest{Config: docker.Config{
		Image: "mariadb:11", Env: []string{"MARIADB_ROOT_PASSWORD=" + pw},
		Healthcheck: &docker.Healthcheck{Test: []string{"CMD-SHELL", "mariadb-admin ping -uroot -p" + pw}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	read, err := d.ReadAdoption(t.Context(), taken, id)
	if err != nil {
		t.Fatal(err)
	}
	spec := read.Spec.(*resourcev1.Container)
	if len(read.Shows) != 0 || !slices.Equal(spec.GetReferences(), []string{"healthcheck.test"}) ||
		spec.GetHealthcheck().GetTest()[1] != "mariadb-admin ping -uroot -p${MARIADB_ROOT_PASSWORD}" {
		t.Fatalf("read shows %v, as %v", read.Shows, spec)
	}
	if _, err := d.Adoption(t.Context(), taken, id, spec); err != nil {
		t.Fatalf("checking it again as it's applied: %v", err)
	}
	if err := d.Adopt(t.Context(), taken, id, spec, resource.Decision{}); err != nil {
		t.Fatal(err)
	}
	got, err := d.Inspect(t.Context(), taken)
	if err != nil {
		t.Fatal(err)
	}
	if changes := resource.Diff(spec, got); len(changes) != 0 {
		t.Errorf("it reads back as %v, differing in %v", got, changes)
	}
	if strings.Contains(got.GetHealthcheck().String(), pw) {
		t.Error("what it reads back holds the value")
	}
	// The record keeps the names, and never the value.
	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), pw) || !strings.Contains(string(b), "${MARIADB_ROOT_PASSWORD}") {
		t.Errorf("the record holds %s", b)
	}
}
