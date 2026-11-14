// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

// A MySQL app's user is read first, changing nothing, then kept out of its database
// with a password of the restore's own, marked by what the server keeps of it, its
// connections ended, and let back in with how it signed in before, exactly, as the
// server says it, losing the mark. With the restore's password still its own and no
// record of how it signed in before, it isn't read again as the app's; a mark of a
// password it no longer has is no bar. A database with more than its one user, or a
// user Dockwell didn't make, isn't kept out (ADR 0084).
func TestAMySQLUserIsKeptOutAndLetBackIn(t *testing.T) {
	var mu sync.Mutex
	var said []string
	users := "shop\t%\n"
	const original = "CREATE USER `shop`@`%` IDENTIFIED WITH 'caching_sha2_password' AS 0x24412430303524 REQUIRE NONE ACCOUNT UNLOCK"
	create := original
	marks := map[string]bool{}
	revokeFails := false
	e := dockertest.New(t)
	e.AddLocalImage("mysql:8.4", dockertest.Image{Config: docker.Config{Cmd: []string{"mysqld"}}, Exec: func(x dockertest.Exec) int {
		b, _ := io.ReadAll(x.Stdin)
		sql := string(b)
		mu.Lock()
		defer mu.Unlock()
		said = append(said, sql)
		switch {
		case strings.Contains(sql, "FROM mysql.db WHERE Db IN"):
			_, _ = io.WriteString(x.Stdout, users)
		case strings.Contains(sql, "SHOW CREATE USER"):
			_, _ = io.WriteString(x.Stdout, create+"\n")
		case strings.Contains(sql, "AND Db LIKE 'dockwellrestoring%'"):
			for m := range marks {
				_, _ = io.WriteString(x.Stdout, m+"\n")
			}
		case strings.Contains(sql, "PROCESSLIST"):
			if strings.Contains(sql, "IDENTIFIED BY '") {
				create = "CREATE USER `shop`@`%` IDENTIFIED WITH 'caching_sha2_password' AS 0x2441243030352472657374 REQUIRE NONE ACCOUNT UNLOCK"
			}
			_, _ = io.WriteString(x.Stdout, "12\n13\n")
		case strings.HasPrefix(sql, "GRANT SELECT ON `dockwellrestoring"):
			marks[regexp.MustCompile("`(dockwellrestoring[0-9a-f]+)`").FindStringSubmatch(sql)[1]] = true
		case strings.Contains(sql, "REVOKE SELECT ON"):
			if revokeFails {
				_, _ = io.WriteString(x.Stderr, "ERROR 1205 (HY000): Lock wait timeout exceeded\n")
				return 1
			}
			for m := range marks {
				if strings.Contains(sql, "`"+m+"`") {
					delete(marks, m)
				}
			}
		case strings.Contains(sql, "ALTER USER `shop`"):
			create = original
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
	l, err := ReadLockout(t.Context(), client, c, my, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if all := strings.Join(said, ""); strings.Contains(all, "ALTER USER") || strings.Contains(all, "GRANT") {
		t.Errorf("reading who to keep out changed something: %q", all)
	}
	password, err := LockOut(t.Context(), client, c, my, "shop", l)
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
	if len(marks) != 1 {
		t.Fatalf("the restore's password is marked %v", marks)
	}

	// Its record gone, a restore doesn't take the restore's password for the app's.
	if _, err := ReadLockout(t.Context(), client, c, my, "shop"); err == nil || !strings.Contains(err.Error(), "with the password an earlier restore gave it") {
		t.Errorf("reading a user a restore keeps out = %v", err)
	}

	said = nil
	if err := LetIn(t.Context(), client, c, my, "shop", l); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(said, ""); !strings.Contains(got, "ALTER USER "+strings.TrimPrefix(original, "CREATE USER ")+";") {
		t.Errorf("letting it in said %q", got)
	}
	if len(marks) != 0 {
		t.Errorf("let in, it keeps the marks %v", marks)
	}

	// A mark of a password it no longer has, as one set anew, is no bar; one that
	// stays as it's let in is only a warning.
	marks["dockwellrestoring000000000000"] = true
	if l, err = ReadLockout(t.Context(), client, c, my, "shop"); err != nil {
		t.Fatalf("a stale mark = %v", err)
	}
	if _, err := LockOut(t.Context(), client, c, my, "shop", l); err != nil {
		t.Fatal(err)
	}
	revokeFails = true
	if err := LetIn(t.Context(), client, c, my, "shop", l); !errors.Is(err, ErrLoaderLeft) || create != original {
		t.Errorf("a mark that stays = %v, it signs in with %q", err, create)
	}
	revokeFails = false

	for _, list := range []string{"shop\t%\nother\t%\n", "shop\tlocalhost\n", ""} {
		users = list
		if _, err := ReadLockout(t.Context(), client, c, my, "shop"); err == nil || !strings.Contains(err.Error(), "Restore the whole server instead") {
			t.Errorf("users %q: %v", list, err)
		}
	}
	if _, err := LockOut(t.Context(), client, c, my, "shop", nil); err == nil {
		t.Error("it kept a user out with no record of how to let them in")
	}
}

// A MySQL or MariaDB database is emptied, before a dump of it loads, of each table,
// view, routine and event it has now, by names it reads in hex, so any name drops as
// itself (ADR 0084).
func TestAMySQLDatabaseIsEmptiedBeforeALoad(t *testing.T) {
	hexOf := func(s string) string {
		const digits = "0123456789ABCDEF"
		out := make([]byte, 0, 2*len(s))
		for i := range len(s) {
			out = append(out, digits[s[i]>>4], digits[s[i]&15])
		}
		return string(out)
	}
	got, err := MyEmptying("TABLE\t" + hexOf("orders") + "\nVIEW\t" + hexOf("totals") + "\nTABLE\t" + hexOf("odd`name") + "\nPROCEDURE\t" + hexOf("tidy") + "\nEVENT\t" + hexOf("nightly") + "\n")
	if err != nil {
		t.Fatal(err)
	}
	want := "SET FOREIGN_KEY_CHECKS = 0;\nDROP VIEW IF EXISTS `totals`;\nDROP TABLE IF EXISTS `orders`, `odd``name`;\nDROP PROCEDURE IF EXISTS `tidy`;\nDROP EVENT IF EXISTS `nightly`;\n"
	if got != want {
		t.Errorf("it empties with %q, want %q", got, want)
	}
	if got, err := MyEmptying(""); err != nil || got != "SET FOREIGN_KEY_CHECKS = 0;\n" {
		t.Errorf("an empty database empties with %q, %v", got, err)
	}
	if _, err := MyEmptying("TABLE\tnot-hex\n"); err == nil {
		t.Error("it read a name that isn't hex")
	}
}
