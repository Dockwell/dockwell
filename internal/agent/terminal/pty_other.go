// SPDX-License-Identifier: AGPL-3.0-only

//go:build !linux

package terminal

import (
	"errors"
	"os"
	"os/exec"
)

type pty struct {
	*os.File
}

func start(*exec.Cmd, uint16, uint16) (*pty, error) {
	return nil, errors.New("this agent opens terminals only on Linux")
}

func (*pty) resize(uint16, uint16) error { return nil }

func (*pty) redraw() {}

func isHangUp(err error) bool { return errors.Is(err, os.ErrClosed) }
