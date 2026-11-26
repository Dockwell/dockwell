// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	_ "embed"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

// The panel imports the same list of Linux signals accepted by Docker.
//
//go:embed lifecycle-signals.json
var lifecycleSignalData []byte

var lifecycleSignals = func() struct {
	Names     []string
	MaxNumber int
} {
	var signals struct {
		Names     []string
		MaxNumber int
	}
	if err := json.Unmarshal(lifecycleSignalData, &signals); err != nil {
		panic(err)
	}
	return signals
}()

// ValidLifecycleSignal reports whether Docker accepts a reviewed custom signal.
func ValidLifecycleSignal(signal string) bool {
	if number, err := strconv.Atoi(signal); err == nil {
		return number >= 1 && number <= lifecycleSignals.MaxNumber && strconv.Itoa(number) == signal
	}
	return slices.Contains(lifecycleSignals.Names, signal)
}

// AllowedPausedKillSignal reports whether Kill may send signal to a paused
// container: empty means the default SIGTERM-then-SIGKILL escalation, and
// SIGKILL (or 9) terminates without resuming the process. Other signals can
// resume a paused container or sit pending instead of killing it.
func AllowedPausedKillSignal(signal string) bool {
	return signal == "" || strings.EqualFold(signal, "SIGKILL") || strings.EqualFold(signal, "KILL") || signal == "9"
}
