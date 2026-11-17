// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"context"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/store"
)

// runsThere refuses to place a service on a server whose architecture it doesn't say
// it runs on, as an image with no build for a server's architecture fails only as
// Docker pulls it there, after the plan is applied (ADR 0102). A service that names
// none runs anywhere, as before.
//
// A server that hasn't reported its architecture yet is refused only where it's new
// to the service, or the service's list changes: a change that leaves the service
// where it runs, as one to a value it uses does, isn't held up by a report that's
// missing, as after a restore that keeps none. One that reports another architecture
// is refused wherever it is, as that's a fact (#570's arm64 review, M1).
func runsThere(ctx context.Context, tx *store.Tx, it item, servers []string) error {
	svc, ok := it.spec.(*resourcev1.Service)
	if !ok || len(svc.GetArchitectures()) == 0 || len(servers) == 0 {
		return nil
	}
	arch := svc.GetArchitectures()
	was, _ := it.current.(*resourcev1.Service)
	changed := was == nil || !slices.Equal(was.GetArchitectures(), arch)
	reports, err := tx.ServerReports(ctx)
	if err != nil {
		return err
	}
	alone := strings.Join(arch, " or ")
	for _, id := range servers {
		name := id
		if s, err := tx.GetServer(ctx, id); err == nil && s.Name != "" {
			name = s.Name
		}
		var inv dockwellv1.ServerInventory
		r, ok := reports[id]
		if !ok || proto.Unmarshal(r.Inventory, &inv) != nil || inv.GetArchitecture() == "" {
			if !changed && slices.Contains(it.placed, id) {
				continue
			}
			return fault.New(ErrInvalid, "%s runs on %s alone, and %s hasn't said its architecture yet, so Dockwell can't tell whether %s runs there. It says once its agent connects; plan this again then, or put %s on %s", it.ref.Name, alone, name, it.ref.Name, it.ref.Name, an(alone)+" server")
		}
		if !slices.Contains(arch, inv.GetArchitecture()) {
			return fault.New(ErrInvalid, "%s runs on %s alone, and %s is %s, so its images have no build there and it wouldn't start. Put %s on %s", it.ref.Name, alone, name, inv.GetArchitecture(), it.ref.Name, an(alone)+" server")
		}
	}
	return nil
}

// an is a word with the article it takes: "an amd64", "an arm64".
func an(word string) string {
	if word != "" && strings.ContainsRune("aeiou", rune(word[0])) {
		return "an " + word
	}
	return "a " + word
}
