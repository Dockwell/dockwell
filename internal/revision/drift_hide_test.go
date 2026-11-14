// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// What drift says of a command, an entrypoint or a health check masks the values of
// the revision's environment, should an agent ever read the text Docker has
// (ADR 0096); what it says of any other field is as it was.
func TestDriftMasksAValueInWhatRuns(t *testing.T) {
	const pw = "k8Jd2mQxR4"
	want := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "mariadb:11", Env: map[string]string{"MARIADB_ROOT_PASSWORD": pw}}}}
	changes := hideInDrift([]resource.Change{
		{Path: "app.healthcheck.test", From: "CMD-SHELL, mariadb-admin ping -p${MARIADB_ROOT_PASSWORD}", To: "CMD-SHELL, mariadb-admin ping -p" + pw},
		{Path: "app.command[1]", To: "--password=" + pw},
		{Path: "app.image", To: "mariadb:" + pw},
	}, want)
	for _, ch := range changes[:2] {
		if strings.Contains(ch.From+ch.To, pw) {
			t.Errorf("%s says %q", ch.Path, ch.To)
		}
	}
	if changes[2].To != "mariadb:"+pw {
		t.Errorf("the image reads %q", changes[2].To)
	}
}
