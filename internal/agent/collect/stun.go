// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net"
	"net/netip"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
)

// STUN, as much of RFC 8489 as a binding request takes: 20 bytes out, and the one
// attribute that matters back. There's no module for this, as it's on every server
// and the wire format is this small.
const (
	stunBindingRequest  = 0x0001
	stunBindingResponse = 0x0101
	stunMagicCookie     = 0x2112A442
	stunHeaderLength    = 20
	stunMappedAddress   = 0x0001
	stunXorMapped       = 0x0020
	stunFamilyIPv4      = 0x01
	stunFamilyIPv6      = 0x02
	// stunTry bounds one server. stunServers has two, so a server whose UDP is
	// blocked is done with this in a few seconds.
	stunTry = 1500 * time.Millisecond
	// stunMost is the most of an answer that's read.
	stunMost = 548
)

// stunServers are asked in turn, and the first to answer ends it, so only one of
// them hears from a server that can reach it. docs/security/public-address.md says
// who they are and what they get, and needs to change with them.
var stunServers = []string{"stun.cloudflare.com:3478", "stun.l.google.com:19302"}

// stunLookup asks servers, in turn, where this server's packets come from. Empty
// means none said, as with UDP blocked. What it finds is a way out, and nothing
// more: behind a router, it's the router's (ADR 0034).
func stunLookup(ctx context.Context, servers []string) string {
	for _, server := range servers {
		if a, ok := stunBind(ctx, server); ok {
			return a.String()
		}
		if ctx.Err() != nil {
			break
		}
	}
	return ""
}

// stunBind makes one binding request. A connected socket hears from that server
// alone, and the answer has to carry the request's random ID, so nothing else on
// the way can answer for it unless it can read the request.
func stunBind(ctx context.Context, server string) (netip.Addr, bool) {
	ctx, cancel := context.WithTimeout(ctx, stunTry)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp4", server)
	if err != nil {
		return netip.Addr{}, false
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	var transaction [12]byte
	if _, err := rand.Read(transaction[:]); err != nil {
		return netip.Addr{}, false
	}
	if _, err := conn.Write(stunRequest(transaction)); err != nil {
		return netip.Addr{}, false
	}
	buf := make([]byte, stunMost)
	n, err := conn.Read(buf)
	if err != nil {
		return netip.Addr{}, false
	}
	a, ok := stunParse(buf[:n], transaction)
	return a, ok && resource.IsPublicAddress(a)
}

// stunRequest is a binding request, with nothing in it but its ID.
func stunRequest(transaction [12]byte) []byte {
	msg := make([]byte, stunHeaderLength)
	binary.BigEndian.PutUint16(msg[0:], stunBindingRequest)
	binary.BigEndian.PutUint32(msg[4:], stunMagicCookie)
	copy(msg[8:], transaction[:])
	return msg
}

// stunParse finds the address in a binding response to transaction. It takes
// XOR-MAPPED-ADDRESS, or MAPPED-ADDRESS from a server too old to send that.
func stunParse(msg []byte, transaction [12]byte) (netip.Addr, bool) {
	if len(msg) < stunHeaderLength ||
		binary.BigEndian.Uint16(msg[0:]) != stunBindingResponse ||
		binary.BigEndian.Uint32(msg[4:]) != stunMagicCookie ||
		[12]byte(msg[8:20]) != transaction {
		return netip.Addr{}, false
	}
	length := int(binary.BigEndian.Uint16(msg[2:]))
	if stunHeaderLength+length > len(msg) {
		return netip.Addr{}, false
	}
	key := msg[4:stunHeaderLength]
	var mapped netip.Addr
	for rest := msg[stunHeaderLength : stunHeaderLength+length]; len(rest) >= 4; {
		kind := binary.BigEndian.Uint16(rest[0:])
		size := int(binary.BigEndian.Uint16(rest[2:]))
		if 4+size > len(rest) {
			return netip.Addr{}, false
		}
		value := rest[4 : 4+size]
		switch kind {
		case stunXorMapped:
			if a, ok := stunAddress(value, key); ok {
				return a, true
			}
		case stunMappedAddress:
			if a, ok := stunAddress(value, nil); ok {
				mapped = a
			}
		}
		// Attributes are padded to four bytes, and the padding isn't in their length.
		next := 4 + (size+3)&^3
		if next > len(rest) {
			break
		}
		rest = rest[next:]
	}
	return mapped, mapped.IsValid()
}

// stunAddress reads an address attribute: a byte of nothing, the family, the port
// and the address, XORed with key if it has one, which is the cookie and the ID.
func stunAddress(value, key []byte) (netip.Addr, bool) {
	if len(value) < 4 {
		return netip.Addr{}, false
	}
	size := 0
	switch value[1] {
	case stunFamilyIPv4:
		size = 4
	case stunFamilyIPv6:
		size = 16
	default:
		return netip.Addr{}, false
	}
	if len(value) != 4+size {
		return netip.Addr{}, false
	}
	raw := make([]byte, size)
	copy(raw, value[4:])
	for i := range raw {
		if key != nil {
			raw[i] ^= key[i]
		}
	}
	return netip.AddrFromSlice(raw)
}
