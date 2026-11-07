// SPDX-License-Identifier: AGPL-3.0-only

package enroll

import "time"

// SetJoinBounds has a test reach a ticket's addresses with shorter bounds, and puts
// them back when it's done.
func SetJoinBounds(dial, handshake time.Duration) (restore func()) {
	wasDial, wasHandshake := joinDial, joinHandshake
	joinDial, joinHandshake = dial, handshake
	return func() { joinDial, joinHandshake = wasDial, wasHandshake }
}
