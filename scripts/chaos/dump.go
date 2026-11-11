// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// dump leaves enough state to assess a failure without copying credentials,
// specs or decrypted desired records into a CI artifact.
func (s *stand) dump(name string, round int) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	state := map[string]any{"scenario": name, "round": round}
	boxes, err := s.boxes(ctx)
	state["containers"], state["containers_error"] = boxes, errorText(err)
	live, err := s.live(ctx)
	state["live"], state["live_error"] = live, errorText(err)
	events, err := s.reconciled(ctx)
	state["activity"], state["activity_error"] = events, errorText(err)
	drift, err := s.drift(ctx)
	state["drift"], state["drift_error"] = drift, errorText(err)
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		say("can't describe the failure: %v", err)
		return
	}
	path := filepath.Join(s.dir, fmt.Sprintf("failure-%s-%d.json", strings.ReplaceAll(name, "/", "-"), round))
	if err := os.WriteFile(path, b, 0o600); err != nil { //nolint:gosec // a fixed scenario name in the stand's own temporary folder
		say("can't keep the failure state: %v", err)
		return
	}
	say("the failure state is in %s", path)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
