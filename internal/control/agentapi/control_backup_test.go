// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"bytes"
	"io"
	"os"
	"testing"
)

func TestControlBackupTransferIsReceiveOnlyAndBoundToTheIntendedStream(t *testing.T) {
	agents := &Agents{sessions: map[string]*Session{}, transfers: map[uint64]*transfer{}}
	session := &Session{agents: agents, server: "srv_1", serial: "current"}
	agents.sessions[session.server] = session
	transfer := agents.openTransfer(session.server)
	transfer.control = &controlBackupTransfer{session: session}
	defer agents.closeTransfer(transfer)

	peer := Peer{ServerID: session.server, Serial: session.serial}
	if agents.takeTransfer(peer, transfer.token, false) != nil ||
		agents.takeTransfer(Peer{ServerID: "srv_2", Serial: peer.Serial}, transfer.token, true) != nil ||
		agents.takeTransfer(Peer{ServerID: peer.ServerID, Serial: "old"}, transfer.token, true) != nil {
		t.Fatal("the transfer was available outside its session or direction")
	}
	agents.sessions[session.server] = &Session{agents: agents, server: peer.ServerID, serial: peer.Serial}
	if agents.takeTransfer(peer, transfer.token, true) != nil {
		t.Fatal("a replacement connection accepted the transfer")
	}
	agents.sessions[session.server] = session
	if agents.takeTransfer(peer, transfer.token, true) != transfer ||
		agents.takeTransfer(peer, transfer.token, true) != nil {
		t.Fatal("the transfer was not single use")
	}
}

func TestGitPackTransferCallsOnReadWhenEOFReached(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "dockwell-ssh-pack-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("PACKtest"); err != nil {
		t.Fatal(err)
	}
	name := f.Name()
	var called int
	cleanup := func() {
		called++
		_ = f.Close()
		_ = os.Remove(name)
	}
	agents := &Agents{sessions: map[string]*Session{}, transfers: map[uint64]*transfer{}}
	session := &Session{agents: agents, server: "srv_1", serial: "s1"}
	tr := session.openGitPackTransfer(f, int64(len("PACKtest")), cleanup)
	defer agents.closeTransfer(tr)

	// Reading the SectionReader to EOF and invoking onRead removes the pack file
	// immediately, matching controlBackupTransfer.send on EOF (N13).
	var got bytes.Buffer
	if _, err := io.Copy(&got, tr.control.file); err != nil {
		t.Fatal(err)
	}
	if got.String() != "PACKtest" {
		t.Fatalf("read = %q, want PACKtest", got.String())
	}
	tr.control.onRead()
	if called != 1 {
		t.Fatalf("onRead called %d times, want 1", called)
	}
	if _, err := os.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("pack temp file still exists after onRead: %v", err)
	}
}
