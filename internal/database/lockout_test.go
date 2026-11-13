// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"io"
	"strings"
	"sync"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// A MySQL app's user is kept out of its database with a password of the restore's
// own, its connections ended, and let back in with how it signed in before, exactly,
// as the server says it; a database with more than its one user, or a user Dockwell
// didn't make, isn't kept out (ADR 0084).
func TestAMySQLUserIsKeptOutAndLetBackIn(t *testing.T) {
	var mu sync.Mutex
	var said []string
	users := "shop\t%\n"
	e := dockertest.New(t)
	e.AddLocalImage("mysql:8.4", dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}, Exec: func(x dockertest.Exec) int {
		b, _ := io.ReadAll(x.Stdin)
		sql := string(b)
		mu.Lock()
		defer mu.Unlock()
		said = append(said, sql)
		switch {
		case strings.Contains(sql, "FROM mysql.db"):
			_, _ = io.WriteString(x.Stdout, users)
		case strings.Contains(sql, "SHOW CREATE USER"):
			_, _ = io.WriteString(x.Stdout, "CREATE USER `shop`@`%` IDENTIFIED WITH 'caching_sha2_password' AS 0x24412430303524 REQUIRE NONE ACCOUNT UNLOCK\n")
		case strings.Contains(sql, "PROCESSLIST"):
			_, _ = io.WriteString(x.Stdout, "12\n13\n")
		}
		return 0
	}})
	client := e.Client()
	id, err := client.CreateContainer(t.Context(), "env_1-db-1", &docker.CreateRequest{Config: docker.Config{Image: "mysql:8.4"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	c, err := client.InspectContainer(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	my := Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL)
	l, password, err := LockOut(t.Context(), client, c, my, "shop", nil)
	if err != nil {
		t.Fatal(err)
	}
	if user, prelude := l.Signer(password); user != "shop" || prelude != "" || password == "" {
		t.Errorf("it loads as %q with %q", user, prelude)
	}
	all := strings.Join(said, "")
	if !strings.Contains(all, "print_identified_with_as_hex = ON") || !strings.Contains(all, "ALTER USER 'shop'@'%' IDENTIFIED BY '"+password+"'") || !strings.Contains(all, "KILL 12;\nKILL 13;\n") {
		t.Errorf("it said %q", all)
	}
	said = nil
	if err := LetIn(t.Context(), client, c, my, "shop", l); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(said, ""); !strings.Contains(got, "ALTER USER `shop`@`%` IDENTIFIED WITH 'caching_sha2_password' AS 0x24412430303524 REQUIRE NONE ACCOUNT UNLOCK;") {
		t.Errorf("letting it in said %q", got)
	}
	for _, list := range []string{"shop\t%\nother\t%\n", "shop\tlocalhost\n", ""} {
		users = list
		if _, _, err := LockOut(t.Context(), client, c, my, "shop", nil); err == nil || !strings.Contains(err.Error(), "Restore the whole server instead") {
			t.Errorf("users %q: %v", list, err)
		}
	}
}
