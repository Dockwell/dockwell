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

// How long each nft run may take.
const (
	loadWait = 15 * time.Second
	readWait = 10 * time.Second
)

var (
	// ErrNoNft is a server without nft, as Alpine and some minimal images ship.
	ErrNoNft = errors.New("this server has no nft, so Dockwell can't write its firewall. Install the nftables package")
	// ErrRefused is a kernel that won't load a table, as LXC and OpenVZ
	// containers can refuse, or an agent without root.
	ErrRefused = errors.New("this server won't let Dockwell load an nftables table")
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
}

// Drifted says whether the table now differs from what was read back after
// Dockwell loaded it: removed, flushed, or changed by something else.
func (l Loaded) Drifted(now Loaded) bool {
	return l.Present != now.Present || l.Fingerprint != now.Fingerprint
}

// Load replaces inet dockwell with t in one transaction, after nft has checked
// it without loading, and returns the table as nft lists it after.
func (f *Firewall) Load(ctx context.Context, t Table) (Loaded, error) {
	script, err := t.Script()
	if err != nil {
		return Loaded{}, err
	}
	if err := f.nftIn(ctx, "check the table", []byte(script), "-c", "-f", "-"); err != nil {
		return Loaded{}, err
	}
	if err := f.nftIn(ctx, "load the table", []byte(script), "-f", "-"); err != nil {
		return Loaded{}, err
	}
	return f.Read(ctx)
}

// Delete removes inet dockwell, whether it's there or not.
func (f *Firewall) Delete(ctx context.Context) error {
	return f.nftIn(ctx, "remove the table", []byte(DeleteScript()), "-f", "-")
}

// Read lists inet dockwell as it is now.
func (f *Firewall) Read(ctx context.Context) (Loaded, error) {
	res, err := f.Run.Run(ctx, Tool, readWait, nil, "-j", "list", "table", Family, Name)
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
	return f.nftIn(ctx, "check that a table can load", []byte(fmt.Sprintf("table %s %s {}\n", Family, Name)), "-c", "-f", "-")
}

// nftIn runs nft with script on its standard input, and says what failed in
// words a person can act on.
func (f *Firewall) nftIn(ctx context.Context, what string, script []byte, args ...string) error {
	res, err := f.Run.Run(ctx, Tool, loadWait, script, args...)
	if err == nil && res.Code != 0 {
		err = hosttool.Fail("nft", res)
	}
	if err != nil {
		return f.why(what, err, res)
	}
	return nil
}

// why says why nft couldn't do what, apart for a server without nft and one
// that won't let a table load, as each has its own words in the tab.
func (f *Firewall) why(what string, err error, res hosttool.Result) error {
	switch {
	case errors.Is(err, hosttool.ErrMissing):
		return ErrNoNft
	case refused(res.Errs):
		euid := os.Geteuid
		if f.euid != nil {
			euid = f.euid
		}
		if euid() != 0 {
			return fmt.Errorf("%w, as the agent doesn't run as root. Run it as root, as the install script does", ErrRefused)
		}
		return fmt.Errorf("%w: its kernel said %q, as an LXC or OpenVZ container can. Its host has to allow nftables in it", ErrRefused, hosttool.LastLines(res.Errs, 1))
	}
	return fmt.Errorf("can't %s: %w", what, err)
}

// refused is what nft says when the kernel won't take a table: not permitted,
// or no nf_tables in it.
func refused(errs string) bool {
	for _, s := range []string{"Operation not permitted", "Operation not supported", "Protocol not supported"} {
		if strings.Contains(errs, s) {
			return true
		}
	}
	return false
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
