// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"maps"
	"strings"
)

// What a file may come to once its aliases are read, before anything else
// looks at it. The cap on a file means little if a short one can repeat a
// long string: the YAML library counts nodes, not bytes, so a file under the
// cap with 173,397 aliases of a 512 KiB string came to 87 GiB and took the
// control plane down, which a write token can do. Nor can a file nest without
// limit, as compose-go's canonical forms read it again at each level: 17 KiB
// nested 9,000 deep took 30 s.
const (
	// MaxExpanded is what a file's strings, names and values may come to. A
	// file under MaxFile with no aliases never reaches it.
	MaxExpanded = 512 << 10
	// MaxDepth is how deep a file may nest. Compose's own forms go about 8
	// deep.
	MaxDepth = 64
	// MaxName is how long a name may be, of a service, a variable or anything
	// else in a file. A library's errors repeat a name once for each mistake
	// under it, so a name of 64 KiB and 2,000 bad ports in a file of 87 KiB took
	// 888 MiB. With this, the same shape takes 118 MiB.
	MaxName = 255
	// nodeCost is what each value counts for besides its text, so that a
	// list of many small values costs what it takes to read. A value takes
	// at least 2 bytes of a file, so with a letter of text it comes to less
	// than twice what it took.
	nodeCost = 2
)

// dropExtensions removes x- extensions where they can't be anything else: at the
// top of a file, and directly in a service, a volume or a network. They are there
// for their anchors, which are read by now, and nothing looks at them after this,
// so one costs nothing however large, and needs no value for its variables.
// Those sections' own names can start with x- too, as a service's depends_on's
// can, and those stay.
func dropExtensions(raw map[string]any) {
	isExtension := func(k string, _ any) bool { return strings.HasPrefix(k, "x-") }
	maps.DeleteFunc(raw, isExtension)
	for _, section := range []string{"services", "volumes", "networks"} {
		entries, _ := raw[section].(map[string]any)
		for _, entry := range entries {
			if m, ok := entry.(map[string]any); ok {
				maps.DeleteFunc(m, isExtension)
			}
		}
	}
}

// measure refuses a file that comes to too much, or nests too deep.
func measure(raw map[string]any) error {
	spent := 0
	return measured(raw, 1, &spent)
}

func measured(v any, depth int, spent *int) error {
	if depth > MaxDepth {
		return invalid("the Compose file nests more than %d levels deep, which Dockwell doesn't read. Flatten what's deepest", MaxDepth)
	}
	*spent += nodeCost
	switch v := v.(type) {
	case string:
		*spent += len(v)
	case []any:
		for _, e := range v {
			if err := measured(e, depth+1, spent); err != nil {
				return err
			}
		}
	case map[string]any:
		for k, e := range v {
			if len(k) > MaxName {
				return invalid("the Compose file has a name of %d bytes, and Dockwell reads names of up to %d. Shorten it", len(k), MaxName)
			}
			*spent += len(k)
			if err := measured(e, depth+1, spent); err != nil {
				return err
			}
		}
	case map[any]any:
		for _, e := range v {
			if err := measured(e, depth+1, spent); err != nil {
				return err
			}
		}
	}
	if *spent > MaxExpanded {
		return invalid("the Compose file comes to more than %d KiB once its aliases are read, which Dockwell doesn't read. Repeat long values less often", MaxExpanded>>10)
	}
	return nil
}
