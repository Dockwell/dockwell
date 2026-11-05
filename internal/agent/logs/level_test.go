// SPDX-License-Identifier: AGPL-3.0-only

package logs

import (
	"testing"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
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

// What common runtimes print as they crash, a line at a time, with the level each
// line should read as once those before it are read: a trace's frames take its
// level, and blank lines don't break it.
func TestReadsHowRuntimesCrash(t *testing.T) {
	const (
		none = dockwellv1.LogLevel_LOG_LEVEL_UNSPECIFIED
		info = dockwellv1.LogLevel_LOG_LEVEL_INFO
		warn = dockwellv1.LogLevel_LOG_LEVEL_WARN
		bad  = dockwellv1.LogLevel_LOG_LEVEL_ERROR
	)
	type line struct {
		text string
		want dockwellv1.LogLevel
	}
	for _, tc := range []struct {
		runtime string
		lines   []line
	}{
		{"Python", []line{
			{"starting worker", none},
			{"Traceback (most recent call last):", bad},
			{`  File "/app/main.py", line 3, in <module>`, bad},
			{"    run()", bad},
			{"ValueError: no template for pt-BR", bad},
			{"", bad},
			{"During handling of the above exception, another exception occurred:", bad},
			{"", bad},
			{"Traceback (most recent call last):", bad},
			{`  File "/app/main.py", line 9, in <module>`, bad},
			{"KeyError: 'id'", bad},
			{"sqlalchemy.exc.OperationalError: could not connect to server", bad},
			{"ModuleNotFoundError: No module named 'flask'", bad},
			{"ready", none},
			{"/app/main.py:3: DeprecationWarning: this goes away", warn},
			{"Exception: something broke", bad},
		}},
		{"Node", []line{
			{"listening on 3000", none},
			{"TypeError: Cannot read properties of undefined (reading 'id')", bad},
			{"    at handler (/app/index.js:3:9)", bad},
			{"    at process.processTicksAndRejections (node:internal/process/task_queues:95:5) {", bad},
			{"  code: '2BP01',", bad},
			{"}", bad},
			{"", bad},
			{"Node.js v20.11.0", bad},
			{"ready", none},
			{"Uncaught Exception: boom", bad},
			{"Error: ENOENT: no such file or directory, open '/app/config.json'", bad},
			{"UnhandledPromiseRejectionWarning: Error: boom", bad},
			{"ReferenceError: x is not defined", bad},
			{"npm ERR! code ELIFECYCLE", bad},
			{"npm ERR! errno 1", bad},
			{"npm error code 1", bad},
			{"npm WARN deprecated left-pad@1.0.0", warn},
			{"error Command failed with exit code 1.", bad},
		}},
		{"Go", []line{
			{"listening on :8080", none},
			{"panic: runtime error: index out of range [3] with length 2", bad},
			{"", bad},
			{"goroutine 1 [running]:", bad},
			{"main.handle(0x1, 0x2)", bad},
			{"\t/app/main.go:12 +0x1d", bad},
			{"created by main.main in goroutine 1", bad},
			{"exit status 2", bad},
			{"ready", none},
			{"fatal error: all goroutines are asleep - deadlock!", bad},
			{"[signal SIGSEGV: segmentation violation code=0x1 addr=0x0 pc=0x45]", bad},
			{"failed to connect to db: timeout", bad},
			{"exit status 0", none},
		}},
		{"Java", []line{
			{"Started Application in 2.3 seconds", none},
			{`Exception in thread "main" java.lang.NullPointerException`, bad},
			{"\tat com.acme.Main.run(Main.java:42)", bad},
			{"Caused by: java.net.ConnectException: Connection refused", bad},
			{"\t... 12 more", bad},
			{"java.lang.IllegalStateException: Failed to load ApplicationContext", bad},
			{"org.postgresql.util.PSQLException: FATAL: password authentication failed", bad},
			{"java.lang.OutOfMemoryError", bad},
			{"SEVERE: Servlet.service() threw exception", bad},
		}},
		{"PHP", []line{
			{"Server running", none},
			{"PHP Fatal error:  Uncaught Error: Call to undefined function foo() in /app/x.php:3", bad},
			{"Stack trace:", bad},
			{"#0 /app/x.php(3): foo()", bad},
			{"#1 {main}", bad},
			{"  thrown in /app/x.php on line 3", bad},
			{"PHP Warning:  Undefined variable $x in /app/x.php on line 3", warn},
			{"PHP Notice:  Undefined index: a in /app/x.php on line 3", info},
			{"PHP Parse error:  syntax error, unexpected end of file", bad},
			{"Fatal error: Allowed memory size exhausted", bad},
		}},
		{"Ruby", []line{
			{"Puma starting", none},
			{"/app/x.rb:1:in 'foo': boom (RuntimeError)", bad},
			{"\tfrom /app/x.rb:5:in '<main>'", bad},
			{"ActionController::RoutingError (No route matches):", bad},
			{"E, [2026-11-05T01:02:03.123] ERROR -- : boom", bad},
		}},
		{"Rust", []line{
			{"listening", none},
			{"thread 'main' panicked at src/main.rs:2:5:", bad},
			{"called `Option::unwrap()` on a `None` value", none},
			{"note: run with `RUST_BACKTRACE=1` environment variable to display a backtrace", none},
			{"error[E0425]: cannot find value `x` in this scope", bad},
			{"warning: unused variable: `y`", warn},
		}},
		{"C", []line{
			{"started", none},
			{"Segmentation fault (core dumped)", bad},
			{"Aborted (core dumped)", bad},
			{"Bus error", bad},
			{"Illegal instruction", bad},
			{"Killed", bad},
			{"terminate called after throwing an instance of 'std::runtime_error'", bad},
			{"  what():  boom", bad},
			{"double free or corruption (out)", bad},
			{"*** stack smashing detected ***: terminated", bad},
			{"/app/server: error while loading shared libraries: libfoo.so: cannot open shared object file", bad},
			{"exec /app/server: exec format error", bad},
		}},
		{"what isn't a crash", []line{
			{"GET /v1/errors 200 9 ms", none},
			{"GET /search?q=traceback 200", none},
			{"processed TypeError count=0", none},
			{"Killed 3 idle workers", none},
			{"failed", none},
			{"failure rate is 0.1%", none},
			{"exit status", none},
			{"at 12:00 the job runs", none},
			{"UserError", none},
		}},
	} {
		t.Run(tc.runtime, func(t *testing.T) {
			st := &sourceState{}
			for _, l := range tc.lines {
				if got := lineLevel(st, &docker.LogEntry{Stream: docker.Stdout, Text: []byte(l.text)}); got != l.want {
					t.Errorf("%q = %v, want %v", l.text, got, l.want)
				}
			}
		})
	}
}

func TestContinuationLines(t *testing.T) {
	for line, want := range map[string]bool{
		"    at com.acme.Main.run(Main.java:42)": true,
		"\tat com.acme.Main.run(Main.java:42)":   true,
		"  File \"app.py\", line 3, in <module>": true,
		"Caused by: java.io.IOException":         true,
		"goroutine 1 [running]:":                 true,
		"main.handle(0x1, 0x2)":                  true,
		"#0 /app/x.php(3): foo()":                true,
		"created by main.main in goroutine 1":    true,
		"new line":                               false,
		"GET /v1/products(1) 200":                false,
		"#hashtag here":                          false,
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
	for _, line := range [][]byte{line, []byte("Traceback (most recent call last):"), []byte("java.lang.IllegalStateException: boom"), []byte("nothing of note in this line at all")} {
		if n := testing.AllocsPerRun(100, func() { ReadLevel(line) }); n != 0 {
			t.Errorf("ReadLevel(%q) allocates %v times per line", line, n)
		}
	}
}
