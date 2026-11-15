// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// A deploy-on-push hook acts for the person who made it (ADR 0046): what the
// engine keeps of that.

// forgetHooks deletes the deploy-on-push hook of a resource change deletes, with
// a line in the audit log for each, as turning one off has.
func forgetHooks(ctx context.Context, tx *store.Tx, resourceID, changeID string, by By, at time.Time) error {
	gone, err := tx.DeleteGitHooksFor(ctx, resourceID)
	if err != nil {
		return err
	}
	for i := range gone {
		if err := tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: at, Actor: by.Actor, Surface: by.Via, Action: "git_hook.delete",
			Subject: gone[i].UserID, Target: resourceID, Detail: "hook " + gone[i].ID + " deleted with its app, in " + changeID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// isHook reports a caller's token that is a deploy-on-push hook's stand-in, by
// its ID, so a refusal names the hook, not an API token (ADR 0071).
func isHook(tokenID string) bool {
	return strings.HasPrefix(tokenID, id.GitHook+"_")
}

// actsFor is who a change by actor acts for, to record on it: a deploy-on-push
// hook's maker, who is the caller its steps carry, or nobody for any other actor.
func actsFor(ctx context.Context, actor string) string {
	if !strings.HasPrefix(actor, "hook:") {
		return ""
	}
	if caller, ok := place.CallerFrom(ctx); ok {
		return caller.UserID
	}
	return ""
}
