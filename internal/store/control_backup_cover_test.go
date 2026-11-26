// SPDX-License-Identifier: AGPL-3.0-only

package store_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// Excluded tables are still snapshotted unless the snapshot itself omits them.
var noBackupTrigger = map[string]string{
	"schema_migrations":     "schema bookkeeping",
	"control_backup":        "the schedule explicitly records managed configuration edits",
	"control_backup_places": "delivery status would schedule another backup after every delivery",
	"api_token_addresses":   "token last-used IP observations",
	"audit_events":          "activity history",
	"git_hook_deliveries":   "webhook delivery history",
	"task_runs":             "scheduled job execution history",
	"builds":                "build execution and runtime status",
	"build_lines":           "build logs",
	"alerts":                "live alert state from the evaluator",
	"alert_deliveries":      "notification outbox",
	"alert_sends":           "notification attempts",
	"history_series":        "metric history omitted from bundles",
	"history_chunks":        "metric history omitted from bundles",
	"server_history":        "metric history omitted from bundles",
	"file_access_undo":      "short-lived undo records for filesystem operations",
}

func TestEveryTableHasAControlBackupTriggerOrExclusion(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		_, location := storetest.Open(t, backend)
		driver := "sqlite"
		query := `SELECT t.name, COALESCE(tr.name, '') FROM sqlite_schema t
			LEFT JOIN sqlite_schema tr ON tr.type = 'trigger' AND tr.tbl_name = t.name AND tr.name LIKE 'control_backup_%'
			WHERE t.type = 'table' AND t.name NOT LIKE 'sqlite_%'`
		if backend == store.Postgres {
			driver = "pgx"
			query = `SELECT t.table_name, COALESCE(tr.event_manipulation, '') FROM information_schema.tables t
				LEFT JOIN information_schema.triggers tr ON tr.event_object_schema = t.table_schema
				AND tr.event_object_table = t.table_name AND tr.trigger_name LIKE 'control_backup_%'
				WHERE t.table_schema = current_schema() AND t.table_type = 'BASE TABLE'`
		}
		db, err := sql.Open(driver, location)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		rows, err := db.QueryContext(t.Context(), query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		triggers := make(map[string]map[string]bool)
		for rows.Next() {
			var table, trigger string
			if err := rows.Scan(&table, &trigger); err != nil {
				t.Fatal(err)
			}
			if triggers[table] == nil {
				triggers[table] = make(map[string]bool)
			}
			if trigger != "" {
				triggers[table][trigger] = true
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		for table, events := range triggers {
			if noBackupTrigger[table] != "" {
				if len(events) != 0 {
					t.Errorf("excluded table %s has a backup trigger", table)
				}
				continue
			}
			// Session issuance, cleanup and last-seen writes are routine. Only revocation is managed.
			if table == "sessions" && len(events) == 1 && (events["UPDATE"] || events["control_backup_sessions_update"]) {
				continue
			}
			for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
				if !events[event] && !events["control_backup_"+table+"_"+strings.ToLower(event)] {
					t.Errorf("%s needs a backup trigger for %s or a documented exclusion", table, event)
				}
			}
		}
	})
}
