// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// LabelAdopted marks a container that was taken on, in what the client reads, and
// never on the container, with the name it came with. See Adopted.
const LabelAdopted = OwnPrefix + "adopted"

// Taken is what a container that was taken on is now.
type Taken struct {
	Project string `json:"project"`
	Service string `json:"service"`
	// Name is what it was called when it was taken on. A rollout renames it while
	// it stops it, so what it's called in Docker may not be that any more.
	Name string `json:"name,omitempty"`
}

// Adopted is the record of the containers a server took on as services. Docker
// can't change a running container's labels, and they are how everything in Dockwell
// finds a service's containers, so a client that holds this reads each of these as
// if it had the labels of what it is now: its list, its inspect and its filters
// alike. So nothing above the client needs to know, and a container isn't restarted
// to be taken on. It stays in force until the container goes, as a replace removes
// it, and then what runs has the labels of its own.
//
// It reads its file once and keeps it in memory, and writes each change to the file
// before it counts. A server that lost its file has lost its identity too, so no
// service would be placed on it.
type Adopted struct {
	// Path is the file. Empty keeps it in memory, as in tests.
	Path string

	mu     sync.Mutex
	loaded bool
	byID   map[string]Taken
}

func (a *Adopted) load() error {
	if a.loaded {
		return nil
	}
	a.byID = map[string]Taken{}
	if a.Path != "" {
		b, err := os.ReadFile(a.Path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return fmt.Errorf("can't read which containers were taken on: %w", err)
		default:
			if err := json.Unmarshal(b, &a.byID); err != nil {
				return fmt.Errorf("can't read which containers were taken on, from %s: %w. Restore it from a backup, or remove it to forget them", a.Path, err)
			}
		}
	}
	a.loaded = true
	return nil
}

func (a *Adopted) save(next map[string]Taken) error {
	if a.Path != "" {
		b, err := json.Marshal(next)
		if err != nil {
			return err
		}
		tmp := a.Path + ".tmp"
		if err := os.MkdirAll(filepath.Dir(a.Path), 0o700); err != nil {
			return fmt.Errorf("can't record a container taken on: %w", err)
		}
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return fmt.Errorf("can't record a container taken on: %w", err)
		}
		if err := os.Rename(tmp, a.Path); err != nil {
			return fmt.Errorf("can't record a container taken on: %w", err)
		}
	}
	a.byID = next
	return nil
}

// Take records the container id as what a service runs.
func (a *Adopted) Take(id string, t Taken) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.load(); err != nil {
		return err
	}
	next := maps.Clone(a.byID)
	next[id] = t
	return a.save(next)
}

// Forget lets go of the container id.
func (a *Adopted) Forget(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.load(); err != nil {
		return err
	}
	if _, ok := a.byID[id]; !ok {
		return nil
	}
	next := maps.Clone(a.byID)
	delete(next, id)
	return a.save(next)
}

// snapshot is the record now. Nil on a client with none, and for one it can't read,
// which reads as none, as its Take and Forget say why.
func (a *Adopted) snapshot() map[string]Taken {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.load() != nil {
		return nil
	}
	return a.byID
}

// asTaken gives labels those of what a container taken on is now, on a copy, as a
// list's labels may be shared, and name is what it's called in Docker, for one read
// before it was recorded.
func asTaken(labels map[string]string, t Taken, name string) map[string]string {
	out := make(map[string]string, len(labels)+5)
	maps.Copy(out, labels)
	out[LabelProject], out[LabelService], out[LabelNumber], out[LabelOneoff] = t.Project, t.Service, strconv.Itoa(1), "False"
	out[LabelAdopted] = cmp.Or(t.Name, strings.TrimPrefix(name, "/"))
	return out
}

// labelsPass reports labels that pass a filter's label= entries, as Docker reads them.
func labelsPass(labels map[string]string, filters []string) bool {
	for _, f := range filters {
		k, v, hasValue := strings.Cut(f, "=")
		if got, ok := labels[k]; !ok || hasValue && got != v {
			return false
		}
	}
	return true
}

// plainSummaryName is the name of a list's entry, without a link's.
func plainSummaryName(names []string) string {
	for _, n := range names {
		if !strings.Contains(strings.TrimPrefix(n, "/"), "/") {
			return n
		}
	}
	return ""
}

// listTaken puts what was taken on right in a list that passed filters: what's
// there reads as what it is now, what that's no longer true of goes, and what Docker
// left out for the labels it has on it comes in. It asks Docker again only when one
// could pass the filters, so a server with none taken on pays nothing.
func (c *Client) listTaken(list []ContainerSummary, filters map[string][]string, read func(map[string][]string, func(*ContainerSummary) error) error) ([]ContainerSummary, error) {
	taken := c.Adopted.snapshot()
	if len(taken) == 0 {
		return list, nil
	}
	seen := map[string]bool{}
	out := list[:0]
	for i := range list {
		s := list[i]
		if t, ok := taken[s.ID]; ok {
			seen[s.ID] = true
			s.Labels = asTaken(s.Labels, t, plainSummaryName(s.Names))
			if !labelsPass(s.Labels, filters["label"]) {
				continue
			}
		}
		out = append(out, s)
	}
	if len(filters["label"]) == 0 {
		return out, nil
	}
	var missing []string
	for id, t := range taken {
		if !seen[id] && labelsPass(asTaken(nil, t, ""), filters["label"]) {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	slices.Sort(missing)
	more := map[string][]string{"id": missing}
	for k, v := range filters {
		if k != "label" {
			more[k] = v
		}
	}
	err := read(more, func(s *ContainerSummary) error {
		s.Labels = asTaken(s.Labels, taken[s.ID], plainSummaryName(s.Names))
		if labelsPass(s.Labels, filters["label"]) {
			out = append(out, *s)
		}
		return nil
	})
	return out, err
}
