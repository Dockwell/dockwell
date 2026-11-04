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
	"syscall"
	"time"
)

// What a child may spend reading one file. The worst real shape of a file
// under MaxFile takes about 2 s and 44 MiB of heap. Memory is limited as
// RLIMIT_DATA, what it can write to, as Go reserves far more address space
// than it uses: under an RLIMIT_AS of 1 GiB it died with 28 MiB in use.
var (
	guardCPU     uint64 = 10 // seconds
	guardMemory  uint64 = 1 << 30
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
func UseGuard() {
	guardPath = "/proc/self/exe"
	if _, err := os.Stat(guardPath); err != nil {
		if exe, err := os.Executable(); err == nil {
			guardPath = exe
		}
	}
}

type guardRequest struct {
	File      []byte            `json:"file"`
	Variables map[string]string `json:"variables"`
	CPU       uint64            `json:"cpu"`
	Memory    uint64            `json:"memory"`
}

// guard reads the file in a child, and says only whether it finished within
// its limits. Parse then reads it again here for the result, whatever it is:
// a file that finishes in the child costs the same here, and there's no second
// form of a Stack or an error to carry across.
func guard(ctx context.Context, file []byte, variables map[string]string) error {
	if guardPath == "" {
		return nil
	}
	body, err := json.Marshal(guardRequest{file, variables, guardCPU, guardMemory})
	if err != nil {
		return fmt.Errorf("can't pass the Compose file to be read: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, guardTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, guardPath, GuardCommand)
	// Nothing of this process's own passes to it, such as a database's
	// address, and a crash's traces go nowhere, as they can hold what it read.
	cmd.Env = []string{"GOMAXPROCS=2"}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("can't start reading the Compose file: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("can't start reading the Compose file: %w", err)
	}
	go func() {
		_, _ = stdin.Write(body)
		_ = stdin.Close()
	}()
	err = cmd.Wait()
	// A signal is the CPU limit, as is the deadline for time, and exit status 2
	// is how Go ends when it runs out of memory. Any other is the child itself
	// failing, which says nothing about the file.
	var exit *exec.ExitError
	spent := errors.As(err, &exit) && (exit.ExitCode() < 0 || exit.ExitCode() == 2)
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("stopped reading the Compose file: %w", ctx.Err())
	case spent || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return invalid("reading the Compose file took more time or memory than Dockwell allows, so it stopped. " +
			"A real file doesn't: look for one very long word in a command, thousands of names in one place, or aliases repeated many times")
	}
	return fmt.Errorf("can't read the Compose file: %w", err)
}

// RunGuard is the child: it takes a request from in, sets its limits, then
// reads the file. A refusal is a finished read. Only running out of what it's
// allowed ends it any other way.
func RunGuard(in io.Reader) error {
	var req guardRequest
	if err := json.NewDecoder(in).Decode(&req); err != nil {
		return fmt.Errorf("can't read the request: %w", err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: req.CPU, Max: req.CPU}); err != nil {
		return fmt.Errorf("can't limit reading a Compose file to %d s of CPU: %w", req.CPU, err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_DATA, &syscall.Rlimit{Cur: req.Memory, Max: req.Memory}); err != nil {
		return fmt.Errorf("can't limit reading a Compose file to %d MiB: %w", req.Memory>>20, err)
	}
	_, _ = parse(req.File, req.Variables)
	return nil
}
