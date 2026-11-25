// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

func TestFail2banActionsProtectAccess(t *testing.T) {
	s := newStandIns(t)
	s.tool(t, "/usr/bin/fail2ban-client", `case "$*" in
status) printf 'Status\n|- Number of jail:\t1\n\140- Jail list:\tsshd\n' ;;
"get sshd ignoreip") printf 'These IP addresses/networks are ignored:\n|- 198.51.100.0/24\n' ;;
"set sshd banip "*|"set sshd unbanip "*) printf '%s\n' "$*" >> "$0.log" ;;
*) exit 17 ;;
esac
`)
	r := newFail2banReader(Config{
		Tools:             s.runner(),
		Fail2banProtected: func(context.Context) ([]string, error) { return []string{"203.0.113.44"}, nil },
		SSHSessions:       func(context.Context) []string { return []string{"2001:db8::29"} },
	})
	for _, addr := range []string{"::ffff:203.0.113.44", "198.51.100.5", "2001:db8::29", "127.0.0.1", "203.0.113.5/24"} {
		req := &v1.SetFail2BanAddressBanRequest{Jail: "sshd", Address: addr, Banned: true}
		if err := r.setBan(t.Context(), req); resource.ReasonOf(err) != resource.ReasonSSHRefused {
			t.Errorf("ban %s: want protection refusal, got %v", addr, err)
		}
	}
	if err := r.setBan(t.Context(), &v1.SetFail2BanAddressBanRequest{Jail: "sshd", Address: "192.0.2.36", Banned: true}); err != nil {
		t.Fatal(err)
	}
	if err := r.setBan(t.Context(), &v1.SetFail2BanAddressBanRequest{Jail: "sshd", Address: "::ffff:203.0.113.44"}); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(filepath.Join(s.runner().Root, "usr", "bin", "fail2ban-client.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(log)); got != "set sshd banip 192.0.2.36\nset sshd unbanip 203.0.113.44" {
		t.Fatalf("unexpected operations: %s", got)
	}
}
