// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	_ "embed"
	"encoding/json"
	"slices"
	"strconv"
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
