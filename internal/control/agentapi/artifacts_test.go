// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import "testing"

func TestArtifactGrantIsSingleUseAndBoundToTheAgentStream(t *testing.T) {
	agents := &Agents{sessions: map[string]*Session{}, transfers: map[uint64]*transfer{}}
	s := &Session{agents: agents, server: "srv", serial: "certificate"}
	agents.sessions[s.server] = s
	peer := Peer{ServerID: s.server, Serial: s.serial}
	g := &artifactGrant{id: "lease", owner: "person/session", offset: 128, length: 256, receive: true}
	transfer, err := s.grantArtifact(g)
	if err != nil {
		t.Fatal(err)
	}
	defer agents.closeTransfer(transfer)
	if agents.takeTransfer(peer, transfer.token, false) != nil {
		t.Fatal("opened in the other direction")
	}
	if agents.takeTransfer(Peer{ServerID: s.server, Serial: "renewed"}, transfer.token, true) != nil {
		t.Fatal("opened under a different certificate")
	}
	if agents.takeTransfer(peer, transfer.token, true) != transfer || agents.takeTransfer(peer, transfer.token, true) != nil {
		t.Fatal("grant was not single-use")
	}
	if !agents.artifactCurrent(transfer) {
		t.Fatal("live stream was refused")
	}
	agents.sessions[s.server] = &Session{agents: agents, server: s.server, serial: s.serial}
	if agents.artifactCurrent(transfer) {
		t.Fatal("grant survived session replacement")
	}
}
