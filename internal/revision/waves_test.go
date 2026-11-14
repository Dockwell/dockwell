// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"slices"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
)

// A rollout's waves are a canary of one, then waves of the batch, or of a quarter
// of the servers rounded up; fewer than two servers have none.
func TestWavesAreACanaryThenBatches(t *testing.T) {
	servers := []string{"a", "b", "c", "d", "e", "f", "g"}
	cases := []struct {
		canary bool
		batch  int
		want   [][]string
	}{
		{true, 0, [][]string{{"a"}, {"b", "c"}, {"d", "e"}, {"f", "g"}}},
		{false, 0, [][]string{{"a", "b"}, {"c", "d"}, {"e", "f"}, {"g"}}},
		{true, 3, [][]string{{"a"}, {"b", "c", "d"}, {"e", "f", "g"}}},
		{false, 10, [][]string{servers}},
	}
	for _, c := range cases {
		w := wavesOf(servers, c.canary, c.batch, time.Minute, true)
		if !slices.EqualFunc(w.Order, c.want, slices.Equal) || w.Pause != time.Minute || !w.KeepGoing {
			t.Errorf("canary %t, batch %d: %+v, want %v", c.canary, c.batch, w, c.want)
		}
	}
	if w := wavesOf([]string{"a"}, true, 0, 0, false); w != nil {
		t.Errorf("one server has the waves %+v", w)
	}
	w := wavesOf(servers, true, 0, 0, false)
	if w.waveOf("a") != 1 || w.waveOf("g") != 4 || w.waveOf("z") != 0 || (*Waves)(nil).waveOf("a") != 0 {
		t.Error("waveOf says the wrong wave")
	}
	if (*Waves)(nil).fingerprint() != "" || w.fingerprint() == wavesOf(servers, false, 0, 0, false).fingerprint() {
		t.Error("the fingerprint doesn't follow the waves")
	}
}

// A plan's waves are of the servers that take a step, in the rollout's order, so a
// canary is one that acts; the rest are said to need nothing.
func TestAPlansWavesAreOfTheServersThatAct(t *testing.T) {
	step := []resource.Step{{Ref: resource.Ref{Kind: "server.host", Name: "host"}}}
	plan := &Plan{Servers: []ServerPlan{{Server: "b", Steps: step}, {Server: "c", Steps: step}, {Server: "d", Steps: step}}}
	waves, idle := planWith{rollout: &Rollout{Servers: []string{"a", "b", "c", "d"}, Canary: true}}.wavesOf(plan)
	if !slices.EqualFunc(waves.Order, [][]string{{"b"}, {"c"}, {"d"}}, slices.Equal) || !slices.Equal(idle, []string{"a"}) {
		t.Errorf("the waves are %v, and %v need nothing", waves.Order, idle)
	}
	planned := &Waves{Order: [][]string{{"x"}}}
	if got, idle := (planWith{waves: planned}).wavesOf(plan); got != planned || idle != nil {
		t.Errorf("a change planned before keeps its waves: %v, %v", got, idle)
	}
}

// The next wave's pause counts from when the last step of any wave before it ended:
// one found done after a later one, as a resume finds it, counts too.
func TestTheWavesBeforeEndWithTheirLastStep(t *testing.T) {
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	w := &Waves{Order: [][]string{{"a"}, {"b", "c"}, {"d"}}}
	p := &Progress{Steps: []StepProgress{
		{Server: "a", Finished: at.Add(5 * time.Minute)},
		{Server: "b", Finished: at.Add(2 * time.Minute)},
		{Server: "c", Finished: at.Add(time.Minute)},
		{Server: "d", Finished: at.Add(9 * time.Minute)},
	}}
	if got := w.endedBefore(3, p); !got.Equal(at.Add(5 * time.Minute)) {
		t.Errorf("the waves before wave 3 ended at %s, want wave 1's later end", got)
	}
	if got := w.endedBefore(2, p); !got.Equal(at.Add(5 * time.Minute)) {
		t.Errorf("the wave before wave 2 ended at %s", got)
	}
	if !w.endedBefore(1, p).IsZero() || !w.endedBefore(2, nil).IsZero() {
		t.Error("there's a wave before wave 1")
	}
}
