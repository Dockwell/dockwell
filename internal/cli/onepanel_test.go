// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/agent/onepanel/onepaneltest"
)

// A test reads no 1Panel of the machine it runs on: only what DOCKWELL_TEST_ONEPANEL
// names, and nothing without it.
func TestATestReadsOnlyThe1PanelItNames(t *testing.T) {
	env := func(vars map[string]string) IO {
		return IO{Getenv: func(k string) string { return vars[k] }}
	}
	res, err := serverOnePanel(env(map[string]string{envTest: "1"}))(t.Context())
	if err != nil || res.GetFound() != nil || !strings.Contains(res.GetNotHere(), envTestOnePanel) {
		t.Errorf("a test with no root = %v, %v", res, err)
	}
	root := onepaneltest.Make(t, "/opt", false)
	res, err = serverOnePanel(env(map[string]string{envTest: "1", envTestOnePanel: root}))(t.Context())
	if err != nil || res.GetFound().GetVersion() != "v2.4.2" {
		t.Errorf("a test with a root = %v, %v", res, err)
	}
}
