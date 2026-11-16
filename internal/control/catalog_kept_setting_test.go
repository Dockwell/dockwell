// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

// settingEntry is the test entry with a setting a person may give, TIMEZONE, which
// web reads as TZ: as Dokploy's syncthing converted before #570's re-check, an
// optional input with nothing in its place where it's left empty; or, as it converts
// now, with the template's own value as its default, in the file too.
func settingEntry(release int, version, image string, now bool) string {
	read, input := "TZ: ${TIMEZONE}", "  - name: TIMEZONE\n    label: Timezone\n    type: INPUT_TYPE_TEXT\n    optional: true\n"
	if now {
		read, input = "TZ: ${TIMEZONE:-America/Sao_Paulo}", input+"    defaultValue: America/Sao_Paulo\n"
	}
	e := strings.Replace(testEntry(release, version, image, ""), "PASSWORD: ${PASSWORD}", "PASSWORD: ${PASSWORD}\n        "+read, 1)
	return strings.Replace(e, "compose: |", strings.TrimSuffix(input, "\n")+"\ncompose: |", 1)
}

// An install made before a store's plain setting had the template's value as its
// default keeps what a person set it to through an upgrade to a release that has
// one: the setting stays an input under the same name, so the upgrade carries it
// (#570's re-check, H-A). Before, the release wrote the template's value in its place,
// and the upgrade dropped the person's.
func TestAnUpgradeKeepsASettingAPersonSet(t *testing.T) {
	t.Parallel()
	u := startUpgradingWith(t, store.SQLite, settingEntry(1, "1.0", firstRelease, false),
		&dockwellv1.InstallCatalogEntryRequest{Id: "thing", Values: map[string]string{"TIMEZONE": "Europe/Berlin"}})
	tz := func() string {
		t.Helper()
		for _, kv := range u.web().Config.Env {
			if v, ok := strings.CutPrefix(kv, "TZ="); ok {
				return v
			}
		}
		return ""
	}
	if got := tz(); got != "Europe/Berlin" {
		t.Fatalf("web runs with TZ %q, want the person's Europe/Berlin", got)
	}
	u.release(settingEntry(2, "2.0", secondRelease, true))
	u.applied(u.upgrade())
	if got := tz(); got != "Europe/Berlin" {
		t.Errorf("after the upgrade web runs with TZ %q, want the person's Europe/Berlin", got)
	}
	if got := u.install().GetPin().GetValues()["TIMEZONE"]; got != "Europe/Berlin" {
		t.Errorf("the install's pin keeps TIMEZONE %q", got)
	}
}
