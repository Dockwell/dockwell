// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AuditFilter narrows the audit log as a person reads it. Each part that is
// set must hold. See docs/adr/0043-the-audit-log-for-a-person.md.
type AuditFilter struct {
	// Since keeps what happened at or after it. Zero keeps everything.
	Since time.Time
	// ActorPrefixes keeps what actors whose names start with one of these
	// did, as TimelineFilter's do: "user:", or a whole name such as
	// "dockwell".
	ActorPrefixes []string
	// Surface keeps what came from one place, such as SurfacePanel.
	Surface string
	// Query keeps the events it is in, whatever its case: in the action, the
	// actor and what the event acted on, by ID or by the name of the person,
	// token or server, and in the event's words for people.
	Query string
}

// AuditEvents returns a page of the audit log, newest first: at most limit
// events from before cursor, or from the newest with a zero cursor.
func (t *Tx) AuditEvents(ctx context.Context, f AuditFilter, before TimelineCursor, limit int) ([]Event, error) {
	var w []string
	var args []any
	if !f.Since.IsZero() {
		w = append(w, "at >= ?")
		args = append(args, millis(f.Since))
	}
	if len(f.ActorPrefixes) > 0 {
		var alts []string
		for _, p := range f.ActorPrefixes {
			if strings.HasSuffix(p, ":") {
				alts = append(alts, "actor LIKE ?")
				args = append(args, p+"%")
			} else {
				alts = append(alts, "actor = ?")
				args = append(args, p)
			}
		}
		w = append(w, "("+strings.Join(alts, " OR ")+")")
	}
	if f.Surface != "" {
		w = append(w, "surface = ?")
		args = append(args, f.Surface)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + escapeLike(strings.ToLower(q)) + "%"
		// A name is the person's, token's or server's as it is now.
		const named = `SELECT 'user:' || id FROM users WHERE lower(name) LIKE ? ESCAPE '\'
			UNION ALL SELECT 'token:' || id FROM api_tokens WHERE lower(name) LIKE ? ESCAPE '\'
			UNION ALL SELECT 'server:' || id FROM servers WHERE lower(name) LIKE ? ESCAPE '\'`
		const targets = `SELECT id FROM users WHERE lower(name) LIKE ? ESCAPE '\'
			UNION ALL SELECT id FROM api_tokens WHERE lower(name) LIKE ? ESCAPE '\'
			UNION ALL SELECT id FROM servers WHERE lower(name) LIKE ? ESCAPE '\'`
		w = append(w, `(lower(action) LIKE ? ESCAPE '\' OR lower(actor) LIKE ? ESCAPE '\' OR lower(target) LIKE ? ESCAPE '\'
			OR lower(subject) LIKE ? ESCAPE '\' OR lower(detail) LIKE ? ESCAPE '\'
			OR actor IN (`+named+`) OR target IN (`+targets+`))`)
		for range 11 {
			args = append(args, like)
		}
	}
	if before.ID != "" {
		w = append(w, "(at < ? OR (at = ? AND id < ?))")
		args = append(args, millis(before.At), millis(before.At), before.ID)
	}
	query := `SELECT ` + eventColumns + ` FROM audit_events`
	if len(w) > 0 {
		query += ` WHERE ` + strings.Join(w, " AND ")
	}
	query += ` ORDER BY at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := t.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("can't read the audit log: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("can't read the audit log: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// escapeLike makes s match itself in a LIKE pattern with ESCAPE '\'.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
