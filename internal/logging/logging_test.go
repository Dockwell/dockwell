// SPDX-License-Identifier: AGPL-3.0-only

package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNewFormats(t *testing.T) {
	t.Run("auto is JSON off a terminal", func(t *testing.T) {
		var buf bytes.Buffer
		logger, err := New(&buf, Options{Level: "info", Format: FormatAuto})
		if err != nil {
			t.Fatal(err)
		}
		logger.Info("control plane started", "addr", "127.0.0.1:7600")
		var rec map[string]any
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			t.Fatalf("auto format off a terminal is not JSON: %v\n%s", err, buf.String())
		}
		if rec["msg"] != "control plane started" || rec["addr"] != "127.0.0.1:7600" {
			t.Errorf("unexpected record: %v", rec)
		}
	})

	t.Run("text", func(t *testing.T) {
		var buf bytes.Buffer
		logger, err := New(&buf, Options{Level: "info", Format: FormatText})
		if err != nil {
			t.Fatal(err)
		}
		logger.Info("control plane started")
		if !strings.Contains(buf.String(), `msg="control plane started"`) {
			t.Errorf("text output = %q", buf.String())
		}
	})

	t.Run("level filters, case-insensitively", func(t *testing.T) {
		var buf bytes.Buffer
		logger, err := New(&buf, Options{Level: "WARN", Format: FormatJSON})
		if err != nil {
			t.Fatal(err)
		}
		logger.Info("hidden")
		logger.Warn("shown")
		if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "shown") {
			t.Errorf("level warn wrote %q", buf.String())
		}
	})
}

func TestNewRejectsUnknownValues(t *testing.T) {
	for _, opts := range []Options{
		{Level: "loud", Format: FormatText},
		{Level: "info+2", Format: FormatText},
		{Level: "", Format: FormatText},
		{Level: "info", Format: "yaml"},
	} {
		if _, err := New(&bytes.Buffer{}, opts); err == nil {
			t.Errorf("New(%+v) succeeded, want an error", opts)
		} else if !strings.Contains(err.Error(), "use ") {
			t.Errorf("error %q doesn't say which values are accepted", err)
		}
	}
}
