// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

func TestReadLevel(t *testing.T) {
	const (
		none  = dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
		debug = dockwellv1.LogLevel_LOG_LEVEL_DEBUG
		info  = dockwellv1.LogLevel_LOG_LEVEL_INFO
		warn  = dockwellv1.LogLevel_LOG_LEVEL_WARN
		err   = dockwellv1.LogLevel_LOG_LEVEL_ERROR
	)
	for line, want := range map[string]dockwellv1.LogLevel{
		// Structured loggers say so in a field.
		`{"level":"error","msg":"connection reset"}`:                   err,
		`{"time":"2026-11-04T14:19:58Z","severity":"WARNING","m":"x"}`: warn,
		`{"lvl":"info"}`:                                        info,
		`{"level":30,"msg":"pino"}`:                             info,
		`{"level":50,"msg":"pino"}`:                             err,
		`{"level":10,"msg":"pino"}`:                             debug,
		`time=2026-11-04T14:19:58Z level=warn msg="slow query"`: warn,
		`ts=1 level="error" msg=x`:                              err,
		`severity=debug msg=x`:                                  debug,
		// Words in capitals, where they stand alone.
		`2026-11-04 14:19:58 ERROR failed to connect`:  err,
		`[2026-11-04 14:19:58] WARN: disk is 90% full`: warn,
		`INFO  server listening on :8080`:              info,
		`14:19:58 | FATAL | can't bind`:                err,
		`DEBUG cache miss`:                             debug,
		`ERROR:root:something broke`:                   err,
		// Lowercase only where it plainly names a level.
		`2026/11/04 14:19:58 [error] 29#29: *1 connect() failed`: err,
		`[warn] worker exiting`:                                  warn,
		`<notice> ready`:                                         info,
		`error: no such file`:                                    err,
		`panic: runtime error: index out of range`:               err,
		`warning: deprecated flag`:                               warn,
		// klog and glog.
		`E0102 15:04:05.000000 1 server.go:42] broken`: err,
		`W0102 15:04:05.000000 1 server.go:42] slow`:   warn,
		`I0102 15:04:05.000000 1 server.go:42] fine`:   info,
		// A coloured logger's escapes don't hide its level.
		"\x1b[31mERROR\x1b[0m something failed": err,
		"\x1b[33m[warn]\x1b[0m slow":            warn,
		// Words about errors aren't levels.
		`GET /v1/errors 200 9 ms`:                         none,
		`GET /ERROR 200`:                                  none,
		`no error found`:                                  none,
		`error rate is 0.1%`:                              none,
		`processed 10 items without a warning`:            none,
		`fetching https://example.test/INFO/page`:         none,
		`user error-prone@example.test signed in`:         none,
		`GET /healthz 200 1 ms`:                           none,
		``:                                                none,
		"\x00\x01\x02 binary junk \xff\xfe":               none,
		`{"msg":"error in the message, none in a field"}`: none,
	} {
		if got := ReadLevel([]byte(line)); got != want {
			t.Errorf("ReadLevel(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestContinuationLines(t *testing.T) {
	for line, want := range map[string]bool{
		"    at com.acme.Main.run(Main.java:42)": true,
		"\tat com.acme.Main.run(Main.java:42)":   true,
		"  File \"app.py\", line 3, in <module>": true,
		"Caused by: java.io.IOException":         true,
		"goroutine 1 [running]:":                 true,
		"new line":                               false,
		"":                                       false,
		"    ":                                   false,
	} {
		if got := isContinuation([]byte(line)); got != want {
			t.Errorf("isContinuation(%q) = %v, want %v", line, got, want)
		}
	}
}

// ReadLevel runs on every line a live stream keeps, so it must not allocate.
func TestReadLevelDoesntAllocate(t *testing.T) {
	line := []byte(`2026-11-04 14:19:58 INFO GET /v1/products?category=shoes 200 in 14 ms`)
	if n := testing.AllocsPerRun(100, func() { ReadLevel(line) }); n != 0 {
		t.Errorf("ReadLevel allocates %v times per line", n)
	}
}
