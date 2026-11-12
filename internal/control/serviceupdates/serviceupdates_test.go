// SPDX-License-Identifier: AGPL-3.0-only

package serviceupdates

import (
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/store"
)

const (
	dOld = "sha256:0100000000000000000000000000000000000000000000000000000000000000"
	dNew = "sha256:0200000000000000000000000000000000000000000000000000000000000000"
)

func TestNotesOf(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/docker-library/postgres":   "https://github.com/docker-library/postgres/releases",
		"https://github.com/goauthentik/authentik.git": "https://github.com/goauthentik/authentik/releases",
		"https://github.com/acme/app/tree/main/docker": "https://github.com/acme/app/releases",
		"https://gitlab.com/group/sub/app":             "https://gitlab.com/group/sub/app/-/releases",
		"https://codeberg.org/forgejo/forgejo":         "https://codeberg.org/forgejo/forgejo",
		"http://github.com/acme/app":                   "",
		"git@github.com:acme/app.git":                  "",
		"https://user:pw@github.com/acme/app":          "",
		"":                                             "",
	} {
		if got := NotesOf(in); got != want {
			t.Errorf("NotesOf(%q) = %q, want %q", in, got, want)
		}
	}
	if got := compareURL("https://github.com/acme/web.git", "aaa", "bbb"); got != "https://github.com/acme/web/compare/aaa...bbb" {
		t.Errorf("compareURL = %q", got)
	}
	if got := compareURL("https://git.example.com/acme/web.git", "aaa", "bbb"); got != "" {
		t.Errorf("compareURL off GitHub = %q", got)
	}
}

func TestRetag(t *testing.T) {
	for _, c := range [][3]string{
		{"postgres:16.3", "16.4", "postgres:16.4"},
		{"ghcr.io/acme/web", "2", "ghcr.io/acme/web:2"},
		{"localhost:5000/web:1.0@" + dOld, "1.1", "localhost:5000/web:1.1"},
		{"docker.io/library/nginx:1.27-alpine", "1.28-alpine", "docker.io/library/nginx:1.28-alpine"},
	} {
		if got := retag(c[0], c[1]); got != c[2] {
			t.Errorf("retag(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

// An image pinned by digest is compared by its pin when what runs isn't known,
// and its new build is pinned to the new digest.
func TestOffersOfAPinnedImage(t *testing.T) {
	res := &agentv1.ImageUpdatesResult{Answer: dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND, Digest: dNew, Newer: []*agentv1.NewerTag{
		{Name: "2.0", Kind: dockwellv1.UpdateKind_UPDATE_KIND_MAJOR}, {Name: "1.1", Kind: dockwellv1.UpdateKind_UPDATE_KIND_MINOR},
	}}
	got := Offers("acme/web:1.0@"+dOld, res, "")
	if len(got) != 3 {
		t.Fatalf("offers = %v", got)
	}
	if got[0].GetKind() != dockwellv1.UpdateKind_UPDATE_KIND_DIGEST || got[0].GetToImage() != "acme/web:1.0@"+dNew || got[0].GetFromDigest() != dOld {
		t.Errorf("the new build = %v", got[0])
	}
	if got[1].GetToVersion() != "1.1" || got[2].GetToVersion() != "2.0" {
		t.Errorf("smallest step first: %v, %v", got[1], got[2])
	}
	// What runs is what its tag points at: nothing but the newer tags.
	if got := Offers("acme/web:1.0", &agentv1.ImageUpdatesResult{Digest: dOld}, dOld); len(got) != 0 {
		t.Errorf("up to date offers %v", got)
	}
}

// A catalog install's step reads from its entry's versions; the same version is
// a release.
func TestCatalogUpdate(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		from, to string
		want     dockwellv1.UpdateKind
	}{
		{"17.11", "17.12", dockwellv1.UpdateKind_UPDATE_KIND_PATCH},
		{"17.12", "18.1", dockwellv1.UpdateKind_UPDATE_KIND_MAJOR},
		{"11.4.4", "11.8.2", dockwellv1.UpdateKind_UPDATE_KIND_MINOR},
		{"11.4.4", "11.4.4", dockwellv1.UpdateKind_UPDATE_KIND_RELEASE},
	} {
		u := catalogUpdate(CatalogInstall{FromVersion: c.from, ToVersion: c.to, ToRelease: 7}, now)
		if len(u.GetAvailable()) != 1 || u.GetAvailable()[0].GetKind() != c.want || u.GetAvailable()[0].GetCatalogRelease() != 7 {
			t.Errorf("%s to %s = %v, want %v", c.from, c.to, u.GetAvailable(), c.want)
		}
	}
	if u := catalogUpdate(CatalogInstall{FromVersion: "17.12"}, now); len(u.GetAvailable()) != 0 || u.GetSource() != dockwellv1.UpdateSource_UPDATE_SOURCE_CATALOG {
		t.Errorf("an install that's up to date = %v", u)
	}
}

// What a person skipped is marked, and sets the state by what's left; a snooze
// that ended says nothing.
func TestApplyPerson(t *testing.T) {
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	u := &dockwellv1.ServiceUpdate{Check: dockwellv1.UpdateCheck_UPDATE_CHECK_AVAILABLE, Available: []*dockwellv1.AvailableUpdate{
		{Kind: dockwellv1.UpdateKind_UPDATE_KIND_MAJOR, ToVersion: "17.2"},
		{Kind: dockwellv1.UpdateKind_UPDATE_KIND_DIGEST, ToVersion: "16.3", ToDigest: dNew},
	}}
	applyPerson(u, store.ServiceUpdate{Skipped: `["17.2","` + dNew + `"]`, SnoozeUntil: now.Add(-time.Hour)}, now)
	if u.GetCheck() != dockwellv1.UpdateCheck_UPDATE_CHECK_UP_TO_DATE || u.GetSnoozeTime() != nil {
		t.Errorf("with both skipped: %v", u)
	}
	if u.GetAvailable()[0].GetKind() != dockwellv1.UpdateKind_UPDATE_KIND_DIGEST {
		t.Errorf("not smallest step first: %v", u.GetAvailable())
	}
	if Counts(u, now) {
		t.Error("skipped updates count")
	}
}

// A registry that limits how often it's asked is left alone an hour, then two,
// doubling to a day; one that answers is asked as usual again.
func TestHeardHoldsOff(t *testing.T) {
	k := &Keeper{}
	now := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	for _, want := range []time.Duration{time.Hour, 2 * time.Hour, 4 * time.Hour} {
		k.heard("srv_1", "docker.io", dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_ASKED, now)
		h, held := k.heldOff("srv_1", "docker.io", now)
		if !held || h.until.Sub(now) != want || h.why != dockwellv1.UpdateProblem_UPDATE_PROBLEM_RATE_LIMITED {
			t.Errorf("held %v until %v, want %v", held, h.until.Sub(now), want)
		}
	}
	for range 10 {
		k.heard("srv_1", "docker.io", dockwellv1.ImageAnswer_IMAGE_ANSWER_NOT_ASKED, now)
	}
	if h, _ := k.heldOff("srv_1", "docker.io", now); h.until.Sub(now) != limitedMost {
		t.Errorf("held %v, want a day at most", h.until.Sub(now))
	}
	if _, held := k.heldOff("srv_2", "docker.io", now); held {
		t.Error("another server is held off too")
	}
	k.heard("srv_1", "docker.io", dockwellv1.ImageAnswer_IMAGE_ANSWER_FOUND, now)
	if _, held := k.heldOff("srv_1", "docker.io", now); held {
		t.Error("a registry that answered is still held off")
	}
}

func TestJittered(t *testing.T) {
	for range 1000 {
		if d := jittered(Every); d < Every-Every/10 || d > Every+Every/10 {
			t.Fatalf("jittered(%v) = %v", Every, d)
		}
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"1d": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "12h": 12 * time.Hour} {
		if got, err := parseDuration(in); err != nil || got != want {
			t.Errorf("parseDuration(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := parseDuration("soon"); err == nil {
		t.Error("parseDuration(soon) took it")
	}
}
