// SPDX-License-Identifier: AGPL-3.0-only

package nftables

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/agent/hosttool"
)

// Tool is nft, at the paths the distributions put it.
var Tool = hosttool.Tool{Name: "nft", Paths: []string{"/usr/sbin/nft", "/sbin/nft"}}

// How long each nft run may take. Variables, for a test.
var (
	loadWait = 15 * time.Second
	readWait = 10 * time.Second
)

var (
	// ErrNoNft is a server without nft, as Alpine and some minimal images ship.
	ErrNoNft = errors.New("this server has no nft, so Dockwell can't write its firewall. Install the nftables package")
	// ErrRefused is a kernel that won't load a table, as LXC and OpenVZ
	// containers can refuse, one without nftables, or an agent without root.
	ErrRefused = errors.New("this server won't let Dockwell load an nftables table")
	// ErrUnknown is a load that went into the kernel and then couldn't be seen
	// through: nft was stopped, or what it loaded couldn't be read back. The
	// table may be live, so the caller puts back the table it last confirmed,
	// rather than take it that nothing changed.
	ErrUnknown = errors.New("the table may have loaded, and Dockwell can't tell")
)

// Firewall writes and reads inet dockwell with nft.
type Firewall struct {
	Run *hosttool.Runner
	// euid is the agent's user, for a test. Nil asks the kernel.
	euid func() int
}

// Loaded is inet dockwell as nft lists it, read back right after a load or
// later, so the two can be compared.
type Loaded struct {
	// Present is false when there's no inet dockwell.
	Present bool
	// Fingerprint is a hash of the table's chains and rules as nft lists them,
	// without the handles a reload renumbers.
	Fingerprint string
	// Skipped are Docker's interfaces the load left out, as nft can't match
	// their names; SkippedWords says what that means.
	Skipped []string
}

// Drifted says whether the table now differs from what was read back after
// Dockwell loaded it: removed, flushed, or changed by something else.
func (l Loaded) Drifted(now Loaded) bool {
	return l.Present != now.Present || l.Fingerprint != now.Fingerprint
}

// Load replaces inet dockwell with t in one transaction, after nft has checked
// it without loading, and returns the table as nft lists it after. No other
// tool runs between the three.
//
// An error before the load says nothing changed. Once nft has the table, the
// load and the read back run to their end whether the caller stays or not, and
// what goes wrong from then on is ErrUnknown: nft's transaction either went in
// whole or not at all, and Dockwell can't tell which. nft refusing the table
// itself is the one failure there that changed nothing.
func (f *Firewall) Load(ctx context.Context, t Table) (Loaded, error) {
	script, err := t.Script()
	if err != nil {
		return Loaded{}, err
	}
	turn, err := f.Run.Turn(ctx)
	if err != nil {
		return Loaded{}, err
	}
	defer turn.Done()
	if err := f.nftIn(ctx, turn, "check the table", []byte(script), "-c", "-f", "-"); err != nil {
		return Loaded{}, err
	}
	// From here the kernel may take the table, so the caller leaving doesn't
	// cut short what tells whether it did.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loadWait+readWait)
	defer cancel()
	res, err := turn.Run(ctx, Tool, loadWait, []byte(script), "-f", "-")
	switch {
	case err != nil:
		return Loaded{}, fmt.Errorf("%w: nft was loading it: %w", ErrUnknown, err)
	case res.Code != 0:
		return Loaded{}, fmt.Errorf("nft refused the table, so nothing changed: %w", f.why("load the table", hosttool.Fail("nft", res), res))
	}
	loaded, err := f.read(ctx, turn)
	switch {
	case err != nil:
		return Loaded{}, fmt.Errorf("%w: nft loaded it, then Dockwell couldn't read it back: %w", ErrUnknown, err)
	case !loaded.Present:
		return Loaded{}, fmt.Errorf("%w: nft loaded it, then the kernel listed no table %s %s", ErrUnknown, Family, Name)
	}
	loaded.Skipped = t.Skipped()
	return loaded, nil
}

// Delete removes inet dockwell, whether it's there or not. Once nft has the
// transaction, it runs to its end whether the caller stays or not.
func (f *Firewall) Delete(ctx context.Context) error {
	turn, err := f.Run.Turn(ctx)
	if err != nil {
		return err
	}
	defer turn.Done()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loadWait)
	defer cancel()
	res, err := turn.Run(ctx, Tool, loadWait, []byte(DeleteScript()), "-f", "-")
	switch {
	case err != nil:
		return fmt.Errorf("%w: nft was removing it: %w", ErrUnknown, err)
	case res.Code != 0:
		return fmt.Errorf("nft refused to remove the table, so nothing changed: %w", f.why("remove the table", hosttool.Fail("nft", res), res))
	}
	return nil
}

// Read lists inet dockwell as it is now.
func (f *Firewall) Read(ctx context.Context) (Loaded, error) {
	turn, err := f.Run.Turn(ctx)
	if err != nil {
		return Loaded{}, err
	}
	defer turn.Done()
	return f.read(ctx, turn)
}

func (f *Firewall) read(ctx context.Context, turn *hosttool.Turn) (Loaded, error) {
	res, err := turn.Run(ctx, Tool, readWait, nil, "-j", "list", "table", Family, Name)
	if err != nil {
		return Loaded{}, f.why("read the table", err, res)
	}
	if res.Code != 0 {
		// nft says "No such file or directory" for a table that isn't there.
		if strings.Contains(res.Errs, "No such file or directory") {
			return Loaded{}, nil
		}
		return Loaded{}, f.why("read the table", hosttool.Fail("nft", res), res)
	}
	fp, err := fingerprint(res.Out)
	if err != nil {
		return Loaded{}, fmt.Errorf("nft listed the table in a way Dockwell can't read: %w", err)
	}
	return Loaded{Present: true, Fingerprint: fp}, nil
}

// Probe says whether this server can hold Dockwell's table: it has nft, and
// its kernel lets the agent load a table, which nft checks without loading.
func (f *Firewall) Probe(ctx context.Context) error {
	turn, err := f.Run.Turn(ctx)
	if err != nil {
		return err
	}
	defer turn.Done()
	return f.nftIn(ctx, turn, "check that a table can load", []byte(fmt.Sprintf("table %s %s {}\n", Family, Name)), "-c", "-f", "-")
}

// nftIn checks a script with nft, which changes nothing, and says what failed in
// words a person can act on.
func (f *Firewall) nftIn(ctx context.Context, turn *hosttool.Turn, what string, script []byte, args ...string) error {
	res, err := turn.Run(ctx, Tool, loadWait, script, args...)
	if err == nil && res.Code != 0 {
		err = hosttool.Fail("nft", res)
	}
	if err != nil {
		return fmt.Errorf("%w, so nothing changed", f.why(what, err, res))
	}
	return nil
}

// why says why nft couldn't do what, apart for a server without nft and one
// that won't let a table load, as each has its own words in the tab.
func (f *Firewall) why(what string, err error, res hosttool.Result) error {
	switch {
	case errors.Is(err, hosttool.ErrMissing):
		return ErrNoNft
	case strings.Contains(res.Errs, "Operation not permitted"):
		euid := os.Geteuid
		if f.euid != nil {
			euid = f.euid
		}
		if euid() != 0 {
			return fmt.Errorf("%w, as the agent doesn't run as root. Run it as root, as the install script does", ErrRefused)
		}
		return fmt.Errorf("%w: its kernel said %q, as it says inside an LXC or OpenVZ container. The container's host has to allow nftables in it", ErrRefused, hosttool.LastLines(res.Errs, 1))
	case strings.Contains(res.Errs, "Operation not supported") || strings.Contains(res.Errs, "Protocol not supported"):
		return fmt.Errorf("%w: its kernel said %q, as it says without nftables (the nf_tables module) or inside a container that doesn't offer it. Load nf_tables with modprobe nf_tables, or ask the container's host", ErrRefused, hosttool.LastLines(res.Errs, 1))
	}
	return fmt.Errorf("can't %s: %w", what, err)
}

// fingerprint hashes nft's JSON listing without its metainfo and handles, so
// a table loaded again the same reads the same.
func fingerprint(out []byte) (string, error) {
	var doc struct {
		Nftables []map[string]any `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return "", err
	}
	var kept []any
	for _, item := range doc.Nftables {
		if _, ok := item["metainfo"]; ok {
			continue
		}
		kept = append(kept, dropHandles(item))
	}
	// encoding/json writes a map's keys in order, so the same table hashes the same.
	b, err := json.Marshal(kept)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func dropHandles(v any) any {
	switch v := v.(type) {
	case map[string]any:
		delete(v, "handle")
		for k, inner := range v {
			v[k] = dropHandles(inner)
		}
		return v
	case []any:
		for i, inner := range v {
			v[i] = dropHandles(inner)
		}
		return v
	default:
		return v
	}
}
