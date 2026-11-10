// SPDX-License-Identifier: AGPL-3.0-only

package task

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

const (
	// keptRuns is how many of a task's runs the agent keeps unread (ADR 0053).
	keptRuns = 100
	// readRuns and readBytes bound one read, so a message stays small.
	readRuns  = 20
	readBytes = 2 << 20
)

// runLog keeps runs on the agent's disk until the control plane reads them. Each
// record is a file named by a sequence number: a run that's going, and again when
// it ends, under the same run ID, the second taking the first's place. A read
// returns what's past a cursor and lets go of what's at or before it, as the
// control plane has that.
type runLog struct {
	dir     string
	mu      sync.Mutex
	seq     uint64
	entries []entry // by seq
	dropped int64
}

type entry struct {
	seq  uint64
	run  string
	task string
	size int64
}

func (l *runLog) file(seq uint64) string {
	return filepath.Join(l.dir, fmt.Sprintf("%016d.json", seq))
}

// load reads what an earlier start left.
func (l *runLog) load() error {
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	names, err := os.ReadDir(l.dir)
	if err != nil {
		return err
	}
	for _, n := range names {
		seq, err := strconv.ParseUint(strings.TrimSuffix(n.Name(), ".json"), 10, 64)
		if err != nil || !strings.HasSuffix(n.Name(), ".json") {
			continue
		}
		r, size, err := l.readFile(seq)
		if err != nil {
			// A record cut short by a crash is of no use, and only in the way.
			_ = os.Remove(l.file(seq))
			continue
		}
		l.entries = append(l.entries, entry{seq: seq, run: r.GetRun().GetId(), task: resourcepb.FromRef(r.GetTask()).String(), size: size})
		l.seq = max(l.seq, seq)
	}
	slices.SortFunc(l.entries, func(a, b entry) int { return int(a.seq - b.seq) }) //nolint:gosec // sequence numbers are far apart from overflowing
	return nil
}

func (l *runLog) readFile(seq uint64) (*agentv1.AgentTaskRun, int64, error) {
	b, err := os.ReadFile(l.file(seq))
	if err != nil {
		return nil, 0, err
	}
	r := &agentv1.AgentTaskRun{}
	if err := protojson.Unmarshal(b, r); err != nil {
		return nil, 0, err
	}
	return r, int64(len(b)), nil
}

// put keeps a run as it is now, in place of what it was, and lets go of a task's
// oldest past keptRuns.
func (l *runLog) put(ref resource.Ref, r *agentv1.AgentTaskRun) error {
	b, err := protojson.Marshal(r)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	tmp := l.file(l.seq) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.file(l.seq)); err != nil {
		return err
	}
	task, run := ref.String(), r.GetRun().GetId()
	l.entries = slices.DeleteFunc(l.entries, func(e entry) bool {
		if e.run == run {
			_ = os.Remove(l.file(e.seq))
			return true
		}
		return false
	})
	l.entries = append(l.entries, entry{seq: l.seq, run: run, task: task, size: int64(len(b))})
	count := 0
	for _, e := range l.entries {
		if e.task == task {
			count++
		}
	}
	for i := 0; count > keptRuns && i < len(l.entries); {
		if l.entries[i].task != task {
			i++
			continue
		}
		_ = os.Remove(l.file(l.entries[i].seq))
		l.entries = slices.Delete(l.entries, i, i+1)
		l.dropped++
		count--
	}
	return nil
}

// read returns what's past after, oldest first, and lets go of what's at or before
// it, which the control plane has.
func (l *runLog) read(after string) (*agentv1.TaskRunsResult, error) {
	var from uint64
	if after != "" {
		n, err := strconv.ParseUint(after, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q isn't where a read of task runs ended", after)
		}
		from = n
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = slices.DeleteFunc(l.entries, func(e entry) bool {
		if e.seq <= from {
			_ = os.Remove(l.file(e.seq))
			return true
		}
		return false
	})
	out := &agentv1.TaskRunsResult{Cursor: after, Dropped: l.dropped}
	var size int64
	for i, e := range l.entries {
		if len(out.GetRuns()) == readRuns || len(out.GetRuns()) > 0 && size+e.size > readBytes {
			out.More = i < len(l.entries)
			break
		}
		r, n, err := l.readFile(e.seq)
		if err != nil {
			return nil, err
		}
		out.Runs = append(out.Runs, r)
		size += n
		out.Cursor = strconv.FormatUint(e.seq, 10)
	}
	if from > 0 || after == "" {
		// The control plane has the count once it has read past it.
		l.dropped = 0
	}
	return out, nil
}

// waiting says whether a run is kept that the control plane hasn't read.
func (l *runLog) waiting() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries) > 0
}
