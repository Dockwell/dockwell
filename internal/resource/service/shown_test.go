// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

func shownApp(env map[string]string, shown ...string) *resourcev1.Service {
	return &resourcev1.Service{
		Runs:     &resourcev1.Service_App{App: &resourcev1.Container{Image: "wordpress:6", Env: env}},
		ShownEnv: shown,
	}
}

// Which values a read shows names only what the app's environment has, sorted
// and once each, so two specs that mean the same compare equal (ADR 0081).
func TestShownEnvIsTidied(t *testing.T) {
	svc := shownApp(map[string]string{"DB_HOST": "db", "DB_NAME": "wp"}, "DB_NAME", "GONE", "DB_HOST", "DB_NAME")
	if err := (&Driver{}).Validate(svc); err != nil {
		t.Fatal(err)
	}
	if want := []string{"DB_HOST", "DB_NAME"}; !slices.Equal(svc.GetShownEnv(), want) {
		t.Errorf("shown_env = %v, want %v", svc.GetShownEnv(), want)
	}
	none := shownApp(map[string]string{"DB_HOST": "db"}, "GONE")
	if err := (&Driver{}).Validate(none); err != nil {
		t.Fatal(err)
	}
	if none.ShownEnv != nil {
		t.Errorf("shown_env = %#v, want nil", none.ShownEnv)
	}
}

// Servers never see which values are shown, so a change of it alone restarts
// nothing, and drift never reads it as missing.
func TestShownEnvStaysOnTheControlPlane(t *testing.T) {
	svc := shownApp(map[string]string{"DB_HOST": "db"}, "DB_HOST")
	on := (&Driver{}).OnServer(svc).(*resourcev1.Service)
	if on.GetShownEnv() != nil {
		t.Errorf("the server's spec shows %v", on.GetShownEnv())
	}
	if svc.GetShownEnv() == nil {
		t.Error("OnServer changed the spec it was given")
	}
	if !resource.Equal(on, (&Driver{}).OnServer(shownApp(map[string]string{"DB_HOST": "db"}))) {
		t.Error("two specs that differ only in what's shown differ on the server")
	}
}

// Drift kept as the latest revision shows a value only while the server has the
// one shown: one changed there by hand could be anything.
func TestReadoptedKeepsShownWhileTheValueIsTheSame(t *testing.T) {
	current := shownApp(map[string]string{"DB_HOST": "db", "DB_NAME": "wp"}, "DB_HOST", "DB_NAME")
	observed := shownApp(map[string]string{"DB_HOST": "db", "DB_NAME": "hunter2", "NEW": "x"})
	kept := (&Driver{}).Readopted(current, observed).(*resourcev1.Service)
	if want := []string{"DB_HOST"}; !slices.Equal(kept.GetShownEnv(), want) {
		t.Errorf("shown_env = %v, want %v", kept.GetShownEnv(), want)
	}
	if kept.GetAutoUpdate() != nil {
		t.Errorf("auto_update = %v, want none", kept.GetAutoUpdate())
	}
	if observed.GetShownEnv() != nil {
		t.Error("Readopted changed what was observed")
	}
}

// Keeping a value without reading it can't show it: whoever shows a value gives it.
func TestKeepEnvCantShowWhatNobodySaw(t *testing.T) {
	current := shownApp(map[string]string{"DB_HOST": "db", "DB_PASSWORD": "hunter2"}, "DB_HOST")
	for _, tc := range []struct {
		name    string
		desired *resourcev1.Service
		keep    []string
		refused string
	}{
		{"keeps what's shown already", shownApp(nil, "DB_HOST"), []string{"DB_HOST", "DB_PASSWORD"}, ""},
		{"keeps a hidden one hidden", shownApp(nil), []string{"DB_PASSWORD"}, ""},
		{"shows one it gives", shownApp(map[string]string{"DB_PASSWORD": "new"}, "DB_PASSWORD"), []string{"DB_HOST"}, ""},
		{"shows one it keeps", shownApp(nil, "DB_HOST", "DB_PASSWORD"), []string{"DB_HOST", "DB_PASSWORD"}, "shows DB_PASSWORD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desired := proto.CloneOf(tc.desired)
			_, err := (&Driver{}).KeepEnv(current, desired, tc.keep)
			switch {
			case tc.refused == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)):
				t.Fatalf("err = %v, want one that says %q", err, tc.refused)
			}
		})
	}
}
