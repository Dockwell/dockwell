// SPDX-License-Identifier: AGPL-3.0-only

// Package logging builds the structured logger every dockwell command uses.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"golang.org/x/term"
)

// Format selects how log records are written.
type Format string

// The supported formats. Auto writes text to a terminal and JSON elsewhere,
// so `journalctl` and log drains get structured records.
const (
	FormatAuto Format = "auto"
	FormatText Format = "text"
	FormatJSON Format = "json"
)

// levels are the only accepted names. slog would also take offsets such as
// "info+2", which the flag's help doesn't promise.
var levels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// Options configures New.
type Options struct {
	// Level is one of debug, info, warn or error.
	Level string
	// Format is auto, text or json.
	Format Format
}

// New returns a logger writing to w. It fails on an unknown level or format,
// naming the values that are accepted.
func New(w io.Writer, opts Options) (*slog.Logger, error) {
	level, ok := levels[strings.ToLower(opts.Level)]
	if !ok {
		return nil, fmt.Errorf("unknown log level %q: use debug, info, warn or error", opts.Level)
	}

	format := Format(strings.ToLower(string(opts.Format)))
	if format == "" || format == FormatAuto {
		format = FormatJSON
		if isTerminal(w) {
			format = FormatText
		}
	}

	handlerOpts := &slog.HandlerOptions{Level: level}
	switch format {
	case FormatText:
		return slog.New(slog.NewTextHandler(w, handlerOpts)), nil
	case FormatJSON:
		return slog.New(slog.NewJSONHandler(w, handlerOpts)), nil
	default:
		return nil, fmt.Errorf("unknown log format %q: use auto, text or json", opts.Format)
	}
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
