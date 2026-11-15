// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
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

// What the review of slice 1 found (ADR 0096): a short secret, a value after a
// backslash or a $ and a name, in a here-document or a comment, or where a program
// is named past a keyword or a wrapper, isn't named, and refuses as before. What is
// named runs under sh as it ran.
func TestAScriptRunsAsItDid(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh here")
	}
	run := func(script string, env map[string]string) string {
		cmd := exec.Command(sh, "-c", script)
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	for name, c := range map[string]struct {
		env              map[string]string
		entry, cmd, test []string
		named            bool
	}{
		"a short secret in a port":      {env: map[string]string{"PASSWORD_MIN_LENGTH": "8"}, test: []string{"CMD-SHELL", "echo http://localhost:8080/health"}},
		"a short secret in a path":      {env: map[string]string{"KEYCLOAK_ADMIN": "admin"}, test: []string{"CMD", "curl", "-f", "http://localhost:8080/admin/health"}},
		"a short secret as a program":   {env: map[string]string{"DB_PASS": "sleep"}, test: []string{"CMD-SHELL", "sleep 1; echo $?"}},
		"a one-letter secret":           {env: map[string]string{"PASS_N": "n"}, test: []string{"CMD-SHELL", `printf "ok\n"; echo "a\nb"`}},
		"a secret after a backslash":    {env: map[string]string{"DB_PASSWORD": "k8Jd2mQx"}, test: []string{"CMD-SHELL", `echo "x\k8Jd2mQx"`}},
		"a secret after a $ and a name": {env: map[string]string{"DB_PASSWORD": "k8Jd2mQx", "FOOk8Jd2mQx": "zz"}, test: []string{"CMD-SHELL", `echo $FOOk8Jd2mQx`}},
		"a program after exec":          {env: map[string]string{"POSTGRES_USER": "postgres"}, cmd: []string{"sh", "-c", "exec postgres -c max_connections=200"}},
		"a program after gosu":          {env: map[string]string{"APP_BIN": "shop-server"}, cmd: []string{"gosu", "app", "shop-server", "--x"}},
		"a program after if":            {env: map[string]string{"APP_BIN": "shop-server"}, test: []string{"CMD-SHELL", "if shop-server --check; then echo y; fi"}},
		"a program after env and NAME=": {env: map[string]string{"APP_BIN": "shop-server"}, cmd: []string{"env", "-i", "A=1", "shop-server", "--x"}},
		"a program after timeout":       {env: map[string]string{"APP_BIN": "shop-server"}, test: []string{"CMD", "timeout", "5", "shop-server", "--check"}},
		"a program after tini --":       {env: map[string]string{"APP_BIN": "shop-server"}, entry: []string{"tini", "--", "shop-server"}},
		"a secret in a here-document":   {env: map[string]string{"DB_PASSWORD": "k8Jd2mQx"}, test: []string{"CMD-SHELL", "cat <<'EOF'\nk8Jd2mQx\nEOF"}},
		"a secret in a comment":         {env: map[string]string{"DB_PASSWORD": "k8Jd2mQx"}, test: []string{"CMD-SHELL", "echo ok # k8Jd2mQx"}},
		"a short secret in a host":      {env: map[string]string{"API_KEY": "ab"}, cmd: []string{"serve", "--url=http://tab.example.com/abc"}},
		// The review's narrow check (ADR 0096): quotes inside $( or backquotes aren't
		// the ones around them, so a script with either names nothing.
		"a secret inside $(…)":       {env: map[string]string{"REDIS_PASSWORD": "k8Jd2mQx"}, test: []string{"CMD-SHELL", `[ "$(redis-cli -a 'k8Jd2mQx' ping)" = PONG ]`}},
		"a secret inside backquotes": {env: map[string]string{"REDIS_PASSWORD": "k8Jd2mQx"}, test: []string{"CMD-SHELL", "[ \"`redis-cli -a 'k8Jd2mQx' ping`\" = PONG ]"}},
		"a secret the script sets":   {env: map[string]string{"DB_PASSWORD": "k8Jd2mQx"}, test: []string{"CMD-SHELL", "export DB_PASSWORD=other; app --pw k8Jd2mQx"}},
		"a secret the script unsets": {env: map[string]string{"DB_PASSWORD": "k8Jd2mQx"}, test: []string{"CMD-SHELL", "unset DB_PASSWORD; app --pw k8Jd2mQx"}},
		// What runs another program, a shell's word for it, or a redirection, leaves no
		// word read for sure as an argument.
		"a program after nohup":         {env: map[string]string{"APP_BIN": "shop-server"}, test: []string{"CMD-SHELL", "nohup shop-server --check"}},
		"a program after setsid":        {env: map[string]string{"APP_BIN": "shop-server"}, test: []string{"CMD-SHELL", "setsid shop-server --check"}},
		"a program after xargs":         {env: map[string]string{"APP_BIN": "shop-server"}, test: []string{"CMD-SHELL", "echo x | xargs shop-server"}},
		"a program after chroot":        {env: map[string]string{"APP_BIN": "shop-server"}, test: []string{"CMD-SHELL", "chroot /srv shop-server"}},
		"a program after runuser":       {env: map[string]string{"APP_BIN": "shop-server"}, test: []string{"CMD-SHELL", "runuser -u app -- shop-server"}},
		"a program after s6-setuidgid":  {env: map[string]string{"APP_BIN": "shop-server"}, test: []string{"CMD-SHELL", "s6-setuidgid app shop-server"}},
		"a program after catatonit":     {env: map[string]string{"APP_BIN": "shop-server"}, cmd: []string{"catatonit", "--", "shop-server"}},
		"a program after busybox":       {env: map[string]string{"APP_BIN": "shop-server"}, test: []string{"CMD-SHELL", "busybox shop-server"}},
		"a program after command":       {env: map[string]string{"APP_BIN": "shop-server"}, test: []string{"CMD-SHELL", "command shop-server --check"}},
		"a program after time -p":       {env: map[string]string{"APP_BIN": "shop-server"}, test: []string{"CMD-SHELL", "time -p shop-server --check"}},
		"a program in a nested sh -c":   {env: map[string]string{"APP_BIN": "shop-server"}, test: []string{"CMD-SHELL", "sh -c 'shop-server --check'"}},
		"a program after a redirection": {env: map[string]string{"APP_BIN": "shop-server"}, test: []string{"CMD-SHELL", ">/dev/null shop-server --check"}},
		"a program after 2>&1":          {env: map[string]string{"APP_BIN": "shop-server"}, test: []string{"CMD-SHELL", "2>&1 shop-server --check"}},
		// An argument after a redirection is still one.
		"an argument after a redirection":  {env: map[string]string{"APP_USER": "chatwoot"}, test: []string{"CMD-SHELL", "pg_isready 2>/dev/null -U chatwoot"}, named: true},
		"a secret beside a single quote":   {env: map[string]string{"DB_PASSWORD": "k8Jd2mQx"}, test: []string{"CMD-SHELL", `echo "it's k8Jd2mQx"`}, named: true},
		"a secret after an escaped quote":  {env: map[string]string{"DB_PASSWORD": "k8Jd2mQx"}, test: []string{"CMD-SHELL", `echo it\'s k8Jd2mQx`}, named: true},
		"a secret inside single quotes":    {env: map[string]string{"DB_PASSWORD": "k8Jd2mQx"}, test: []string{"CMD-SHELL", `echo 'pw=k8Jd2mQx;' ok`}, named: true},
		"a short secret as a word after =": {env: map[string]string{"DB_PASS": "s3cr7"}, test: []string{"CMD-SHELL", "echo --db-pass=s3cr7"}, named: true},
	} {
		t.Run(name, func(t *testing.T) {
			spec := &resourcev1.Container{Image: "x:1", Env: c.env, Entrypoint: slices.Clone(c.entry), Command: slices.Clone(c.cmd)}
			if c.test != nil {
				spec.Healthcheck = &resourcev1.Healthcheck{Test: slices.Clone(c.test)}
			}
			nameValues(spec)
			if got := len(spec.GetReferences()) > 0; got != c.named {
				t.Fatalf("named %v (%q %q %q), want %v", spec.GetReferences(), spec.GetEntrypoint(), spec.GetCommand(), spec.GetHealthcheck().GetTest(), c.named)
			}
			if !c.named {
				if len(valuesShown(spec)) == 0 {
					t.Error("it isn't named, and nothing refuses it")
				}
				return
			}
			test := spec.GetHealthcheck().GetTest()
			rendered, err := binding.RenderField(test, c.env, binding.ScriptIn(spec, binding.FieldTest))
			if err != nil {
				t.Fatal(err)
			}
			if was, now := run(c.test[1], c.env), run(rendered[1], c.env); was != now {
				t.Errorf("sh ran %q as %q, and runs %q as %q", c.test[1], was, rendered[1], now)
			}
		})
	}

	// A secret that is the shell itself isn't named there, as the script would
	// then be no script, and its values would go into the command line.
	spec := &resourcev1.Container{
		Image: "x:1", Env: map[string]string{"SHELL_PATH_KEY": "/bin/sh", "DB_PASSWORD": "k8Jd2mQx"},
		Entrypoint: []string{"/bin/sh", "-c"}, Command: []string{"echo k8Jd2mQx"},
	}
	nameValues(spec)
	if !slices.Equal(spec.GetEntrypoint(), []string{"/bin/sh", "-c"}) || !slices.Equal(spec.GetCommand(), []string{"echo ${DB_PASSWORD}"}) || binding.ScriptIn(spec, binding.FieldCommand) != 0 {
		t.Errorf("a secret that is the shell named %q and %q", spec.GetEntrypoint(), spec.GetCommand())
	}
}
