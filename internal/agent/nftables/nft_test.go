// SPDX-License-Identifier: AGPL-3.0-only

package nftables

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/agent/hosttool"
)

// listing is inet dockwell as nft -j lists it, with handles to renumber.
const listing = `{"nftables": [{"metainfo": {"version": "1.1.3", "json_schema_version": 1}},
 {"table": {"family": "inet", "name": "dockwell", "handle": HANDLE}},
 {"chain": {"family": "inet", "table": "dockwell", "name": "input", "handle": 1, "type": "filter", "hook": "input", "prio": 0, "policy": "accept"}},
 {"rule": {"family": "inet", "table": "dockwell", "chain": "input", "handle": 2, "comment": "rule web", "expr": [{"accept": null}]}}]}`

// standIn writes a stand-in nft under a folder that a runner takes as its
// root, which records each call's arguments and input, one file each, and
// answers as the script given says.
func standIn(t *testing.T, script string) (*hosttool.Runner, string) {
	t.Helper()
	root := t.TempDir()
	calls := filepath.Join(root, "calls")
	if err := os.MkdirAll(filepath.Join(root, "usr", "sbin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(calls, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\n" +
		"n=$(ls " + calls + " | grep -c args)\n" +
		"echo \"$*\" > " + calls + "/$n.args\n" +
		"case \"$*\" in *-f*) cat > " + calls + "/$n.in ;; esac\n" +
		script
	if err := os.WriteFile(filepath.Join(root, "usr", "sbin", "nft"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return &hosttool.Runner{Root: root}, calls
}

func callsOf(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for i := 0; ; i++ {
		b, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(i)+".args"))
		if err != nil {
			return out
		}
		out = append(out, strings.TrimSpace(string(b)))
	}
}

func listingWith(handle string) string { return strings.Replace(listing, "HANDLE", handle, 1) }

// A load checks the table with nft first, loads it in one transaction from
// standard input, and reads it back, so drift can be told later.
func TestLoadChecksThenLoadsThenReadsBack(t *testing.T) {
	run, calls := standIn(t, `case "$*" in *list*) cat <<'EOF'
`+listingWith("7")+`
EOF
;; esac
`)
	table := Table{Allows: []Rule{{Name: "rule web", Protocol: "tcp", Ports: []uint16{443}}}}
	fw := &Firewall{Run: run}
	loaded, err := fw.Load(t.Context(), table)
	if err != nil {
		t.Fatal(err)
	}
	got := callsOf(t, calls)
	want := []string{"-c -f -", "-f -", "-j list table inet dockwell"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("nft ran %q, want %q", got, want)
	}
	script, _ := table.Script()
	for _, n := range []string{"0", "1"} {
		in, err := os.ReadFile(filepath.Join(calls, n+".in"))
		if err != nil || string(in) != script {
			t.Errorf("call %s read %q, want the table's script", n, in)
		}
	}
	if !loaded.Present || loaded.Fingerprint == "" {
		t.Errorf("loaded %+v", loaded)
	}
}

// A table nft finds wrong when it checks isn't loaded.
func TestLoadStopsWhenTheCheckFails(t *testing.T) {
	run, calls := standIn(t, `case "$*" in -c*) echo "Error: syntax error, unexpected newline" >&2; exit 1 ;; esac
`)
	_, err := (&Firewall{Run: run}).Load(t.Context(), Table{})
	if err == nil || !strings.Contains(err.Error(), "can't check the table") || !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("a failed check says %v", err)
	}
	if got := callsOf(t, calls); len(got) != 1 {
		t.Errorf("nft ran %q after its check failed", got)
	}
}

// A server without nft, and one whose kernel won't take a table, each say so
// in words of their own; a kernel that refuses an agent without root says to
// run it as root.
func TestNoNftAndAKernelThatRefuses(t *testing.T) {
	empty := &hosttool.Runner{Root: t.TempDir()}
	if err := (&Firewall{Run: empty}).Probe(t.Context()); !errors.Is(err, ErrNoNft) {
		t.Errorf("without nft: %v", err)
	}
	run, _ := standIn(t, `echo "Error: Could not process rule: Operation not permitted" >&2; exit 1
`)
	err := (&Firewall{Run: run, euid: func() int { return 0 }}).Probe(t.Context())
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "LXC or OpenVZ") {
		t.Errorf("a kernel that refuses: %v", err)
	}
	err = (&Firewall{Run: run, euid: func() int { return 1000 }}).Probe(t.Context())
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "doesn't run as root") {
		t.Errorf("an agent without root: %v", err)
	}
}

// A table that isn't there reads as absent, not as an error.
func TestReadSaysWhenThereIsNoTable(t *testing.T) {
	run, _ := standIn(t, `echo "Error: No such file or directory; did you mean table 'filter' in family inet?" >&2; exit 1
`)
	loaded, err := (&Firewall{Run: run}).Read(t.Context())
	if err != nil || loaded.Present {
		t.Errorf("no table reads as %+v, %v", loaded, err)
	}
}

// The same table, loaded again with new handles, hasn't drifted; one with a
// rule changed, or gone, has.
func TestDriftIgnoresHandles(t *testing.T) {
	a, err := fingerprint([]byte(listingWith("7")))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := fingerprint([]byte(listingWith("9")))
	changed, _ := fingerprint([]byte(strings.Replace(listingWith("7"), `"accept": null`, `"drop": null`, 1)))
	was := Loaded{Present: true, Fingerprint: a}
	if was.Drifted(Loaded{Present: true, Fingerprint: b}) {
		t.Error("new handles read as drift")
	}
	if !was.Drifted(Loaded{Present: true, Fingerprint: changed}) {
		t.Error("a changed rule doesn't read as drift")
	}
	if !was.Drifted(Loaded{}) {
		t.Error("a table gone doesn't read as drift")
	}
}

// A test with no stand-ins runs no nft at all.
func TestATestRunsNoNft(t *testing.T) {
	err := (&Firewall{Run: &hosttool.Runner{Off: true}}).Probe(context.Background())
	if !errors.Is(err, hosttool.ErrOff) {
		t.Errorf("an agent in a test ran nft: %v", err)
	}
}

// With DOCKWELL_TEST_NFT naming a real nft, as root, the table loads into the
// kernel, reads back the same, and goes. It changes the machine's firewall,
// so it runs only where CI says, in a container of its own.
func TestTheTableLoadsInRealNftables(t *testing.T) {
	nft := os.Getenv("DOCKWELL_TEST_NFT")
	if nft == "" {
		t.Skip("DOCKWELL_TEST_NFT names no nft")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "usr", "sbin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nft, filepath.Join(root, "usr", "sbin", "nft")); err != nil {
		t.Fatal(err)
	}
	fw := &Firewall{Run: &hosttool.Runner{Root: root}}
	table := Table{
		Pins:             []Rule{{Name: "pin ssh", Protocol: "tcp", Ports: []uint16{22}, Sources: prefixes(t, "203.0.113.7/32", "2001:db8::7/128")}},
		Allows:           []Rule{{Name: "rule web", Protocol: "tcp", Ports: []uint16{80, 443}}},
		Denies:           []Rule{{Name: "rule db", Protocol: "tcp", Ports: []uint16{5432}, Sources: prefixes(t, "0.0.0.0/0", "::/0")}},
		DenyRest:         true,
		DockerInterfaces: []string{"docker0"},
	}
	loaded, err := fw.Load(t.Context(), table)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fw.Delete(context.Background()) })
	again, err := fw.Load(t.Context(), table)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Drifted(again) {
		t.Error("the same table loaded twice reads as drift")
	}
	if err := fw.Delete(t.Context()); err != nil {
		t.Fatal(err)
	}
	if gone, err := fw.Read(t.Context()); err != nil || gone.Present {
		t.Errorf("after delete: %+v, %v", gone, err)
	}
}
