// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"
)

// stunResponder answers binding requests on a loopback port as answer says.
func stunResponder(t *testing.T, answer func(request []byte) []byte) string {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if reply := answer(buf[:n]); reply != nil {
				_, _ = conn.WriteTo(reply, from)
			}
		}
	}()
	return conn.LocalAddr().String()
}

// xorMapped is a binding response saying a, with padding before it to step over.
func xorMapped(request []byte, a netip.Addr) []byte {
	raw := a.As4()
	value := []byte{0, stunFamilyIPv4, 0xa1, 0x47}
	for i, b := range raw {
		value = append(value, b^request[4+i])
	}
	body := []byte{0x80, 0x22, 0, 11, 't', 'e', 's', 't', ' ', 'v', 'e', 'c', 't', 'o', 'r', 0, 0, stunXorMapped, 0, byte(len(value))}
	body = append(body, value...)
	msg := make([]byte, stunHeaderLength, stunHeaderLength+len(body))
	copy(msg, request[:stunHeaderLength])
	binary.BigEndian.PutUint16(msg[0:], stunBindingResponse)
	binary.BigEndian.PutUint16(msg[2:], uint16(len(body)))
	return append(msg, body...)
}

func TestABindingRequestIsTwentyBytesWithNothingOfTheServer(t *testing.T) {
	var transaction [12]byte
	transaction[0] = 0xb7
	msg := stunRequest(transaction)
	if len(msg) != 20 || msg[0] != 0 || msg[1] != 1 || msg[2] != 0 || msg[3] != 0 || binary.BigEndian.Uint32(msg[4:]) != 0x2112A442 || msg[8] != 0xb7 {
		t.Errorf("the request is %x", msg)
	}
}

// RFC 5769's sample IPv4 response, as far as the address.
func TestAnAddressIsReadFromASampleResponse(t *testing.T) {
	transaction := [12]byte{0xb7, 0xe7, 0xa7, 0x01, 0xbc, 0x34, 0xd6, 0x86, 0xfa, 0x87, 0xdf, 0xae}
	msg := []byte{
		0x01, 0x01, 0x00, 0x1c, 0x21, 0x12, 0xa4, 0x42,
		0xb7, 0xe7, 0xa7, 0x01, 0xbc, 0x34, 0xd6, 0x86, 0xfa, 0x87, 0xdf, 0xae,
		0x80, 0x22, 0x00, 0x0b, 0x74, 0x65, 0x73, 0x74, 0x20, 0x76, 0x65, 0x63, 0x74, 0x6f, 0x72, 0x20,
		0x00, 0x20, 0x00, 0x08, 0x00, 0x01, 0xa1, 0x47, 0xe1, 0x12, 0xa6, 0x43,
	}
	a, ok := stunParse(msg, transaction)
	if !ok || a != netip.MustParseAddr("192.0.2.1") {
		t.Errorf("read %v, %t, want 192.0.2.1", a, ok)
	}
	other := transaction
	other[11]++
	if _, ok := stunParse(msg, other); ok {
		t.Error("an answer to another request was believed")
	}
	if _, ok := stunParse(msg[:30], transaction); ok {
		t.Error("a message cut short was believed")
	}
	if _, ok := stunParse(msg[:19], transaction); ok {
		t.Error("less than a header was believed")
	}
}

func TestALookupReadsAnAnswerOverRealUDP(t *testing.T) {
	want := netip.MustParseAddr("203.0.113.7")
	server := stunResponder(t, func(request []byte) []byte {
		if len(request) != stunHeaderLength {
			t.Errorf("the request is %d bytes", len(request))
		}
		return xorMapped(request, want)
	})
	if got := stunLookup(t.Context(), []string{server}); got != want.String() {
		t.Errorf("found %q", got)
	}
}

func TestALookupGoesOnToTheNextServerAndOnlyAsFarAsItNeeds(t *testing.T) {
	asked := make(chan struct{}, 1)
	silent := stunResponder(t, func([]byte) []byte { return nil })
	answers := stunResponder(t, func(request []byte) []byte { return xorMapped(request, netip.MustParseAddr("203.0.113.7")) })
	never := stunResponder(t, func([]byte) []byte { asked <- struct{}{}; return nil })
	if got := stunLookup(t.Context(), []string{silent, answers, never}); got != "203.0.113.7" {
		t.Errorf("found %q", got)
	}
	select {
	case <-asked:
		t.Error("it asked one more than it needed")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAnAnswerThatIsntPublicSaysNothing(t *testing.T) {
	server := stunResponder(t, func(request []byte) []byte { return xorMapped(request, netip.MustParseAddr("10.0.0.5")) })
	if got := stunLookup(t.Context(), []string{server}); got != "" {
		t.Errorf("found %q", got)
	}
}

// With UDP blocked nothing answers, so it says nothing, and within its bound.
func TestALookupWithUDPBlockedSaysNothing(t *testing.T) {
	server := stunResponder(t, func([]byte) []byte { return nil })
	start := time.Now()
	if got := stunLookup(t.Context(), []string{server}); got != "" {
		t.Errorf("found %q", got)
	}
	if took := time.Since(start); took > stunTry+time.Second {
		t.Errorf("took %s", took)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := stunLookup(ctx, []string{server, server}); got != "" {
		t.Errorf("found %q when stopped", got)
	}
}
