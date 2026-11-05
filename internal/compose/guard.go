// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// What a child may spend reading one file. The largest real shapes under
// MaxFile take 1 to 3 s of CPU and 198 MiB it can write to, and two run at once,
// so even a server of 1 GB has room. Memory is limited as RLIMIT_DATA, what it
// can write to, as Go reserves far more address space than it uses: under an
// RLIMIT_AS of 1 GiB it died with 28 MiB in use.
var (
	guardCPU     uint64 = 10 // seconds
	guardMemory  uint64 = 256 << 20
	guardTimeout        = 15 * time.Second
)

// GuardCommand is the hidden command the binary runs a child as.
const GuardCommand = "__read-compose"

// guardPath is the binary to run as a child, or empty to read in this
// process, as tests do. Serve sets it.
var guardPath string

// UseGuard makes Parse read each file in a limited child process first. It
// is what a process that takes files from others should do: reading one goes
// through a YAML library, compose-go and a shell-word splitter, none of them
// written for input that means harm, and each has had a short file that takes
// minutes or all the memory there is. A Go process can't be stopped or
// recover from running out of memory, so the only safe place to find out is
// another process. /proc/self/exe stays right across an upgrade that replaces
// the binary.
func UseGuard() { guardPath = "/proc/self/exe" }

// guardRequest is what a child reads, all of what Parse reads, so nothing the
// parent goes on to read gets there without the child having read it first.
type guardRequest struct {
	File      []byte            `json:"file"`
	Variables map[string]string `json:"variables"`
	CPU       uint64            `json:"cpu"`
	Memory    uint64            `json:"memory"`
	// Inspect asks it to say what the file says of itself, on its stdout.
	Inspect bool `json:"inspect,omitempty"`
}

// maxInspection is the most a child may say of a file. Every variable in a
// file of MaxFile comes to under 3 MiB.
const maxInspection = 8 << 20

// guard reads the file in a child, and says whether it finished within its
// limits and, when asked, what the file says of itself. Parse then reads it
// again here for the result, whatever it is: a file that finishes in the child
// costs the same here, and there's no second form of a Stack or an error to
// carry across. What the child says is its own reading of the file, which
// nothing here repeats, so only the child pays for it.
func guard(ctx context.Context, file []byte, variables map[string]string, asked bool) (Inspection, error) {
	if guardPath == "" {
		return Inspection{}, nil
	}
	body, err := json.Marshal(guardRequest{file, variables, guardCPU, guardMemory, asked})
	if err != nil {
		return Inspection{}, fmt.Errorf("can't pass the Compose file to be read: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, guardTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, guardPath, GuardCommand)
	// Nothing of this process's own passes to it, such as a database's
	// address. GOMEMLIMIT has it collect garbage before it reaches its limit.
	cmd.Env = []string{"GOMAXPROCS=2", fmt.Sprintf("GOMEMLIMIT=%d", guardMemory-guardMemory/4)}
	// Its first words say how it ended. Nothing else of what it writes is kept,
	// as a crash's traces can hold what it read.
	said := head{room: 256}
	cmd.Stderr = &said
	told := head{room: maxInspection}
	if asked {
		cmd.Stdout = &told
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Inspection{}, fmt.Errorf("can't start reading the Compose file: %w. This is about the server, not the file", err)
	}
	if err := cmd.Start(); err != nil {
		return Inspection{}, fmt.Errorf("can't start reading the Compose file: %w. This is about the server, not the file: dockwell needs /proc, and to run its own binary", err)
	}
	go func() {
		_, _ = stdin.Write(body)
		_ = stdin.Close()
	}()
	err = cmd.Wait()
	var exit *exec.ExitError
	failed := errors.As(err, &exit)
	switch {
	case err == nil && !asked:
		return Inspection{}, nil
	case err == nil && told.over:
		return Inspection{}, ErrCostly
	case err == nil:
		var seen Inspection
		if err := json.Unmarshal(told.b, &seen); err != nil {
			return Inspection{}, fmt.Errorf("can't read what the Compose file's reader said: %w. This is about the server, not the file", err)
		}
		return seen, nil
	case ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded):
		return Inspection{}, stopped(ctx)
	case failed && exit.ExitCode() == 1:
		// RunGuard's own error, which holds nothing it read.
		return Inspection{}, fmt.Errorf("can't read the Compose file: %s. This is about the server, not the file", strings.TrimSpace(said.String()))
	case failed && strings.Contains(said.String(), "new OS thread"):
		return Inspection{}, errors.New("can't read the Compose file, as the server can't start another thread. This is about the server, not the file: check its limit on processes")
	case failed || errors.Is(ctx.Err(), context.DeadlineExceeded):
		// A signal is the CPU limit, and any other exit a crash, as Go ends when
		// it runs out of memory, on what the file made it do.
		return Inspection{}, ErrCostly
	}
	return Inspection{}, fmt.Errorf("can't read the Compose file: %w", err)
}

// head keeps the first room bytes written to it, and notes any it doesn't.
type head struct {
	b    []byte
	room int
	over bool
}

func (h *head) Write(p []byte) (int, error) {
	take := min(h.room-len(h.b), len(p))
	h.b = append(h.b, p[:take]...)
	h.over = h.over || take < len(p)
	return len(p), nil
}

func (h *head) String() string { return string(h.b) }

// RunGuard is the child: it takes a request from in, sets its limits, then
// reads the file, saying on out what the file says of itself when asked. A
// refusal is a finished read. Only running out of what it's allowed ends it any
// other way. It asks the kernel to take it first when memory runs short, rather
// than anything else on the server.
func RunGuard(in io.Reader, out io.Writer) error {
	var req guardRequest
	if err := json.NewDecoder(in).Decode(&req); err != nil {
		return fmt.Errorf("can't read the request: %w", err)
	}
	_ = os.WriteFile("/proc/self/oom_score_adj", []byte("1000"), 0)
	if err := limit(syscall.RLIMIT_CPU, req.CPU); err != nil {
		return fmt.Errorf("can't limit its CPU to %d s: %w", req.CPU, err)
	}
	if err := limit(syscall.RLIMIT_DATA, req.Memory); err != nil {
		return fmt.Errorf("can't limit its memory to %d MiB: %w", req.Memory>>20, err)
	}
	ctx := context.Background()
	if req.Inspect {
		if err := json.NewEncoder(out).Encode(inspect(ctx, req.File, req.Variables)); err != nil {
			return fmt.Errorf("can't say what it read: %w", err)
		}
	}
	_, _ = parse(ctx, req.File, req.Variables)
	return nil
}

// limit sets a resource limit, or keeps a lower one the server has already set.
func limit(resource int, to uint64) error {
	var now syscall.Rlimit
	if err := syscall.Getrlimit(resource, &now); err != nil {
		return err
	}
	to = min(to, now.Max)
	return syscall.Setrlimit(resource, &syscall.Rlimit{Cur: to, Max: to})
}
