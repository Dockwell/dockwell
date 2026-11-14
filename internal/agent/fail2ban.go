// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/lazyre"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/agent/jails"
)

// fail2banWait bounds each fail2ban-client the agent runs, which starts Python
// and asks fail2ban's server over its socket.
const fail2banWait = 10 * time.Second

// fail2banReadWait bounds a whole read, a few commands for each jail.
const fail2banReadWait = time.Minute

// fail2banClient asks fail2ban's server what it does, at the paths the
// distributions and pip put it.
var fail2banClient = hosttool.Tool{Name: "fail2ban-client", Paths: []string{"/usr/bin/fail2ban-client", "/usr/local/bin/fail2ban-client"}}

// fail2banReader says what fail2ban does, through fail2ban-client, and
// changes nothing (ADR 0052).
type fail2banReader struct {
	tools *hosttool.Runner
	now   func() time.Time
	// loc is the time zone fail2ban says its times in: the server's own.
	loc *time.Location

	// specs are the jails Dockwell holds, and sessions the addresses signed in
	// over SSH now, when the agent can say.
	specs    func() map[string]*resourcev1.Jail
	sessions func(ctx context.Context) []string

	reads shared[*dockwellv1.ServerFail2Ban]
}

// newFail2banReader reads with the agent's tools, or, with none given, as in a
// test, runs none of the machine's.
func newFail2banReader(cfg Config) *fail2banReader {
	tools := cfg.Tools
	if tools == nil {
		tools = &hosttool.Runner{Off: true}
	}
	return &fail2banReader{tools: tools, loc: time.Local, specs: cfg.Jails, sessions: cfg.SSHSessions}
}

// read reads fail2ban as it is now. Two tabs asking at once share one read.
func (f *fail2banReader) read(ctx context.Context) (*dockwellv1.ServerFail2Ban, error) {
	if f == nil {
		return nil, errors.New("this agent can't read fail2ban")
	}
	return f.reads.read(ctx, fail2banReadWait, f.readNow)
}

// forget has the next read start again, as a jail was just written, which a
// read under way may have looked at before (ADR 0052).
func (f *fail2banReader) forget() {
	if f != nil {
		f.reads.forget()
	}
}

// client runs fail2ban-client, and returns what it printed, or why it failed.
func (f *fail2banReader) client(ctx context.Context, args ...string) (string, error) {
	res, err := f.tools.Run(ctx, fail2banClient, fail2banWait, nil, args...)
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return "", errors.New(hosttool.LastLines(res.Errs+"\n"+string(res.Out), 1))
	}
	return string(res.Out), nil
}

// readNow reads what's there. What it can't read, it says in what it returns.
func (f *fail2banReader) readNow(ctx context.Context) *dockwellv1.ServerFail2Ban {
	now := time.Now
	if f.now != nil {
		now = f.now
	}
	out := &dockwellv1.ServerFail2Ban{Time: timestamppb.New(now())}
	if _, ok := f.tools.Find(fail2banClient); !ok {
		out.State = dockwellv1.Fail2BanState_FAIL2BAN_STATE_NOT_INSTALLED
		return out
	}
	if v, err := f.client(ctx, "version"); err == nil {
		out.Version = strings.TrimPrefix(lastWord(v), "v")
	}
	// ping fails only as fail2ban's server doesn't answer on its socket.
	res, err := f.tools.Run(ctx, fail2banClient, fail2banWait, nil, "ping")
	switch {
	case err != nil:
		out.State, out.Why = dockwellv1.Fail2BanState_FAIL2BAN_STATE_UNREADABLE, fmt.Sprintf("fail2ban-client ping failed: %v", err)
		return out
	case res.Code != 0:
		out.State, out.Why = dockwellv1.Fail2BanState_FAIL2BAN_STATE_STOPPED, hosttool.LastLines(res.Errs+"\n"+string(res.Out), 1)
		return out
	}
	status, err := f.client(ctx, "status")
	if err != nil {
		out.State, out.Why = dockwellv1.Fail2BanState_FAIL2BAN_STATE_UNREADABLE, fmt.Sprintf("fail2ban-client status failed: %v", err)
		return out
	}
	out.State = dockwellv1.Fail2BanState_FAIL2BAN_STATE_RUNNING
	if f.sessions != nil {
		out.SignedInFrom = f.sessions(ctx)
	}
	ours := f.dockwellJails()
	var specs map[string]*resourcev1.Jail
	if f.specs != nil {
		specs = f.specs()
	}
	for _, name := range jailList(status) {
		if !isJailArg(name) {
			// Never an argument: a name that starts with a dash would read as one of
			// fail2ban-client's options.
			out.Jails = append(out.Jails, &dockwellv1.Fail2BanJail{Name: name, Unreadable: "Dockwell reads only jails named with letters, digits, . _ and -."})
			continue
		}
		j := f.jail(ctx, name)
		if j.Dockwell = ours[name]; j.Dockwell {
			j.Spec = specs[name]
		}
		out.Jails = append(out.Jails, j)
	}
	return out
}

// jail reads one jail: what it counts, how long it bans, and whom it bans now.
func (f *fail2banReader) jail(ctx context.Context, name string) *dockwellv1.Fail2BanJail {
	j := &dockwellv1.Fail2BanJail{Name: name}
	var why []string
	status, err := f.client(ctx, "status", name)
	if err != nil {
		why = append(why, fmt.Sprintf("fail2ban-client status %s failed: %v", name, err))
	}
	fields := statusFields(status)
	j.FailedNow, j.FailedTotal = count(fields["Currently failed"]), count(fields["Total failed"])
	j.BannedNow, j.BannedTotal = count(fields["Currently banned"]), count(fields["Total banned"])
	j.Files = strings.Fields(fields["File list"])
	if m := strings.TrimSpace(fields["Journal matches"]); m != "" {
		j.JournalMatches = []string{m}
	}
	if v, err := f.client(ctx, "get", name, "maxretry"); err == nil {
		j.MaxRetry = count(v)
	}
	if v, err := f.client(ctx, "get", name, "findtime"); err == nil {
		if s, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && s > 0 {
			j.FindTime = durationpb.New(time.Duration(s) * time.Second)
		}
	}
	if v, err := f.client(ctx, "get", name, "bantime"); err == nil {
		if s, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			if s < 0 {
				j.BansForGood = true
			} else {
				j.BanTime = durationpb.New(time.Duration(s) * time.Second)
			}
		}
	}
	// What it never bans, as it runs: a section's own ignoreip replaces what
	// [DEFAULT] sets, so only fail2ban can say what holds.
	if v, err := f.client(ctx, "get", name, "ignoreip"); err == nil {
		j.Ignore = ignored(v)
	}
	// --with-time, from fail2ban 0.11, says when each ban began and ends;
	// before it, the status's list is all there is.
	if v, err := f.client(ctx, "get", name, "banip", "--with-time"); err == nil {
		j.Bans = f.timedBans(v)
	}
	if len(j.Bans) == 0 {
		for _, ip := range strings.Fields(fields["Banned IP list"]) {
			j.Bans = append(j.Bans, &dockwellv1.Fail2BanBan{Address: ip})
		}
	}
	if len(j.Bans) > maxBans {
		j.Bans = j.Bans[:maxBans]
	}
	j.Unreadable = strings.Join(why, " ")
	return j
}

// ignoredName is a host name in what a jail ignores: no spaces, and a dot, so
// the line that heads the list isn't one.
var ignoredName = lazyre.Regexp(`^[A-Za-z0-9]([A-Za-z0-9-]*\.)+[A-Za-z0-9-]*[A-Za-z]$`)

// ignored reads fail2ban-client get <jail> ignoreip, a tree of addresses and
// ranges under a line that says so, or one line that says none is ignored.
func ignored(text string) []string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		entry := strings.TrimSpace(strings.TrimLeft(line, "|`- \t"))
		if _, err := netip.ParsePrefix(entry); err == nil {
			out = append(out, entry)
		} else if _, err := netip.ParseAddr(entry); err == nil {
			out = append(out, entry)
		} else if ignoredName().MatchString(entry) && len(entry) <= 253 && !strings.HasSuffix(line, ":") {
			// A host name, which fail2ban looks up itself: an admin's own list may hold one.
			out = append(out, entry)
		}
	}
	return out
}

// jailArg is a jail's name the agent passes to fail2ban-client.
// Its length is checked apart from its form, as a counted repetition compiles
// to as many copies of what it repeats, which every agent pays for as it
// starts (ADR 0060).
var jailArg = lazyre.Regexp(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// maxJailArg is how long a jail's name gets.
const maxJailArg = 128

// isJailArg reports a jail's name the agent passes to fail2ban-client.
func isJailArg(name string) bool { return len(name) <= maxJailArg && jailArg().MatchString(name) }

// maxBans is how many of a jail's bans a read carries, the latest first. Its
// count of those banned now is fail2ban's own, and says them all.
const maxBans = 500

// jailList reads fail2ban-client status's "Jail list:", its jails apart by
// commas.
func jailList(status string) []string {
	var out []string
	for name := range strings.SplitSeq(statusFields(status)["Jail list"], ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// statusLine is a line of fail2ban-client's status, drawn as a tree:
// "|  |- Currently failed:\t1".
var statusLine = lazyre.Regexp("^[\\s|`-]*([A-Za-z][A-Za-z ]*?):\\s*(.*)$")

// statusFields reads fail2ban-client's status lines into what each says.
func statusFields(status string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(status, "\n") {
		if m := statusLine().FindStringSubmatch(line); m != nil {
			out[m[1]] = strings.TrimSpace(m[2])
		}
	}
	return out
}

// timedBan is a ban as --with-time says it, in the server's time zone:
// "198.51.100.77 \t2026-11-08 03:10:00 + 600 = 2026-11-08 03:20:00".
var timedBan = lazyre.Regexp(`(\S+)\s+(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) \+ (-?\d+) = (\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)`)

// timedBans reads the bans --with-time says, the latest first.
func (f *fail2banReader) timedBans(text string) []*dockwellv1.Fail2BanBan {
	loc := f.loc
	if loc == nil {
		loc = time.Local
	}
	var out []*dockwellv1.Fail2BanBan
	for _, m := range timedBan().FindAllStringSubmatch(text, -1) {
		b := &dockwellv1.Fail2BanBan{Address: m[1]}
		if since, err := time.ParseInLocation(time.DateTime, m[2], loc); err == nil {
			b.Since = timestamppb.New(since)
		}
		if secs, _ := strconv.ParseInt(m[3], 10, 64); secs >= 0 {
			if until, err := time.ParseInLocation(time.DateTime, m[4], loc); err == nil {
				b.Until = timestamppb.New(until)
			}
		}
		out = append(out, b)
	}
	slices.SortStableFunc(out, func(a, b *dockwellv1.Fail2BanBan) int {
		return b.GetSince().AsTime().Compare(a.GetSince().AsTime())
	})
	return out
}

func count(s string) uint32 {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	if err != nil {
		return 0
	}
	return uint32(n)
}

func lastWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

// dockwellJails are the jails Dockwell writes, by the sections of its own file.
func (f *fail2banReader) dockwellJails() map[string]bool {
	root := f.tools.Root
	if root == "" {
		root = "/"
	}
	out := map[string]bool{}
	b, err := os.ReadFile(filepath.Join(root, jails.File)) //nolint:gosec // fail2ban's own folder
	// A file there that Dockwell didn't write is someone else's jails, not its own.
	if err != nil || !jails.IsDockwells(string(b)) {
		return out
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			out[strings.Trim(line, "[]")] = true
		}
	}
	return out
}
