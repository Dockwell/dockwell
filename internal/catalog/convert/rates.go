// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	_ "embed"
	"encoding/json"
	"sync"
)

// rates is what each store converted to at a commit the repository pins, as `make
// store-rates` records it and CI holds it to (ADR 0085).
//
//go:embed rates.json
var rates []byte

// A Measured store is what it converted to at the commit the repository pins: how
// many apps it held, and how many converted. What a source of it shows can differ a
// little, as an app another catalog has is another choice of that app or taken.
type Measured struct {
	Commit    string
	Apps      int
	Converted int
}

var measured = sync.OnceValue(func() map[string]Measured {
	var rec struct {
		Stores []struct {
			Format    string   `json:"format"`
			Commit    string   `json:"commit"`
			Apps      int      `json:"apps"`
			Converted []string `json:"converted"`
		} `json:"stores"`
	}
	out := map[string]Measured{}
	if json.Unmarshal(rates, &rec) != nil {
		return out
	}
	for _, s := range rec.Stores {
		out[s.Format] = Measured{Commit: s.Commit, Apps: s.Apps, Converted: len(s.Converted)}
	}
	return out
})

// MeasuredOf is what a store of a format converted to when the repository last
// measured it, such as for saying "about 99 of 269 apps install" before a person
// adds it, and false for a format it hasn't measured.
func MeasuredOf(format string) (Measured, bool) {
	m, ok := measured()[format]
	return m, ok
}
