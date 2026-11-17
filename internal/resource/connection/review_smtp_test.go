// SPDX-License-Identifier: AGPL-3.0-only

package connection

import (
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// Security none with a user is refused to a public address, written as IPv6 too:
// the app would send its password across the internet in the clear.
func TestReviewSMTPNoTLSToAPublicIPv6IsRefused(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"2001:db8::25", "2001:db8:5::587", "203.0.113.7"} {
		c := relay()
		c.Smtp.Host = host
		c.Smtp.Security = resourcev1.SmtpSecurity_SMTP_SECURITY_NONE
		if err := (Type{}).Validate(c); err == nil {
			t.Errorf("security none with a password to %s is taken", host)
		}
	}
}
