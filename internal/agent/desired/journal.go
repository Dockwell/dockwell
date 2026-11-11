// SPDX-License-Identifier: AGPL-3.0-only

package desired

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dockwell/dockwell/internal/resource"
)

// JournalFile is where the agent writes down what it did while the control plane
// was away, in Folder.
const JournalFile = "journal"

// JournalCap is how many entries the journal keeps, the oldest going first.
const JournalCap = 500

// What an entry says the agent did.
const (
	// Made says it made the resource again, as it was gone.
	Made = "made"
	// Started says it started the resource, which Docker couldn't.
	Started = "started"
	// Waiting says it couldn't put the resource back, and why: it waits for the
	// control plane.
	Waiting = "waiting"
)

// Entry is one thing the agent did, or couldn't do, while the control plane was
// away. It holds no value and no spec.
type Entry struct {
	// ID is the boot's ID and a sequence, so it means one thing however often
	// it's sent.
	ID     string       `json:"id"`
	Ref    resource.Ref `json:"ref"`
	What   string       `json:"what"`
	Reason string       `json:"reason,omitempty"`
	At     time.Time    `json:"at"`
	// Target is the existing container an unfinished start concerns. It is only
	// kept locally; the hand-back sends neither this nor a spec.
	Target string `json:"target,omitempty"`
}

// journal is what the file holds.
type journal struct {
	Boot    string  `json:"boot"`
	Seq     uint64  `json:"seq"`
	Entries []Entry `json:"entries"`
	Dropped int     `json:"dropped"`
	// DroppedTo is the ID of the newest entry dropped, which names the count, so
	// the count sent again is one event.
	DroppedTo string `json:"dropped_to,omitempty"`
	// Said is the wait the control plane last heard of each resource, by the
	// resource, so a wait that goes on isn't written down again each time the
	// control plane goes away.
	Said map[string]string `json:"said,omitempty"`
	// Doing is written before Docker is changed. A restart can finish the same
	// entry, identified on a new container by its reconciliation label.
	Doing map[string]Entry `json:"doing,omitempty"`
}

// Journal is what the agent did while the control plane was away, until the
// control plane says it has it (ADR 0054).
type Journal struct {
	path string
	// added signals an entry written down, so one written while a stream is up,
	// as by work that began while the control plane was away, goes on that stream.
	added chan struct{}

	mu sync.Mutex
	j  journal
}

// Added signals when an entry is written down.
func (j *Journal) Added() <-chan struct{} { return j.added }

// bootIDFile names this boot of the server. A test stands in another boot.
var bootIDFile = "/proc/sys/kernel/random/boot_id"

// OpenJournal opens the journal in dir, the agent's data folder.
func OpenJournal(dir string) (*Journal, error) {
	folder := filepath.Join(dir, Folder)
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return nil, fmt.Errorf("can't make %s for what this server does on its own: %w", folder, err)
	}
	out := &Journal{path: filepath.Join(folder, JournalFile), added: make(chan struct{}, 1)}
	b, err := os.ReadFile(out.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &out.j); err != nil {
			// Damaged: what it held is lost, at least one entry, and the next
			// hand-back says so.
			out.j = journal{Dropped: 1}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("can't read what this server did on its own, in %s: %w", out.path, err)
	}
	// A new boot numbers its entries afresh, under its own ID.
	if boot := bootID(); boot != out.j.Boot {
		out.j.Boot, out.j.Seq = boot, 0
	}
	return out, nil
}

// bootID names this boot of the server, or, where nothing does, this start.
func bootID() string {
	if b, err := os.ReadFile(bootIDFile); err == nil {
		if id := strings.ReplaceAll(strings.TrimSpace(string(b)), "-", ""); id != "" {
			return id
		}
	}
	r := make([]byte, 16)
	_, _ = rand.Read(r)
	return hex.EncodeToString(r)
}

// Add writes down an entry, unless it's a wait the last entry for the resource
// says already, or, with none, the last the control plane heard: a wait isn't
// written down each minute it goes on, nor each time the control plane goes away.
func (j *Journal) Add(ref resource.Ref, what, reason string, at time.Time) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	pending := false
	for i := len(j.j.Entries) - 1; i >= 0; i-- {
		if e := j.j.Entries[i]; e.Ref == ref {
			if e.What == what && e.Reason == reason {
				return nil
			}
			pending = true
			break
		}
	}
	key := ref.String()
	if said, ok := j.j.Said[key]; what == Waiting && !pending && ok && said == reason {
		return nil
	}
	delete(j.j.Said, key)
	j.j.Seq++
	j.j.Entries = append(j.j.Entries, Entry{ID: fmt.Sprintf("%s-%d", j.j.Boot, j.j.Seq), Ref: ref, What: what, Reason: reason, At: at.UTC()})
	if over := len(j.j.Entries) - JournalCap; over > 0 {
		j.j.DroppedTo = j.j.Entries[over-1].ID
		j.j.Entries = append([]Entry(nil), j.j.Entries[over:]...)
		j.j.Dropped += over
	}
	select {
	case j.added <- struct{}{}:
	default:
	}
	return j.save()
}

// Pending is what hasn't been handed back, how many entries were dropped before
// it, and the ID of the newest of those, which names the count.
func (j *Journal) Pending() ([]Entry, int, string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]Entry(nil), j.j.Entries...), j.j.Dropped, j.j.DroppedTo
}

// Begin durably records an action before it touches Docker. Retrying an action
// keeps its ID and time, including after a failed completion write or a restart.
func (j *Journal) Begin(ref resource.Ref, what, reason, target string, at time.Time) (Entry, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	key := ref.String()
	e, ok := j.j.Doing[key]
	if !ok {
		if len(j.j.Doing) >= JournalCap {
			return Entry{}, errors.New("too many unfinished local actions; reconnect the agent before trying again")
		}
		j.j.Seq++
		e = Entry{ID: fmt.Sprintf("%s-%d", j.j.Boot, j.j.Seq), Ref: ref, What: what, Reason: reason, Target: target, At: at.UTC()}
		if j.j.Doing == nil {
			j.j.Doing = map[string]Entry{}
		}
		j.j.Doing[key] = e
	}
	return e, j.save()
}

// Doing is the actions whose outcome has not yet been written down.
func (j *Journal) Doing() []Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Entry, 0, len(j.j.Doing))
	for key := range j.j.Doing {
		out = append(out, j.j.Doing[key])
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Action is the unfinished local action for ref, if any.
func (j *Journal) Action(ref resource.Ref) (Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.j.Doing[ref.String()]
	return e, ok
}

// Finish records an action's outcome under its reserved ID and original time.
// If saving fails, the old durable intent still identifies that same outcome on
// the next start, and the in-memory entry can already be handed back.
func (j *Journal) Finish(id, what, reason string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	for key := range j.j.Doing {
		e := j.j.Doing[key]
		if e.ID != id {
			continue
		}
		delete(j.j.Doing, key)
		e.What, e.Reason, e.Target = what, reason, ""
		delete(j.j.Said, key)
		j.j.Entries = append(j.j.Entries, e)
		if over := len(j.j.Entries) - JournalCap; over > 0 {
			j.j.DroppedTo = j.j.Entries[over-1].ID
			j.j.Entries = append([]Entry(nil), j.j.Entries[over:]...)
			j.j.Dropped += over
		}
		select {
		case j.added <- struct{}{}:
		default:
		}
		return j.save()
	}
	return nil
}

// Heard drops what the control plane says it has: the entries up to and including
// last, and the count of those dropped before them. With last empty, it had none of
// the entries, as it was sent none, but it has the count.
func (j *Journal) Heard(last string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	n := 0
	if last != "" {
		n = slices.IndexFunc(j.j.Entries, func(e Entry) bool { return e.ID == last }) + 1
		if n == 0 {
			return nil
		}
	}
	for i := range j.j.Entries[:n] {
		e := &j.j.Entries[i]
		key := e.Ref.String()
		if e.What != Waiting {
			delete(j.j.Said, key)
			continue
		}
		if j.j.Said == nil {
			j.j.Said = map[string]string{}
		}
		if _, ok := j.j.Said[key]; ok || len(j.j.Said) < JournalCap {
			j.j.Said[key] = e.Reason
		}
	}
	j.j.Entries = append([]Entry(nil), j.j.Entries[n:]...)
	j.j.Dropped, j.j.DroppedTo = 0, ""
	return j.save()
}

func (j *Journal) save() error {
	b, err := json.Marshal(j.j)
	if err != nil {
		return err
	}
	return writeDurably(j.path, b)
}
