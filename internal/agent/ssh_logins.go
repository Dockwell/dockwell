// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/lazyre"
	"github.com/dockwell/dockwell/internal/resource"
)

const (
	maxSSHLoginHours = 168
	maxSSHLoginRows  = 250
	maxSSHLogScan    = 3000
	maxSSHLogBytes   = 1 << 20
)

var (
	sshJournal  = hosttool.Tool{Name: "journalctl", Paths: []string{"/usr/bin/journalctl", "/bin/journalctl"}}
	sshLoginRE  = lazyre.Regexp(`^(Accepted|Failed) ([a-zA-Z0-9/_-]+) for (?:invalid user )?([^ ]+) from ([^ ]+) port ([0-9]+)(?: (.*)|$)`)
	sshSyslogRE = lazyre.Regexp(`^(.+?) [^ ]+ sshd(?:-session|-auth)?\[[0-9]+\]: (.+)$`)
)

func (r *sshReader) sshJournalLog(ctx context.Context, since time.Time, thisBoot bool) ([]sshJournalEntry, bool, error) {
	args := []string{
		"--quiet", "--no-pager", "--reverse", "--output=json", "--output-fields=MESSAGE,_UID",
		"--since=@" + strconv.FormatInt(since.Unix(), 10), "-n", strconv.Itoa(maxSSHLogScan),
	}
	if thisBoot {
		args = append(args, "-b")
	}
	args = append(args, "_UID=0", "SYSLOG_IDENTIFIER=sshd", "SYSLOG_IDENTIFIER=sshd-session", "SYSLOG_IDENTIFIER=sshd-auth")
	raw, err := r.sshTool(ctx, sshJournal, args...)
	if err != nil {
		return nil, false, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, false, nil
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte{'\n'})
	var entries []sshJournalEntry
	for _, line := range lines {
		var entry sshJournalEntry
		if json.Unmarshal(line, &entry) == nil && entry.UID == "0" {
			entries = append(entries, entry)
		}
	}
	return entries, len(lines) >= maxSSHLogScan, nil
}

func parseSSHLogin(at time.Time, message string) *dockwellv1.SshLogin {
	m := sshLoginRE().FindStringSubmatch(message)
	if len(m) == 0 {
		return nil
	}
	addr, err := netip.ParseAddr(m[4])
	if err != nil || addr.Zone() != "" || !safeSSHAccount(m[3]) {
		return nil
	}
	return &dockwellv1.SshLogin{Time: timestamppb.New(at), User: m[3], Address: addr.Unmap().String(), Method: m[2], Accepted: m[1] == "Accepted"}
}

func syslogSSHTime(text string, now time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, text); err == nil {
		return t
	}
	t, err := time.ParseInLocation(time.Stamp, text, now.Location())
	if err != nil {
		return time.Time{}
	}
	t = t.AddDate(now.Year(), 0, 0)
	if t.After(now.Add(time.Minute)) {
		t = t.AddDate(-1, 0, 0)
	}
	return t
}

func (r *sshReader) loginLog(ctx context.Context, req *dockwellv1.ListServerSshLoginsRequest) (*dockwellv1.ListServerSshLoginsResponse, error) {
	if r.tools.Off {
		return &dockwellv1.ListServerSshLoginsResponse{}, nil
	}
	hours := req.GetHours()
	if hours == 0 {
		hours = 24
	}
	if hours > maxSSHLoginHours {
		return nil, errors.New("choose an SSH login window of seven days or less")
	}
	now := time.Now()
	since := now.Add(-time.Duration(hours) * time.Hour)
	out := &dockwellv1.ListServerSshLoginsResponse{}
	counts := map[string]uint32{}
	take := func(item *dockwellv1.SshLogin) {
		if item == nil || item.GetTime().AsTime().Before(since) || item.GetTime().AsTime().After(now.Add(time.Minute)) || (req.GetAddress() != "" && item.GetAddress() != req.GetAddress()) || (req.GetUser() != "" && item.GetUser() != req.GetUser()) {
			return
		}
		if !item.GetAccepted() {
			counts[item.GetAddress()]++
		}
		if req.Accepted != nil && item.GetAccepted() != req.GetAccepted() {
			return
		}
		if len(out.Logins) < maxSSHLoginRows {
			out.Logins = append(out.Logins, item)
		} else {
			out.Bounded = true
		}
	}
	entries, bounded, err := r.sshJournalLog(ctx, since, false)
	if err == nil && len(entries) > 0 {
		out.Bounded = bounded
		for _, entry := range entries {
			take(parseSSHLogin(entry.when(), entry.Message))
		}
	} else {
		found := false
		for _, path := range []string{"/var/log/auth.log", "/var/log/secure", "/var/log/messages"} {
			data, clipped, err := sshLogTail(r.rootPath(path), r.tools.Root)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			found = true
			out.Unverified = true
			out.Bounded = clipped
			lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
			out.Bounded = out.Bounded || len(lines) > maxSSHLogScan
			for i := len(lines) - 1; i >= max(0, len(lines)-maxSSHLogScan); i-- {
				if parts := sshSyslogRE().FindStringSubmatch(string(lines[i])); len(parts) != 0 {
					take(parseSSHLogin(syslogSSHTime(parts[1], now), parts[2]))
				}
			}
			break
		}
		if !found && err != nil {
			return nil, errors.New("no readable SSH journal or authentication log. Check the server's logging service")
		}
	}
	for address, count := range counts {
		out.Failures = append(out.Failures, &dockwellv1.SshLoginFailures{Address: address, Count: count})
	}
	slices.SortFunc(out.Failures, func(a, b *dockwellv1.SshLoginFailures) int {
		if a.Count != b.Count {
			return int(b.Count) - int(a.Count)
		}
		return strings.Compare(a.Address, b.Address)
	})
	out.Failures = out.Failures[:min(len(out.Failures), maxSSHLoginRows)]
	return out, nil
}

func sshLogTail(path, root string) ([]byte, bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || !sshOwnership(info, root) || info.Mode().Perm()&0o022 != 0 {
		return nil, false, errors.New("the SSH authentication log isn't a regular file")
	}
	offset := max(0, info.Size()-maxSSHLogBytes)
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, false, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSSHLogBytes))
	if offset > 0 {
		_, data, _ = bytes.Cut(data, []byte{'\n'})
	}
	return data, offset > 0, err
}

func (r *sshReader) banSSH(ctx context.Context, f *fail2banReader, text string) (retErr error) {
	attempted := false
	defer func() {
		if retErr != nil && !attempted {
			retErr = resource.PlainlyBecause(resource.ReasonSSHRefused, retErr.Error(), retErr)
		}
	}()
	addr, err := netip.ParseAddr(text)
	if err != nil || addr.Zone() != "" || !addr.IsGlobalUnicast() || r.panelOverSSH {
		return errors.New("choose a failed remote SSH address on a server where the panel doesn't depend on SSH")
	}
	address := addr.Unmap().String()
	sessions, err := r.sessions(ctx, nil)
	if err != nil {
		return err
	}
	for i := range sessions {
		session := &sessions[i]
		if session.Peer.Addr().Unmap() == addr.Unmap() || session.Local.Addr().Unmap() == addr.Unmap() {
			return errors.New("this address belongs to a live SSH connection. It can't be banned here")
		}
	}
	failed := false
	log, err := r.loginLog(ctx, &dockwellv1.ListServerSshLoginsRequest{Address: address, Hours: maxSSHLoginHours, Accepted: &failed})
	if err != nil {
		return err
	}
	if len(log.Failures) == 0 || log.Unverified {
		return errors.New("this address has no verified recent failed logins in the SSH journal. Refresh the log")
	}
	if err := r.sshJailCanBan(ctx, f); err != nil {
		return err
	}
	attempted = true
	_, err = f.client(ctx, "set", "sshd", "banip", address)
	return err
}
