// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/jails"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/fail2ban"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

// A read of fail2ban asked after a jail is written says the jail as written: it
// starts again rather than share a read that began before the write, which
// would show the jail as it was until the Fail2ban card next asks, a minute
// later. A read asked with nothing written in between still shares the one
// under way.
func TestAReadAfterAJailIsWrittenSaysItAsWritten(t *testing.T) {
	s := newStandIns(t)
	if err := os.MkdirAll(filepath.Join(string(s), "etc", "fail2ban", "jail.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	// "status sshd" comes after the read has looked at Dockwell's jail file: while
	// hold is there, a read waits there, having seen the file as it was.
	s.tool(t, "/usr/bin/fail2ban-client", `d=$(dirname "$0")/../..
case "$*" in
version) echo 1.0.2 ;;
ping) echo 'Server replied: pong' ;;
status) printf 'Status\n|- Number of jail:\t1\n`+"`"+`- Jail list:\tsshd\n' ;;
"status sshd")
  echo x >> "$d/asked"
  while [ -e "$d/hold" ]; do sleep 0.02; done
  printf 'Status for the jail: sshd\n`+"`"+`- Actions\n   |- Currently banned:\t0\n' ;;
-t) echo "OK: configuration test is successful" ;;
reload) echo OK ;;
*) exit 255 ;;
esac
`)
	hold := filepath.Join(string(s), "hold")
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	asked := func() int {
		b, _ := os.ReadFile(filepath.Join(string(s), "asked"))
		return len(b) / 2
	}
	waitAsked := func(n int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for asked() < n {
			if time.Now().After(deadline) {
				t.Fatalf("fail2ban-client was asked status sshd %d times, want %d", asked(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	manager := &jails.Manager{
		Dir: t.TempDir(), Root: string(s), Tools: s.runner(),
		Control: func(context.Context) ([]string, error) { return []string{"203.0.113.10"}, nil },
	}
	cfg := Config{Tools: s.runner(), Logger: slog.New(slog.DiscardHandler), Drivers: resource.NewRegistry(&fail2ban.Driver{Backend: manager})}
	cfg.fail2ban = newFail2banReader(cfg)
	out := &sent{results: map[uint64]*agentv1.CallResult{}}
	c := newCalls(context.Background(), cfg, out.send, nil)
	read := func(id uint64) <-chan *agentv1.CallResult {
		done := make(chan *agentv1.CallResult, 1)
		go func() {
			done <- c.run(t.Context(), &agentv1.Call{Id: id, Call: &agentv1.Call_Fail2Ban{Fail2Ban: &agentv1.Fail2BanCall{}}})
		}()
		return done
	}
	ours := func(r *agentv1.CallResult) bool {
		t.Helper()
		js := r.GetFail2Ban().GetJails()
		if len(js) != 1 {
			t.Fatalf("the read says %v", r)
		}
		return js[0].GetDockwell()
	}

	// The read under way, as the shared reads hold it.
	running := func() *sharedRun[*dockwellv1.ServerFail2Ban] {
		cfg.fail2ban.reads.mu.Lock()
		defer cfg.fail2ban.reads.mu.Unlock()
		return cfg.fail2ban.reads.reading
	}
	// A read under way, and one asked beside it with nothing written between, share a run.
	before := read(1)
	waitAsked(1)
	first := running()
	beside := read(2)

	// The jail is written as a step applies, while that read still waits.
	spec := &resourcev1.Jail{Filter: "sshd", Backend: "systemd", MaxRetry: 3}
	if err := (&fail2ban.Driver{}).Validate(spec); err != nil {
		t.Fatal(err)
	}
	encoded, err := resource.Encode(spec)
	if err != nil {
		t.Fatal(err)
	}
	ref := resource.Ref{Kind: fail2ban.Kind, Scope: "srv_A", Name: "sshd"}
	step := resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}
	applied := c.run(t.Context(), &agentv1.Call{Id: 3, Timeout: durationpb.New(time.Minute), Call: &agentv1.Call_Apply{Apply: &agentv1.ApplyCall{
		Step: resourcepb.Step(step, ""), Desired: &agentv1.DesiredResource{Resource: resourcepb.Ref(ref), Spec: encoded},
	}}})
	if applied.GetError() != nil {
		t.Fatalf("applying the jail: %v", applied.GetError())
	}
	if b, err := os.ReadFile(filepath.Join(string(s), jails.File)); err != nil || !jails.IsDockwells(string(b)) {
		t.Fatalf("the jail file reads %q, %v", b, err)
	}

	// Asked now, the read starts a run of its own, which waits its turn at the
	// server's tools: it doesn't join the one from before.
	after := read(4)
	deadline := time.Now().Add(5 * time.Second)
	for running() == first {
		if time.Now().After(deadline) {
			t.Fatal("the read asked after the jail was written joined the one that began before it")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	if ours(<-before) {
		t.Error("the read that began before the write says the jail is Dockwell's")
	}
	if ours(<-beside) {
		t.Error("the read asked beside the first, with nothing written between, didn't share it")
	}
	if !ours(<-after) {
		t.Error("the read asked after the jail was written shows it as it was before")
	}
	if n := asked(); n != 2 {
		t.Errorf("fail2ban-client was asked status sshd %d times, want 2: one shared read before the write, one after", n)
	}
}
