// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"strings"
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// A restore into a new database says it fills that one and leaves the database it
// was taken of as it is, never that it replaces anything or keeps a copy of what
// was there (ADR 0106).
func TestARestoreIntoANewDatabaseSaysItReplacesNothing(t *testing.T) {
	got := strings.Join(restoreWords(&dockwellv1.RestorePlan{IntoNew: true, Method: dockwellv1.BackupMethod_BACKUP_METHOD_DUMP, Database: "PostgreSQL", SizeBytes: 2048}), " ")
	if !strings.Contains(got, "Loads the PostgreSQL dump from") || !strings.Contains(got, "keeps running as it is") || strings.Contains(got, "Replaces") || strings.Contains(got, "7 days") {
		t.Errorf("it says %q", got)
	}
	got = strings.Join(restoreWords(&dockwellv1.RestorePlan{IntoNew: true, Method: dockwellv1.BackupMethod_BACKUP_METHOD_COPY}), " ")
	if !strings.Contains(got, "Fills the new database with the copy from") || strings.Contains(got, "Replaces") {
		t.Errorf("it says %q", got)
	}
}
