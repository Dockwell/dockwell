// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/agent/access"
	"github.com/dockwell/dockwell/internal/agent/hosttool"
	"github.com/dockwell/dockwell/internal/resource/sshconfig"
)

const (
	sshDropInPath = "/etc/ssh/sshd_config.d/" + dockwellDropIn
	sshHeader     = "# Managed by Dockwell\n"
)

var sshSystemdRun = hosttool.Tool{Name: "systemd-run", Paths: []string{"/usr/bin/systemd-run", "/bin/systemd-run"}}

type sshSettings struct{ reader *sshReader }

// SetUpSSH gives the access timer the same SSH backend used by the agent's SSH
// reads. It has to be attached before the firewall recovers any waiting change.
func SetUpSSH(manager *access.Manager, tools *hosttool.Runner) {
	r := newSSHReader(Config{Tools: tools})
	r.panelOverSSH = manager.PanelOverSSH
	manager.SSH = &sshSettings{reader: r}
}

func (r *sshReader) sshd(ctx context.Context, args ...string) ([]byte, error) {
	return r.sshTool(ctx, sshTools["sshd"], args...)
}

func (r *sshReader) sshTool(ctx context.Context, tool hosttool.Tool, args ...string) ([]byte, error) {
	res, err := r.tools.Run(ctx, tool, sshWait, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", tool.Name, err)
	}
	if res.Code != 0 {
		return nil, hosttool.Fail(tool.Name, res)
	}
	return res.Out, nil
}

func sshValue(text, key string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if val, ok := strings.CutPrefix(line, key+" "); ok {
			return strings.TrimSpace(val)
		}
	}
	return ""
}

func (s *sshSettings) editUnit(ctx context.Context) (string, error) {
	r := s.reader
	if r.tools.Off {
		return "", errors.New("SSH editing requires a local OpenSSH service")
	}
	if _, ok := r.tools.Find(sshSystemdRun); !ok {
		return "", errors.New("SSH editing requires systemd to arm an independent rollback")
	}
	found, err := sshDaemons(r.proc())
	if err != nil {
		return "", err
	}
	if len(found) != 1 {
		return "", errors.New("SSH editing needs one running OpenSSH listener")
	}
	d := found[0]
	var unit string
	switch reloadCommand(r.proc(), d.pid, "") {
	case "systemctl reload ssh.service":
		unit = "ssh.service"
	case "systemctl reload sshd.service":
		unit = "sshd.service"
	default:
		return "", errors.New("the SSH listener uses a custom service. Use the server's SSH configuration")
	}
	if name, readable := overridingFlag(r.proc(), d.pid); !readable || name != "" {
		if !readable {
			return "", errors.New("can't read the SSH listener's command line. Check the agent's access to /proc")
		}
		return "", fmt.Errorf("the SSH listener uses %s. Remove the command-line override before taking on this setting", name)
	}
	exe := listenerExe(r.proc(), d.pid)
	asked, _ := r.tools.Find(sshTools["sshd"])
	if r.tools.Root != "" {
		asked = strings.TrimPrefix(asked, r.tools.Root)
	}
	if exe == "" || (exe != asked && exe != resolved(r.root, asked)) {
		return "", errors.New("the running SSH listener uses a different executable. Check the SSH service before changing its settings")
	}
	out, err := r.sshTool(ctx, sshTools["systemctl"], "show", "--property=CanReload", "--property=MainPID", unit)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if !slices.Contains(lines, "CanReload=yes") || !slices.Contains(lines, "MainPID="+d.pid) {
		return "", errors.New("the SSH service can't reload the running listener. Check its systemd service")
	}
	return unit, nil
}

func sshFile(spec *resourcev1.SshConfig, also uint16) string {
	if spec == nil {
		return ""
	}
	out := strings.Builder{}
	out.WriteString(sshHeader)
	if spec.PasswordLogin != nil {
		fmt.Fprintf(&out, "PasswordAuthentication %s\n", sshYesNo(spec.GetPasswordLogin()))
		if !spec.GetPasswordLogin() {
			out.WriteString("ChallengeResponseAuthentication no\n")
		}
	}
	if spec.KeyLogin != nil {
		fmt.Fprintf(&out, "PubkeyAuthentication %s\n", sshYesNo(spec.GetKeyLogin()))
	}
	if spec.GetRootLogin() != "" {
		fmt.Fprintf(&out, "PermitRootLogin %s\n", spec.GetRootLogin())
	}
	if spec.GetPort() != 0 {
		fmt.Fprintf(&out, "Port %d\n", spec.GetPort())
	}
	if also != 0 && uint32(also) != spec.GetPort() {
		fmt.Fprintf(&out, "Port %d\n", also)
	}
	for _, addr := range spec.GetListenAddresses() {
		fmt.Fprintf(&out, "ListenAddress %s\n", addr)
	}
	return out.String()
}

func sshYesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func (s *sshSettings) Plan(ctx context.Context, before, next *resourcev1.SshConfig) (*access.SSHChange, error) {
	r := s.reader
	if next != nil {
		if err := (&sshconfig.Driver{}).Validate(next); err != nil {
			return nil, err
		}
	}
	account := next.GetAccount()
	if next == nil {
		account = before.GetAccount()
	}
	accounts, err := r.accounts()
	if err != nil {
		return nil, err
	}
	admin, ok := accountNamed(accounts, account)
	if !ok || !admin.admin {
		return nil, errors.New("choose root or a local administrator in sudo or wheel to test SSH access")
	}
	unit, err := s.editUnit(ctx)
	if err != nil {
		return nil, err
	}
	files := readSSHDConfig(r.root, r.changed)
	if files.dropIns == "" {
		return nil, errors.New("sshd_config doesn't include 00-dockwell.conf. Include /etc/ssh/sshd_config.d/*.conf before its settings")
	}
	old, err := s.readDropIn()
	if err != nil {
		return nil, err
	}
	if old != sshFile(before, 0) {
		return nil, errors.New("the Dockwell SSH drop-in has changed on disk. Restore its last applied contents before editing SSH here")
	}
	raw, err := r.sshd(ctx, "-T")
	if err != nil {
		return nil, err
	}
	var effectiveNow dockwellv1.ServerSsh
	effective(string(raw), &effectiveNow)
	if len(effectiveNow.Ports) != 1 {
		return nil, errors.New("SSH uses multiple ports. Leave one port before managing SSH settings here")
	}
	oldPort := uint16(effectiveNow.Ports[0]) //nolint:gosec // effective parses a 16-bit port
	port := oldPort
	if next.GetPort() != 0 {
		port = uint16(next.GetPort()) //nolint:gosec // Validate limits the port to 65535
	} else if before.GetPort() != 0 {
		port = 22
	}
	addressesChanged := !slices.Equal(before.GetListenAddresses(), next.GetListenAddresses())
	if (port != oldPort || addressesChanged) && r.socketUnit(ctx) != "" {
		return nil, errors.New("a systemd socket owns the SSH listener. Use its socket configuration to change the port or addresses")
	}
	for _, source := range files.beforeDropIn {
		if sshSetsField(next, source.Keyword) && source.Keyword != "Port" && source.Keyword != "ListenAddress" {
			return nil, fmt.Errorf("%s in %s at line %d is read first. Move it below the drop-in or remove it before taking on this setting", source.Keyword, source.File, source.Line)
		}
	}
	for _, source := range files.setIn {
		if source.File == sshDropInPath {
			continue
		}
		if (source.Keyword == "Port" && port != oldPort) || (source.Keyword == "ListenAddress" && (port != oldPort || addressesChanged)) {
			return nil, fmt.Errorf("%s in %s sets an additional listener. Remove that line before taking on its setting", source.Keyword, source.File)
		}
	}
	for _, match := range files.writeMatches {
		for _, field := range match.Sets {
			if sshSetsField(next, field) || sshSetsField(before, field) {
				return nil, fmt.Errorf("the Match block in %s at line %d sets %s. Remove that override before taking on this setting", match.File, match.Line, field)
			}
		}
	}
	if port != oldPort {
		if slices.ContainsFunc(listeners(r.proc()), func(l *dockwellv1.HostListener) bool {
			return l.GetPort() == uint32(port) && l.GetProtocol() == "tcp"
		}) {
			return nil, fmt.Errorf("port %d already has a listener. Choose a free port for SSH", port)
		}
	}
	var jail *access.SSHJail
	if port != oldPort {
		jail, err = s.planSSHJail(ctx, oldPort, port)
		if err != nil {
			return nil, err
		}
		if err := r.checkForeignSSHPort(ctx, oldPort, port, jail); err != nil {
			return nil, err
		}
	}
	root := next.GetRootLogin()
	if root == "" && before.GetRootLogin() == "" && effectiveNow.GetRootLogin() == dockwellv1.SshRootLogin_SSH_ROOT_LOGIN_NO {
		root = "no"
	}
	if admin.uid == 0 && root == "no" {
		return nil, errors.New("root login is off in this plan. Choose a non-root administrator who can sign in")
	}
	passwordOff := next != nil && next.PasswordLogin != nil && !next.GetPasswordLogin()
	valuesNow := string(raw)
	passwordUnavailable := passwordOff || ((next == nil || next.PasswordLogin == nil) && sshValue(valuesNow, "passwordauthentication") == "no" &&
		sshValue(valuesNow, "kbdinteractiveauthentication") == "no")
	rootKeyOnly := admin.uid == 0 && (root == "prohibit-password" || (root == "" &&
		slices.Contains([]string{"prohibit-password", "without-password", "forced-commands-only"}, sshValue(valuesNow, "permitrootlogin"))))
	var proofKeys []string
	if passwordUnavailable || rootKeyOnly {
		if _, ok := r.tools.Find(sshJournal); !ok || slices.Contains([]string{"quiet", "fatal", "error"}, sshValue(valuesNow, "loglevel")) {
			return nil, errors.New("this change needs verified public-key login records. Enable the SSH journal at log level INFO or higher")
		}
		keys, err := r.authorized(ctx, admin)
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			if key.unrestricted() {
				proofKeys = append(proofKeys, key.key.GetFingerprint())
			}
		}
		if len(proofKeys) == 0 {
			return nil, fmt.Errorf("%s has no unrestricted authorized SSH key. Add a public key for this account first", account)
		}
		if (next != nil && next.KeyLogin != nil && !next.GetKeyLogin()) || ((next == nil || next.KeyLogin == nil) && effectiveNow.GetKeyLogin() != dockwellv1.SshAnswer_SSH_ANSWER_YES) {
			return nil, errors.New("this plan leaves the test account without key login. Enable key login first")
		}
	}
	sessions, err := r.sessions(ctx, []uint16{oldPort})
	if err != nil {
		return nil, err
	}
	change := &access.SSHChange{Account: account, OldPort: oldPort, Port: port, OldFile: old, Final: sshFile(next, 0), Unit: unit, Addresses: next.GetListenAddresses(), ProofKeys: proofKeys, Jail: jail}
	// Prefer the newest interactive connection when the account is shared.
	slices.SortFunc(sessions, func(a, b access.SSHSession) int {
		if (a.TTY != "") != (b.TTY != "") {
			if a.TTY != "" {
				return -1
			}
			return 1
		}
		return cmp.Compare(b.StartTicks, a.StartTicks)
	})
	for i := range sessions {
		conn := &sessions[i]
		peer := conn.Peer.Addr().Unmap()
		local := conn.Local.Addr().Unmap()
		if conn.User == account && peer.IsGlobalUnicast() && matchingSSHAddress(local, change.Addresses) {
			if !change.ProofPeer.IsValid() {
				change.ProofPeer, change.ProofHost = peer, local
			}
			if !slices.Contains(change.From, peer) {
				change.From = append(change.From, peer)
			}
			if !slices.Contains(change.Targets, local) {
				change.Targets = append(change.Targets, local)
			}
		}
	}
	if len(change.From) == 0 {
		return nil, fmt.Errorf("open an SSH session as %s from the address you'll test from, then plan again", account)
	}
	rootNarrows := next.GetRootLogin() == "no" && effectiveNow.GetRootLogin() != dockwellv1.SshRootLogin_SSH_ROOT_LOGIN_NO ||
		next.GetRootLogin() == "prohibit-password" && effectiveNow.GetRootLogin() == dockwellv1.SshRootLogin_SSH_ROOT_LOGIN_YES
	passwordNarrows := passwordOff && effectiveNow.GetPasswordLogin() != dockwellv1.SshAnswer_SSH_ANSWER_NO
	keyNarrows := next != nil && next.KeyLogin != nil && !next.GetKeyLogin() && effectiveNow.GetKeyLogin() != dockwellv1.SshAnswer_SSH_ANSWER_NO
	change.RestrictsAccess = port != oldPort || addressesChanged || passwordNarrows || keyNarrows || rootNarrows || (next == nil && before != nil)
	change.Candidate = change.Final
	if port != oldPort {
		candidate := next
		if candidate == nil {
			candidate = &resourcev1.SshConfig{}
		}
		trialSpec := &resourcev1.SshConfig{Port: uint32(port), PasswordLogin: candidate.PasswordLogin, KeyLogin: candidate.KeyLogin, RootLogin: candidate.RootLogin, ListenAddresses: candidate.ListenAddresses}
		change.Candidate = sshFile(trialSpec, oldPort)
	}
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	change.Marker = "dw-" + hex.EncodeToString(nonce[:12])
	change.Challenge = "dockwell-test-" + hex.EncodeToString(nonce[12:])
	change.Candidate += "VersionAddendum " + change.Marker + "\n"
	return change, nil
}

func matchingSSHAddress(local netip.Addr, addresses []string) bool {
	if len(addresses) == 0 {
		return true
	}
	local = local.Unmap()
	return slices.ContainsFunc(addresses, func(address string) bool {
		addr, err := netip.ParseAddr(address)
		return err == nil && (addr == local || (addr.IsUnspecified() && addr.BitLen() == local.BitLen()))
	})
}

func sshSetsField(spec *resourcev1.SshConfig, name string) bool {
	if spec == nil {
		return false
	}
	switch name {
	case "PasswordAuthentication":
		return spec.PasswordLogin != nil
	case "KbdInteractiveAuthentication", "ChallengeResponseAuthentication":
		return spec.PasswordLogin != nil && !spec.GetPasswordLogin()
	case "PubkeyAuthentication":
		return spec.KeyLogin != nil
	case "PermitRootLogin":
		return spec.GetRootLogin() != ""
	case "Port":
		return spec.GetPort() != 0
	case "ListenAddress":
		return len(spec.GetListenAddresses()) != 0
	default:
		return false
	}
}

func (s *sshSettings) readDropIn() (string, error) {
	path := s.reader.rootPath(sshDropInPath)
	for _, dir := range []string{"/etc", "/etc/ssh", "/etc/ssh/sshd_config.d"} {
		info, err := os.Lstat(s.reader.rootPath(dir))
		if err != nil || !info.IsDir() || !sshOwnership(info, s.reader.tools.Root) || info.Mode().Perm()&0o022 != 0 {
			return "", errors.New("the SSH configuration folder needs a real directory writable only by its owner")
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || !sshOwnership(info, s.reader.tools.Root) || info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("the Dockwell SSH drop-in must be a regular file writable only by its owner")
	}
	data, err := readSSHFile(path, 16<<10)
	return string(data), err
}

func (s *sshSettings) writeDropIn(body string) error {
	return writeSSHOwnedFile(s.reader.rootPath(sshDropInPath), body)
}

func writeSSHOwnedFile(path, body string) error {
	if body == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	} else {
		f, err := os.CreateTemp(filepath.Dir(path), ".dockwell-ssh-")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(f.Name()) }()
		_, writeErr := f.WriteString(body)
		if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
			return err
		}
		if err := os.Rename(f.Name(), path); err != nil {
			return err
		}
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func (s *sshSettings) Install(ctx context.Context, trial *access.SSHChange, body string) error {
	s.reader.edits.Lock()
	defer s.reader.edits.Unlock()
	candidate := body == trial.Candidate
	expected := trial.Candidate
	if candidate {
		expected = trial.OldFile
	}
	if live, err := s.readDropIn(); err != nil || live != expected {
		return errors.New("the Dockwell SSH drop-in changed while this change was being tested. Review the current SSH settings")
	}
	keepOld := trial.Port != trial.OldPort && candidate
	raw, err := s.validateStagedSSH(ctx, body)
	if err != nil {
		return err
	}
	if err := s.checkEffective(string(raw), body, trial, keepOld); err != nil {
		return err
	}
	if candidate && sshValue(string(raw), "versionaddendum") != trial.Marker {
		return errors.New("an earlier VersionAddendum setting takes precedence. Move it below the Dockwell SSH drop-in before changing SSH settings")
	}
	if candidate && trial.Jail != nil {
		if err := s.installSSHJail(ctx, trial, trial.Jail.Candidate); err != nil {
			return err
		}
	}
	if err := s.writeDropIn(body); err != nil {
		return err
	}
	if _, err := s.reader.sshTool(ctx, sshTools["systemctl"], "reload", trial.Unit); err != nil {
		return err
	}
	if err := checkSSHBanner(ctx, trial, candidate); err != nil {
		return err
	}
	if !candidate && trial.Jail != nil {
		return s.installSSHJail(ctx, trial, trial.Jail.Final)
	}
	return nil
}

func (s *sshSettings) checkEffective(raw, body string, ch *access.SSHChange, keepOld bool) error {
	var seen dockwellv1.ServerSsh
	effective(raw, &seen)
	if !slices.Contains(seen.Ports, uint32(ch.Port)) || (keepOld && !slices.Contains(seen.Ports, uint32(ch.OldPort))) || len(seen.Ports) != 1+btoi(keepOld) {
		return errors.New("sshd's effective ports differ from the plan. Remove the overriding Port or ListenAddress lines and plan again")
	}
	if len(ch.Addresses) > 0 {
		var actual []netip.Addr
		for _, address := range seen.GetListenAddresses() {
			if parsed, err := netip.ParseAddrPort(address); err == nil {
				actual = append(actual, parsed.Addr())
			}
		}
		for _, address := range ch.Addresses {
			wanted, _ := netip.ParseAddr(address)
			if !slices.Contains(actual, wanted) || slices.ContainsFunc(actual, func(a netip.Addr) bool {
				return !slices.ContainsFunc(ch.Addresses, func(w string) bool { ip, _ := netip.ParseAddr(w); return a == ip })
			}) {
				return errors.New("sshd's effective listen addresses differ. Remove the overriding ListenAddress lines and plan again")
			}
		}
	}
	for line := range strings.SplitSeq(body, "\n") {
		key, value := sshdLine(line)
		switch key {
		case "permitrootlogin":
			if sshValue(raw, key) == "without-password" && value == "prohibit-password" {
				continue
			}
		case "challengeresponseauthentication":
			if sshValue(raw, "kbdinteractiveauthentication") == value {
				continue
			}
		case "passwordauthentication", "pubkeyauthentication":
		default:
			continue
		}
		if sshValue(raw, key) != value {
			for _, source := range readSSHDConfig(s.reader.root, s.reader.changed).setIn {
				if source.File != sshDropInPath && strings.EqualFold(source.Keyword, key) {
					return fmt.Errorf("%s in %s at line %d wins. Remove that setting before taking it on in Dockwell", source.Keyword, source.File, source.Line)
				}
			}
			return fmt.Errorf("sshd still uses another value for %s. Check the files sshd_config includes and any Match blocks", key)
		}
	}
	return nil
}

func checkSSHBanner(ctx context.Context, trial *access.SSHChange, candidate bool) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for {
		for _, host := range trial.Targets {
			address := netip.AddrPortFrom(host, trial.Port)
			if sshBannerMatches(ctx, address, trial.Marker, candidate) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("the SSH listener didn't reload. Check systemd's SSH service and the listener's addresses")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func sshBannerMatches(ctx context.Context, address netip.AddrPort, marker string, candidate bool) bool {
	dialer := net.Dialer{Timeout: 250 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "tcp", address.String())
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		return false
	}
	line, err := bufio.NewReaderSize(conn, 512).ReadSlice('\n')
	text := strings.TrimSpace(string(line))
	return err == nil && strings.HasPrefix(text, "SSH-2.0-") && strings.HasSuffix(text, " "+marker) == candidate
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *sshSettings) Restore(ctx context.Context, trial *access.SSHChange) error {
	s.reader.edits.Lock()
	defer s.reader.edits.Unlock()
	if trial.Unit != "ssh.service" && trial.Unit != "sshd.service" {
		return errors.New("the SSH rollback names an unknown service")
	}
	if err := s.writeDropIn(trial.OldFile); err != nil {
		return err
	}
	var jailErr error
	if trial.Jail != nil {
		jailErr = s.installSSHJail(ctx, trial, trial.Jail.Old)
	}
	_, err := s.reader.sshTool(ctx, sshTools["systemctl"], "reload-or-restart", trial.Unit)
	return errors.Join(jailErr, err)
}

func (s *sshSettings) Started() (uint64, error) {
	raw, err := fs.ReadFile(s.reader.proc(), "uptime")
	if err != nil {
		return 0, err
	}
	value := strings.Fields(string(raw))
	if len(value) > 0 {
		if uptime, err := strconv.ParseFloat(value[0], 64); err == nil && uptime > 0 {
			return uint64(uptime * procTicks), nil
		}
	}
	return 0, errors.New("can't determine the boot clock to check for a fresh SSH connection")
}

func sshOwnership(info fs.FileInfo, root string) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (stat.Uid == 0 || (root != "" && int(stat.Uid) == os.Geteuid()))
}

func (r *sshReader) editDetails(ctx context.Context, read *dockwellv1.ServerSsh) *dockwellv1.ServerSsh {
	out := proto.CloneOf(read)
	if r.access == nil {
		out.EditRefusal = "This agent can't manage SSH settings."
		return out
	}
	out.Managed = r.access.SSHConfig(ctx)
	change, trial, proof := r.access.SSHTest(ctx)
	out.TestChange = change
	if change != "" {
		out.TestCommand = sshTestCommand(&trial)
	}
	if proof != nil {
		out.ProvedChange = change
		out.Proof = &dockwellv1.SshProof{Address: proof.Address, Method: proof.Method, Fingerprint: proof.Fingerprint}
	}
	if r.access.Applied().Unusable != "" {
		out.EditRefusal = "SSH changes need Dockwell's firewall for rollback. Check the firewall on this server."
		return out
	}
	if accounts, err := r.accounts(); err == nil {
		for _, a := range accounts {
			if a.admin {
				out.AdminAccounts = append(out.AdminAccounts, a.name)
			}
		}
		slices.Sort(out.AdminAccounts)
	}
	return out
}
