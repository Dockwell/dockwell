// SPDX-License-Identifier: AGPL-3.0-only

package resource_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
)

func TestNewAccess(t *testing.T) {
	path := func(p string, ro bool) resource.Access {
		return resource.Access{Kind: resource.AccessPath, Path: p, ReadOnly: ro}
	}
	docker := func(ro bool) resource.Access {
		return resource.Access{Kind: resource.AccessDocker, Path: "/var/run/docker.sock", ReadOnly: ro}
	}
	relabelled := path("/a", false)
	relabelled.Relabel = "z"
	for name, c := range map[string]struct{ from, to, want []resource.Access }{
		"nothing":                     {},
		"new":                         {to: []resource.Access{path("/a", true)}, want: []resource.Access{path("/a", true)}},
		"the same":                    {from: []resource.Access{path("/a", false)}, to: []resource.Access{path("/a", false)}},
		"less":                        {from: []resource.Access{path("/a", false)}, to: []resource.Access{path("/a", true)}},
		"read-only to read-write":     {from: []resource.Access{path("/a", true)}, to: []resource.Access{path("/a", false)}, want: []resource.Access{path("/a", false)}},
		"either way, read-write kept": {from: []resource.Access{path("/a", true), path("/a", false)}, to: []resource.Access{path("/a", false)}},
		"either way, other order":     {from: []resource.Access{path("/a", false), path("/a", true)}, to: []resource.Access{path("/a", false)}},
		"listed twice":                {to: []resource.Access{path("/a", true), path("/a", true)}, want: []resource.Access{path("/a", true)}},
		"both ways":                   {to: []resource.Access{path("/a", true), path("/a", false)}, want: []resource.Access{path("/a", true), path("/a", false)}},
		"relabelled":                  {from: []resource.Access{path("/a", false)}, to: []resource.Access{relabelled}, want: []resource.Access{relabelled}},
		"Docker's socket, read-only":  {from: []resource.Access{docker(true)}, to: []resource.Access{docker(false)}},
		"another":                     {from: []resource.Access{path("/a", false)}, to: []resource.Access{path("/b", false)}, want: []resource.Access{path("/b", false)}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := resource.NewAccess(c.from, c.to); !slices.Equal(got, c.want) {
				t.Errorf("NewAccess = %v, want %v", got, c.want)
			}
		})
	}
}

// It runs twice for each server in every proposal, on whatever a request
// lists. Comparing each with each took 47 s for 100,000.
func TestNewAccessTakesTimeInProportion(t *testing.T) {
	const n = 200_000
	from, to := make([]resource.Access, n), make([]resource.Access, n)
	for i := range n {
		from[i] = resource.Access{Kind: resource.AccessPath, Path: fmt.Sprintf("/from/%d", i)}
		to[i] = resource.Access{Kind: resource.AccessPath, Path: fmt.Sprintf("/to/%d", i)}
	}
	start := time.Now()
	if got := resource.NewAccess(from, to); len(got) != n {
		t.Errorf("NewAccess lists %d, want %d", len(got), n)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("NewAccess took %v for %d", took, n)
	}
}
