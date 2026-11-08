// SPDX-License-Identifier: AGPL-3.0-only

package importapi

import (
	"strings"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

func stepFor(t *testing.T, steps []*dockwellv1.ImportStep, kind dockwellv1.ImportKind, name string) *dockwellv1.ImportStep {
	t.Helper()
	for _, s := range steps {
		if s.GetKind() == kind && s.GetName() == name {
			return s
		}
	}
	t.Fatalf("no %v %s in %v", kind, name, steps)
	return nil
}

// What Dockwell can't run stays, with what a person can do about it.
func TestWhatCantComeSaysWhy(t *testing.T) {
	steps := Steps(&dockwellv1.OnePanel{
		Version: "v2.6.0", BaseDir: "/opt",
		Runtimes: []*dockwellv1.OnePanelRuntime{
			{Name: "php74", Type: "php", Version: "7.4.33"},
			{Name: "php86", Type: "php", Version: "8.6.0"},
			{Name: "php84", Type: "php", Version: "8.4.1"},
			{Name: "idle", Type: "php", Version: "8.3.0"},
		},
		Websites: []*dockwellv1.OnePanelWebsite{
			{PrimaryDomain: "old.example.com", Type: "runtime", Runtime: "php74", Domains: []*dockwellv1.OnePanelDomain{{Domain: "old.example.com"}}},
			{PrimaryDomain: "next.example.com", Type: "runtime", Runtime: "php86"},
			{
				PrimaryDomain: "now.example.com", Type: "runtime", Runtime: "php84", Folder: "/opt/1panel/www/sites/now/index",
				Domains: []*dockwellv1.OnePanelDomain{{Domain: "now.example.com"}, {Domain: "a.example.com"}, {Domain: "b.example.com"}},
			},
			{PrimaryDomain: "odd.example.com", Type: "mystery"},
			{PrimaryDomain: "tcp.example.com", Type: "stream"},
			{PrimaryDomain: "local.example.com", Type: "proxy", Proxy: "http://127.0.0.1:3000"},
			{PrimaryDomain: "lost.example.com", Type: "proxy"},
		},
		Certificates: []*dockwellv1.OnePanelCertificate{
			{PrimaryDomain: "bought.example.com", Kind: "manual"},
			{PrimaryDomain: "self.example.com", Kind: "selfSigned"},
			{PrimaryDomain: "zero.example.com", Kind: "autoSSL", Provider: "zerossl"},
		},
		CronJobs: []*dockwellv1.OnePanelCronJob{
			{Name: "who knows", Type: "mystery", Status: "Enable"},
			{Name: "snap", Type: "snapshot", Status: "Enable"},
			{Name: "twice", Type: "shell", Schedule: "0 1 * * *\n0 13 * * *", ScriptLines: 1, Status: "Enable"},
		},
	})
	leave, again := dockwellv1.ImportAction_IMPORT_ACTION_LEAVE, dockwellv1.ImportAction_IMPORT_ACTION_MAKE
	for _, c := range []struct {
		kind   dockwellv1.ImportKind
		name   string
		action dockwellv1.ImportAction
		says   string
	}{
		{dockwellv1.ImportKind_IMPORT_KIND_RUNTIME, "php74", leave, "PHP 8.2 to 8.5"},
		{dockwellv1.ImportKind_IMPORT_KIND_RUNTIME, "php86", leave, "PHP 8.2 to 8.5"},
		{dockwellv1.ImportKind_IMPORT_KIND_RUNTIME, "php84", again, "PHP 8.4 in each website"},
		{dockwellv1.ImportKind_IMPORT_KIND_RUNTIME, "idle", leave, "No website runs on it"},
		{dockwellv1.ImportKind_IMPORT_KIND_WEBSITE, "old.example.com", leave, "Move it to one of those in 1Panel"},
		{dockwellv1.ImportKind_IMPORT_KIND_WEBSITE, "now.example.com", again, "its 2 other domains"},
		{dockwellv1.ImportKind_IMPORT_KIND_WEBSITE, "odd.example.com", leave, `1Panel calls "mystery"`},
		{dockwellv1.ImportKind_IMPORT_KIND_WEBSITE, "tcp.example.com", leave, "no TCP or UDP proxy"},
		{dockwellv1.ImportKind_IMPORT_KIND_WEBSITE, "local.example.com", again, "port 3000 on the server"},
		{dockwellv1.ImportKind_IMPORT_KIND_WEBSITE, "lost.example.com", leave, "can't be read"},
		{dockwellv1.ImportKind_IMPORT_KIND_CERTIFICATE, "bought.example.com", leave, "a tunnel or a proxy that ends TLS"},
		{dockwellv1.ImportKind_IMPORT_KIND_CERTIFICATE, "self.example.com", leave, "a self-signed certificate"},
		{dockwellv1.ImportKind_IMPORT_KIND_CERTIFICATE, "zero.example.com", leave, "from ZeroSSL"},
		{dockwellv1.ImportKind_IMPORT_KIND_CRON_JOB, "who knows", leave, `1Panel calls "mystery"`},
		{dockwellv1.ImportKind_IMPORT_KIND_CRON_JOB, "snap", leave, "1Panel itself"},
		{dockwellv1.ImportKind_IMPORT_KIND_CRON_JOB, "twice", again, "on 0 1 * * *, 0 13 * * *"},
		{dockwellv1.ImportKind_IMPORT_KIND_PANEL, "1Panel", leave, "v2.6.0 is read the same way"},
	} {
		s := stepFor(t, steps, c.kind, c.name)
		text := s.GetSummary() + " | " + s.GetBecomes() + " | " + s.GetReason()
		if s.GetAction() != c.action || !strings.Contains(text, c.says) {
			t.Errorf("%v %s = %v %q, want %v saying %q", c.kind, c.name, s.GetAction(), text, c.action, c.says)
		}
		if s.GetAction() == leave && s.GetReason() == "" {
			t.Errorf("%v %s stays with no reason", c.kind, c.name)
		}
	}
}

// Every step says what it is, and every one that stays says why.
func TestEveryStepSaysWhatItIs(t *testing.T) {
	for _, s := range Steps(&dockwellv1.OnePanel{Version: "v2.4.2", KnownVersion: true}) {
		if s.GetSummary() == "" || s.GetAction() == dockwellv1.ImportAction_IMPORT_ACTION_UNSPECIFIED {
			t.Errorf("a step says nothing: %v", s)
		}
		if strings.Contains(s.GetReason(), "check what it found") {
			t.Errorf("a version this was written from says to check: %v", s)
		}
	}
}
