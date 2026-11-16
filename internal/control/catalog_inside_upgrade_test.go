// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/url"
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

// insideEnv is web's environment, by name.
func insideEnv(t *testing.T, u *upgrading) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kv := range u.web().Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

// A person points web's URL at a database of their own, by hand, and the next
// release changes only the image: an upgrade keeps what a person changed and the
// release didn't, as its merge reads the file the install ran with the value named
// as the install has it (#570's review, H2).
func TestAPersonsOwnURLStaysThroughAnUpgrade(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, insideEntry(1, "1.0", firstRelease, ""))
	const own = "postgres://me:own@elsewhere:5432/mine"
	u.edit("Use my own database", "web", &resourcev1.Service{
		Secrets: map[string]string{"PASSWORD": "PASSWORD"},
		Runs: &resourcev1.Service_App{App: &resourcev1.Container{
			Image: firstRelease, Networks: []string{"default"}, Restart: resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED,
			Env:    map[string]string{"URL": own},
			Ports:  []*resourcev1.PortMapping{{HostPort: 18100, ContainerPort: 80}},
			Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: "/data"}},
		}},
	})
	if got := insideEnv(t, u)["URL"]; got != own {
		t.Fatalf("after the edit web reads URL %q", got)
	}
	u.release(insideEntry(2, "2.0", secondRelease, ""))
	up := u.upgrade()
	for _, n := range up.GetPlan().GetNotes() {
		t.Logf("note %s: %s", n.GetReason(), n.GetMessage())
	}
	u.applied(up)
	if got := insideEnv(t, u)["URL"]; got != own {
		t.Errorf("after an upgrade that changes only the image, web reads URL %q, want the person's %q", got, own)
	}
}

// Undoing a rotation and undoing an upgrade each leave a URL that holds the password
// the app has then (#570's review).
func TestUndoKeepsTheURLWithItsPassword(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, insideEntry(1, "1.0", firstRelease, ""))
	first := insideEnv(t, u)["PASSWORD"]
	rotate := propose(t, u.c, "Set the password anew", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: u.env, Name: "PASSWORD"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: "p@ss:w/rd?#%+ é"}}},
	})
	u.applied(rotate)
	got := insideEnv(t, u)
	if want := "postgres://u:" + strings.TrimSuffix(url.UserPassword("p@ss:w/rd?#%+ é", "").String(), ":") + "@db:5432/x?port=18100"; got["URL"] != want {
		t.Errorf("after the rotation URL %q, want %q", got["URL"], want)
	}
	if p, err := url.Parse(got["URL"]); err != nil {
		t.Errorf("the URL doesn't parse: %v", err)
	} else if pw, _ := p.User.Password(); pw != "p@ss:w/rd?#%+ é" || p.Host != "db:5432" || p.Query().Get("port") != "18100" {
		t.Errorf("parsed, the URL has password %q host %q port %q", pw, p.Host, p.Query().Get("port"))
	}
	undo, err := u.c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: rotate.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	u.applied(undo.Msg.GetChange())
	got = insideEnv(t, u)
	if got["PASSWORD"] != first || got["URL"] != "postgres://u:"+url.PathEscape(first)+"@db:5432/x?port=18100" {
		t.Errorf("after undoing the rotation web reads PASSWORD %q URL %q", got["PASSWORD"], got["URL"])
	}

	u.release(insideEntry(2, "2.0", secondRelease, "&sslmode=disable"))
	up := u.upgrade()
	u.applied(up)
	if got := insideEnv(t, u); got["URL"] != "postgres://u:"+url.PathEscape(first)+"@db:5432/x?port=18100&sslmode=disable" {
		t.Errorf("after the upgrade URL %q", got["URL"])
	}
	undo, err = u.c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: up.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	u.applied(undo.Msg.GetChange())
	if got := insideEnv(t, u); got["PASSWORD"] != first || got["URL"] != "postgres://u:"+url.PathEscape(first)+"@db:5432/x?port=18100" {
		t.Errorf("after undoing the upgrade web reads PASSWORD %q URL %q", got["PASSWORD"], got["URL"])
	}
}

// A person sets the URL secret itself to a value of their own: an upgrade keeps it,
// where it would write the release's template, and its plan says so (#570's review,
// M1).
func TestAPersonsOwnURLSecretStaysThroughAnUpgrade(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, insideEntry(1, "1.0", firstRelease, ""))
	const own = "postgres://me:own@elsewhere:5432/mine"
	u.applied(propose(t, u.c, "Use my own database", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: u.env, Name: "URL"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: own}}},
	}))
	if got := insideEnv(t, u)["URL"]; got != own {
		t.Fatalf("after the edit web reads URL %q", got)
	}
	u.release(insideEntry(2, "2.0", secondRelease, ""))
	up := u.upgrade()
	if notes := notesOf(up, "upgrade_kept_own"); len(notes) != 1 || !strings.Contains(notes[0], "URL holds a value you set") {
		t.Errorf("the upgrade's plan says %q of the URL", notes)
	}
	u.applied(up)
	if got := insideEnv(t, u)["URL"]; got != own {
		t.Errorf("after an upgrade that changes only the image, web reads URL %q, want the person's %q", got, own)
	}
}

// A release that no longer puts the password inside its URL leaves no URL secret
// behind, with its old template naming the password (#570's review, L5).
func TestARetiredURLSecretGoesWithTheUpgrade(t *testing.T) {
	t.Parallel()
	u := startUpgrading(t, store.SQLite, insideEntry(1, "1.0", firstRelease, ""))
	held := func() bool {
		t.Helper()
		var ok bool
		if err := u.f.store.View(t.Context(), func(tx *store.Tx) error {
			r, err := tx.GetResource(t.Context(), "secret", u.env, "URL")
			ok = err == nil && r.Spec != nil
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !held() {
		t.Fatal("the install has no URL secret")
	}
	u.release(testEntry(2, "2.0", secondRelease, ""))
	u.applied(u.upgrade())
	if _, ok := insideEnv(t, u)["URL"]; ok {
		t.Error("web still reads URL after a release that drops it")
	}
	if held() {
		t.Error("the URL secret stays after a release that no longer writes it")
	}
}
