// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"maps"
	"testing"
)

// A new database runs beside the one it was restored from, on the same server, so
// each port that one publishes from a variable, in any form a Compose file writes
// it, moves to the next that nothing Dockwell runs has taken, passing over the
// range the servers lend to outgoing connections, and the plan says so. A variable
// with no value takes the file's default. What isn't a published port stays (ADR
// 0106, the review's L1).
func TestARestoredDatabasePublishesOnPortsOfItsOwn(t *testing.T) {
	file := `services:
  db:
    image: postgres:17
    environment:
      POSTGRES_DB: ${DB_NAME}
  adminer:
    image: adminer:6
    ports:
      - "${ADMIN_PORT}:8080"
      - 127.0.0.1:${LOCAL_PORT:-9000}:9000
      - $SHORT_PORT:8081
      - "${DEF_PORT:-8090}:8082"
      - target: 80
        published: "${LONG_PORT}"
`
	variables := map[string]string{"ADMIN_PORT": "8081", "LOCAL_PORT": "9000", "SHORT_PORT": "7000", "LONG_PORT": "49999", "DB_NAME": "8081"}
	taken := map[uint32]bool{8081: true, 8082: true, 9000: true, 7000: true, 7001: true, 8090: true}
	// This server lends 50000 to 60999, not Linux's default.
	lent := func(p uint32) bool { return p >= 50000 && p <= 60999 }
	notes := movePorts(file, variables, taken, lent)
	want := map[string]string{
		"ADMIN_PORT": "8083", "LOCAL_PORT": "9001", "SHORT_PORT": "7002", "DEF_PORT": "8091", "LONG_PORT": "61000", "DB_NAME": "8081",
	}
	if !maps.Equal(variables, want) {
		t.Errorf("the variables are %v, want %v", variables, want)
	}
	if len(notes) != 5 || notes[0].Message != "ADMIN_PORT is 8083, as the database it was taken of publishes on 8081" {
		t.Errorf("the notes are %+v", notes)
	}
	if !taken[8083] || !taken[9001] || !taken[7002] || !taken[8091] || !taken[61000] {
		t.Errorf("a moved port isn't taken after: %v", taken)
	}
}
