// SPDX-License-Identifier: AGPL-3.0-only

package ticket

import (
	"errors"
	"strings"
	"testing"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/pki"
)

func aTicket(control string, others ...string) *agentv1.JoinTicket {
	return &agentv1.JoinTicket{
		ControlUrl: control, OtherUrls: others,
		CaPin: pki.PinPrefix + "abc", TokenId: "tok_1", Secret: make([]byte, SecretSize),
	}
}

func TestATicketGoesRoundWithEveryAddress(t *testing.T) {
	got, err := Decode(Encode(aTicket("https://203.0.113.50:7443", "https://[2001:db8::5]:7443", "https://10.0.0.25:7443")))
	if err != nil {
		t.Fatal(err)
	}
	if got.GetControlUrl() != "https://203.0.113.50:7443" || len(got.GetOtherUrls()) != 2 {
		t.Errorf("it reads %v", got)
	}
}

// Every address a ticket holds is checked, as a joining server dials each in turn, and
// sends its secret to whichever answers.
func TestEveryAddressATicketHoldsIsAnHTTPSURL(t *testing.T) {
	for _, bad := range []*agentv1.JoinTicket{
		aTicket("http://203.0.113.50:7443"),
		aTicket("https://"),
		aTicket("https://203.0.113.50:7443", "http://10.0.0.25:7443"),
		aTicket("https://203.0.113.50:7443", "10.0.0.25:7443"),
		aTicket("https://203.0.113.50:7443", "https://"),
	} {
		_, err := Decode(Encode(bad))
		if !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), "isn't an https URL") {
			t.Errorf("%v reads as %v", bad, err)
		}
	}
	if _, err := Decode("not a ticket"); !errors.Is(err, ErrMalformed) {
		t.Errorf("garbage reads as %v", err)
	}
}
