// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import "testing"

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
