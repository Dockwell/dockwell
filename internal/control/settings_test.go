// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/identity"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// The install's name and time zone aren't managed state: a change needs no plan, is
// refused when it can't be one, and is on the activity log with what it replaced,
// once (ADR 0042). The sign-in screen says the name.
func TestTheInstallsSettings(t *testing.T) {
	storetest.Each(t, testTheInstallsSettings)
}

func testTheInstallsSettings(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	c := dockwellv1connect.NewSettingsServiceClient(b, b.origin, connect.WithProtoJSON())

	got, err := c.GetSettings(t.Context(), connect.NewRequest(&dockwellv1.GetSettingsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	s := got.Msg.GetSettings()
	if s.GetName() != "" || s.GetTimeZone() != "UTC" || s.GetCreateTime() == nil {
		t.Errorf("a new install's settings = %v, want no name, UTC, and when it was set up", s)
	}
	if s.GetPanel().GetDomain() != "" || s.GetPanel().GetServedBy() != dockwellv1.PanelServedBy_PANEL_SERVED_BY_NONE {
		t.Errorf("a panel with no domain is served as %v", s.GetPanel())
	}

	named, err := c.SetInstallName(t.Context(), connect.NewRequest(&dockwellv1.SetInstallNameRequest{Name: "  Acme servers "}))
	if err != nil {
		t.Fatal(err)
	}
	if named.Msg.GetSettings().GetName() != "Acme servers" {
		t.Errorf("named %q, want it trimmed", named.Msg.GetSettings().GetName())
	}
	// The same name again changes nothing, and isn't on record twice.
	if _, err := c.SetInstallName(t.Context(), connect.NewRequest(&dockwellv1.SetInstallNameRequest{Name: "Acme servers"})); err != nil {
		t.Fatal(err)
	}
	state, err := b.authClient().GetAuthState(t.Context(), connect.NewRequest(&dockwellv1.GetAuthStateRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if state.Msg.GetInstallName() != "Acme servers" {
		t.Errorf("the sign-in screen's state names the install %q", state.Msg.GetInstallName())
	}
	for name, why := range map[string]string{
		strings.Repeat("a", 65): "64 at most",
		"two\nlines":            "one line",
	} {
		_, err := c.SetInstallName(t.Context(), connect.NewRequest(&dockwellv1.SetInstallNameRequest{Name: name}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), why) {
			t.Errorf("naming the install %q: %v, want InvalidArgument saying %q", name, err, why)
		}
	}

	zoned, err := c.SetTimeZone(t.Context(), connect.NewRequest(&dockwellv1.SetTimeZoneRequest{TimeZone: "Europe/Amsterdam"}))
	if err != nil {
		t.Fatal(err)
	}
	if zoned.Msg.GetSettings().GetTimeZone() != "Europe/Amsterdam" {
		t.Errorf("time zone = %q", zoned.Msg.GetSettings().GetTimeZone())
	}
	for _, zone := range []string{"Mars/Olympus_Mons", "Local", "/etc/passwd", "../../etc/passwd"} {
		_, err := c.SetTimeZone(t.Context(), connect.NewRequest(&dockwellv1.SetTimeZoneRequest{TimeZone: zone}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("setting the time zone %q: %v, want InvalidArgument", zone, err)
		}
	}
	back, err := c.SetTimeZone(t.Context(), connect.NewRequest(&dockwellv1.SetTimeZoneRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if back.Msg.GetSettings().GetTimeZone() != "UTC" {
		t.Errorf("clearing the time zone left %q, want UTC", back.Msg.GetSettings().GetTimeZone())
	}

	events := listEvents(t, dockwellv1connect.NewEventServiceClient(b, b.origin), &dockwellv1.ListEventsRequest{
		Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_SETTINGS},
	})
	var said []string
	for _, e := range events {
		st := e.GetSetting()
		said = append(said, st.GetSetting()+":"+st.GetPrevious()+">"+st.GetValue())
		if e.GetActor().GetName() == "" {
			t.Errorf("a settings event names nobody: %v", e)
		}
	}
	want := []string{"time_zone:Europe/Amsterdam>", "time_zone:>Europe/Amsterdam", "name:>Acme servers"}
	if strings.Join(said, ", ") != strings.Join(want, ", ") {
		t.Errorf("the timeline has %v, want %v, newest first", said, want)
	}
}

// The control plane says what it runs, where it keeps its state and the key,
// where agents reach it, and how it's doing, so the panel can say how to back
// it up and how to upgrade it.
func TestTheControlPlaneSaysWhatItIs(t *testing.T) {
	storetest.Each(t, testTheControlPlaneSaysWhatItIs)
}

func testTheControlPlaneSaysWhatItIs(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	if err := f.ident.SetAgentURL(t.Context(), "https://192.0.2.10:7443"); err != nil {
		t.Fatal(err)
	}
	if err := f.ident.SetPublicAgentURLs(t.Context(), []identity.JoinURL{{URL: "https://203.0.113.7:7443", WayIn: true}}); err != nil {
		t.Fatal(err)
	}
	b := signedIn(t, f)
	c := dockwellv1connect.NewSettingsServiceClient(b, b.origin, connect.WithProtoJSON())
	got, err := c.GetControlPlane(t.Context(), connect.NewRequest(&dockwellv1.GetControlPlaneRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	cp := got.Msg.GetControlPlane()
	if cp.GetBuild().GetVersion() != testBuild.Version || cp.GetStartTime() == nil {
		t.Errorf("build %v, started %v: want the one that runs, and when", cp.GetBuild(), cp.GetStartTime())
	}
	if cp.GetDataDir() != f.config.DataDir || cp.GetKeyFile() != filepath.Join(f.config.DataDir, "key") {
		t.Errorf("data in %q, key %q", cp.GetDataDir(), cp.GetKeyFile())
	}
	st := cp.GetStore()
	engine := map[store.Backend]dockwellv1.StateStoreEngine{
		store.SQLite:   dockwellv1.StateStoreEngine_STATE_STORE_ENGINE_SQLITE,
		store.Postgres: dockwellv1.StateStoreEngine_STATE_STORE_ENGINE_POSTGRES,
	}[backend]
	if st.GetEngine() != engine || st.GetVersion() == "" || st.GetSizeBytes() <= 0 || st.GetRoundTrip() == nil {
		t.Errorf("the state store is %v", st)
	}
	if strings.Contains(st.GetLocation(), "password") || strings.Contains(st.GetLocation(), ":"+"secret@") {
		t.Errorf("the state store's location %q says its password", st.GetLocation())
	}
	if backend == store.SQLite && st.GetLocation() != f.db {
		t.Errorf("the SQLite file is at %q, want %q", st.GetLocation(), f.db)
	}
	agents := cp.GetAgents()
	if agents.GetListen() != f.agentAddr || agents.GetUrl() != "https://192.0.2.10:7443" ||
		strings.Join(agents.GetPublicUrls(), " ") != "https://203.0.113.7:7443" {
		t.Errorf("agents reach it as %v", agents)
	}
	if cp.GetPanel().GetListen() != f.config.PanelListen || cp.GetPanel().GetServedBy() != dockwellv1.PanelServedBy_PANEL_SERVED_BY_NONE {
		t.Errorf("the panel is served as %v", cp.GetPanel())
	}
	if runtime.GOOS == "linux" && cp.GetMemoryBytes() < 1<<20 {
		t.Errorf("it holds %d bytes, want its resident set", cp.GetMemoryBytes())
	}
}
