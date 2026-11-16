// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"

	"google.golang.org/protobuf/encoding/protojson"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// publicCommand says, a line each as "<service> <port>", the ports an entry declares
// public, as an install's values have them: "proxy 80", or "proxy 443/udp". It reads
// {"entry": <the entry, as the API gives it>, "values": {...}} on stdin, for
// scripts/catalog-e2e.sh's floor, which allows a port past the server only there
// (ADR 0101; #752's review, L4).
func publicCommand(stdin io.Reader, stdout io.Writer) error {
	var in struct {
		Entry  json.RawMessage   `json:"entry"`
		Values map[string]string `json:"values"`
	}
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return fmt.Errorf("reading the entry and its values on stdin: %w", err)
	}
	if len(in.Entry) == 0 {
		return errors.New("stdin has no entry. Give {\"entry\": ..., \"values\": {...}}")
	}
	e := &catalogv1.Entry{}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(in.Entry, e); err != nil {
		return fmt.Errorf("reading the entry: %w", err)
	}
	public := catalog.PublicPorts(e, in.Values)
	for _, service := range slices.Sorted(maps.Keys(public)) {
		for _, port := range public[service] {
			if _, err := fmt.Fprintln(stdout, service, port); err != nil {
				return err
			}
		}
	}
	return nil
}
