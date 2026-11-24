// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"fmt"
	"io/fs"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/agent/access"
)

const sshProofScript = "sleep 65; :"

func sshTestCommand(trial *access.SSHChange) string {
	if trial.Challenge == "" || !trial.ProofHost.IsValid() {
		return ""
	}
	auth := ""
	if len(trial.ProofKeys) > 0 {
		auth = " -o PreferredAuthentications=publickey"
	}
	return fmt.Sprintf("ssh -o ControlPath=none -o ClearAllForwardings=yes%s -p %d -l '%s' -- %s \"exec /bin/sh -c '%s' %s\"",
		auth, trial.Port, trial.Account, trial.ProofHost, sshProofScript, trial.Challenge)
}

// The independent revert can have run even though the wait still exists.
func (s *sshSettings) CandidateLive(trial *access.SSHChange) bool {
	body, err := s.readDropIn()
	return err == nil && body == trial.Candidate && trial.Challenge != ""
}

func (s *sshSettings) Proved(ctx context.Context, trial *access.SSHChange) (*access.SSHProof, netip.Addr) {
	var unexpected netip.Addr
	sessions, err := s.reader.sessions(ctx, []uint16{trial.Port})
	if err != nil {
		return nil, unexpected
	}
	for i := range sessions {
		session := &sessions[i]
		if session.StartTicks <= trial.Started || session.User != trial.Account ||
			!matchingSSHAddress(session.Local.Addr(), trial.Addresses) ||
			!sshChallengeRunning(s.reader.proc(), strconv.Itoa(session.PID), trial.Challenge) {
			continue
		}
		peer := session.Peer.Addr().Unmap()
		if peer != trial.ProofPeer {
			unexpected = peer
			continue
		}
		proof := &access.SSHProof{Address: peer.String()}
		logins, _, err := s.reader.sshJournalLog(ctx, trial.StartedAt, true)
		if err == nil {
			for _, entry := range logins {
				if checkSSHAuthentication(trial, session.Peer, entry, proof) {
					return proof, unexpected
				}
			}
		}
		// Where a journal isn't available, the challenge proves this live
		// session, but can't attest to the method or key the client used.
		if len(trial.ProofKeys) == 0 {
			return proof, unexpected
		}
	}
	return nil, unexpected
}

func checkSSHAuthentication(trial *access.SSHChange, peer netip.AddrPort, entry sshJournalEntry, proof *access.SSHProof) bool {
	login := parseSSHLogin(entry.when(), entry.Message)
	match := sshLoginRE().FindStringSubmatch(entry.Message)
	if !login.GetAccepted() || login.GetUser() != trial.Account || login.GetAddress() != peer.Addr().Unmap().String() ||
		match[5] != strconv.Itoa(int(peer.Port())) || entry.when().Before(trial.StartedAt.Truncate(time.Microsecond)) {
		return false
	}
	var fingerprint string
	var certificate bool
	if login.GetMethod() == "publickey" {
		for _, field := range strings.Fields(match[6]) {
			certificate = certificate || strings.HasSuffix(field, "-CERT")
			if strings.HasPrefix(field, "SHA256:") {
				fingerprint = field
				break
			}
		}
	}
	if len(trial.ProofKeys) > 0 && (login.GetMethod() != "publickey" || certificate || !slices.Contains(trial.ProofKeys, fingerprint)) {
		return false
	}
	proof.Method, proof.Fingerprint = login.GetMethod(), fingerprint
	return true
}

// The shell running the one-time command must be a descendant of the
// authenticated socket's monitor. A reconnect or a command in an older login
// can't satisfy both the socket and command checks.
func sshChallengeRunning(proc fs.FS, monitor, challenge string) bool {
	queue := []string{monitor}
	for i := 0; i < len(queue) && i < 64; i++ {
		pid := queue[i]
		if pid != monitor {
			raw, err := fs.ReadFile(proc, pid+"/cmdline")
			if err == nil && string(raw) == "/bin/sh\x00-c\x00"+sshProofScript+"\x00"+challenge+"\x00" {
				return true
			}
		}
		children, err := fs.ReadFile(proc, pid+"/task/"+pid+"/children")
		if err != nil {
			continue
		}
		for _, child := range strings.Fields(string(children)) {
			if n, err := strconv.Atoi(child); err == nil && n > 1 && procParent(proc, child) == pid {
				queue = append(queue, child)
			}
		}
	}
	return false
}

type sshJournalEntry struct {
	At      string `json:"__REALTIME_TIMESTAMP"`
	Message string `json:"MESSAGE"`
	UID     string `json:"_UID"`
}

func (e sshJournalEntry) when() time.Time {
	us, err := strconv.ParseInt(e.At, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMicro(us)
}
