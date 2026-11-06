// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// child builds what the control plane runs as a child, once for all the tests.
var child = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "dockwell-guardchild")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "guardchild")
	cmd := exec.Command("go", "build", "-o", path, "./testdata/guardchild")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("can't build the child: %w: %s", err, out)
	}
	return path, nil
})

func TestMain(m *testing.M) {
	code := m.Run()
	if path, err := child(); err == nil {
		_ = os.RemoveAll(filepath.Dir(path))
	}
	os.Exit(code)
}

func guarded(t *testing.T) {
	t.Helper()
	path, err := child()
	if err != nil {
		t.Fatal(err)
	}
	cpu, memory, timeout := guardCPU, guardMemory, guardTimeout
	guardPath = path
	t.Cleanup(func() { guardPath, guardCPU, guardMemory, guardTimeout = "", cpu, memory, timeout })
}

// One long word goes through a quadratic splitter: 2 MiB of it took 394 s.
func costly() ([]byte, map[string]string) {
	return []byte("services:\n  web:\n    image: nginx:1\n    command: ${A}${A}\n"), map[string]string{"A": strings.Repeat("x", 1<<20-1)}
}

// A child that reads a file changes nothing about the answer.
func TestAGuardedFileReadsTheSame(t *testing.T) {
	guarded(t)
	s, err := Parse(t.Context(), []byte("services:\n  web:\n    image: nginx:1.27\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Services) != 1 || s.Services[0].Name != "web" {
		t.Errorf("services = %v, want web", s.Services)
	}
	_, err = Parse(t.Context(), []byte("services:\n  web:\n    build: .\n"), nil)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "build") {
		t.Errorf("a refused file = %v, want the file's own refusal", err)
	}
}

// What a file costs the child can't cost the control plane. Each limit has to
// be what stopped it, as a child that dies at once reads the same to Parse.
func TestAGuardStopsAFileThatCostsTooMuch(t *testing.T) {
	file, vars := costly()
	for name, c := range map[string]struct {
		limit         func()
		atLeast, most time.Duration
	}{
		"CPU": {func() { guardCPU = 1 }, 300 * time.Millisecond, 8 * time.Second},
		// With the other limits out of the way, so this is what stops it.
		"memory": {func() { guardMemory, guardCPU, guardTimeout = 128<<20, 600, 10*time.Minute }, 0, 60 * time.Second},
		"time":   {func() { guardTimeout = 500 * time.Millisecond }, 400 * time.Millisecond, 10 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			guarded(t)
			c.limit()
			// A file that costs little passes under the same limit, so the
			// limit isn't just stopping the child from starting.
			if _, err := guard(t.Context(), Input{File: []byte("services:\n  web:\n    image: nginx:1.27\n")}, false); err != nil {
				t.Fatalf("a small file under this limit: %v", err)
			}
			start := time.Now()
			_, err := guard(t.Context(), Input{File: file, Variables: vars}, false)
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "more time or memory") {
				t.Errorf("guard = %.300v, want it refused for what it costs", err)
			}
			if took := time.Since(start); took < c.atLeast || took > c.most {
				t.Errorf("took %v, want %v to %v, so something else stopped it", took, c.atLeast, c.most)
			}
		})
	}
}

// The limits are for files made to cost more. The largest real shapes pass.
func TestAGuardTakesTheLargestRealFiles(t *testing.T) {
	guarded(t)
	build := func(head string, each func(i int) string) string {
		var b strings.Builder
		b.WriteString(head)
		for i := 0; b.Len() < MaxFile-64; i++ {
			b.WriteString(each(i))
		}
		return b.String()
	}
	for name, file := range map[string]string{
		"values": build("services:\n  web:\n    image: nginx:1\n    command: [a", func(int) string { return ",a" }) + "]\n",
		"names": build("services:\n  web:\n    image: nginx:1\n    environment:\n",
			func(i int) string { return "      K" + strconv.Itoa(i) + ": v\n" }),
		"services": build("services:\n",
			func(i int) string { return "  s" + strconv.Itoa(i) + ": {image: 'nginx:1'}\n" }),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := guard(t.Context(), Input{File: []byte(file)}, false); err != nil {
				t.Errorf("a file of %d KiB: %.300v", len(file)>>10, err)
			}
		})
	}
}

// Someone who gives up stops the work, and their turn is free for others.
func TestLeavingStopsTheChild(t *testing.T) {
	guarded(t)
	file, vars := costly()
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, err := Parse(ctx, file, vars)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalid) {
		t.Errorf("Parse = %v, want it stopped, not refused", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v to stop", took)
	}
	if len(parsing) != 0 {
		t.Errorf("%d turns still held", len(parsing))
	}
}

// A child that can't do its job says nothing about the file, so it doesn't
// blame it, and nothing is read in this process either.
func TestAChildThatFailsStopsTheRead(t *testing.T) {
	for name, path := range map[string]string{"can't start": "/nonexistent/dockwell", "fails": "/bin/false"} {
		t.Run(name, func(t *testing.T) {
			guarded(t)
			guardPath = path
			_, err := Parse(t.Context(), []byte("services:\n  web:\n    image: nginx:1.27\n"), nil)
			if err == nil || errors.Is(err, ErrInvalid) {
				t.Errorf("Parse = %v, want an error that isn't the file's fault", err)
			}
		})
	}
}

// The name of a service is repeated in a library's errors, once for each mistake
// under it, so a file of 87 KiB took 888 MiB and sent 128 MiB to its caller.
func TestALongNameCantMultiplyItself(t *testing.T) {
	bad := strings.TrimSuffix(strings.Repeat("{target: x},", 2000), ",")
	file := func(name int) []byte {
		return []byte("services:\n  " + strings.Repeat("a", name) + ":\n    image: nginx:1\n    ports: [" + bad + "]\n")
	}
	_, err := parse(t.Context(), Input{File: file(64 << 10)})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "a name of 65536 bytes") {
		t.Errorf("a long name = %.300v, want it refused for its length", err)
	}
	// At the limit, what the file's mistakes come to doesn't reach the caller.
	_, err = parse(t.Context(), Input{File: file(MaxName)})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Parse = %.300v, want it refused", err)
	}
	if got := len(err.Error()); got > maxMessage+64 {
		t.Errorf("the refusal is %d bytes, want about %d at most", got, maxMessage)
	}
}

// A child that fails on its own says nothing about the file, whichever way it
// ends, and says what it can.
func TestAChildSaysWhyItFailed(t *testing.T) {
	dir := t.TempDir()
	script := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for name, c := range map[string]struct {
		body, want    string
		fileIsAtFault bool
	}{
		"its own error":    {"echo 'dockwell: cannot limit its memory' >&2; exit 1", "cannot limit its memory", false},
		"no threads":       {"echo 'runtime: failed to create new OS thread' >&2; exit 2", "another thread", false},
		"out of memory":    {"echo 'fatal error: runtime: out of memory' >&2; exit 2", "more time or memory", true},
		"killed by limits": {"kill -9 $$", "more time or memory", true},
	} {
		t.Run(name, func(t *testing.T) {
			guarded(t)
			guardPath = script(strings.ReplaceAll(name, " ", "-"), c.body)
			_, err := guard(t.Context(), Input{File: []byte("services: {}\n")}, false)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("guard = %v, want it to say %q", err, c.want)
			}
			if errors.Is(err, ErrInvalid) != c.fileIsAtFault {
				t.Errorf("guard = %v, blaming the file: %v, want %v", err, errors.Is(err, ErrInvalid), c.fileIsAtFault)
			}
		})
	}
}

// A server that already holds a child to less keeps that limit, rather than
// failing every import.
func TestAChildKeepsALowerLimit(t *testing.T) {
	path, err := child()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "ulimit -t 5; exec \"$0\"", path)
	cmd.Stdin = strings.NewReader(`{"file":"c2VydmljZXM6IHt9Cg==","cpu":10,"memory":268435456}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("under a lower CPU limit: %v: %s", err, out)
	}
}

// Files beyond those a server can read soon are turned away, and what's too much
// costs nothing to find out.
func TestWaitingIsLimited(t *testing.T) {
	waiting.Store(maxWaiting)
	defer waiting.Store(0)
	_, err := Parse(t.Context(), []byte("services: {}\n"), nil)
	if !errors.Is(err, ErrBusy) {
		t.Errorf("Parse with every place taken = %v, want ErrBusy", err)
	}
	if got := waiting.Load(); got != maxWaiting {
		t.Errorf("%d waiting after that, want %d", got, maxWaiting)
	}
	waiting.Store(0)
	for range cap(parsing) {
		parsing <- struct{}{}
	}
	defer func() {
		for range cap(parsing) {
			<-parsing
		}
	}()
	_, err = Parse(t.Context(), []byte("services: {}\n"), map[string]string{"A": strings.Repeat("x", MaxVariables)})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "the variables come to") {
		t.Errorf("too many variables with every turn taken = %v, want them refused at once", err)
	}
}

// Checking a file a person is typing goes through the guard before anything
// reads it here, so one made to cost too much says nothing of itself.
func TestCheckStopsAtTheGuard(t *testing.T) {
	guarded(t)
	guardCPU, guardTimeout = 1, 3*time.Second
	file, vars := costly()
	named := append([]byte("name: shop\n"), file...)

	seen, stack, err := Check(t.Context(), Input{File: named, Variables: vars})
	if !errors.Is(err, ErrCostly) || !errors.Is(err, ErrInvalid) {
		t.Fatalf("Check = %v, want it stopped", err)
	}
	if stack != nil || seen.Name != "" || len(seen.Variables) != 0 || len(seen.Exposed) != 0 {
		t.Errorf("the file was read past the guard: %+v, %+v", seen, stack)
	}
	if got, _ := Lines(t.Context(), named, err); got != 0 {
		t.Errorf("Lines = %d, want 0: it isn't about a line, and reading it again costs what the guard stopped", got)
	}

	// One it can read says what it says of itself, refused or not.
	seen, _, err = Check(t.Context(), Input{File: []byte("name: shop\nservices:\n  web:\n    image: a:${TAG}\n")})
	if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrCostly) || seen.Name != "shop" || len(seen.Variables) != 1 {
		t.Errorf("a file that needs a variable = %+v, %v", seen, err)
	}
}

// costlyToInspect is refused on its top-level key, before anything reads its
// variables, and its string names one variable about 120,000 times, under 60
// long keys: what took 59 s here when the walk ran beside the child, not in it.
func costlyToInspect() []byte {
	const depth = 60
	key := strings.Repeat("[", 255)
	var b strings.Builder
	b.WriteString("secrets: {}\nservices:\n  web:\n    command: ")
	for range depth {
		fmt.Fprintf(&b, "{%q: ", key)
	}
	b.WriteString(`"`)
	for b.Len() < MaxFile-1024-2*depth-16 {
		b.WriteString("$a")
	}
	b.WriteString(`"` + strings.Repeat("}", depth) + "\n")
	return []byte(b.String())
}

// Check's reading of a file is the child's, under its limits: what the child
// says is what Check says, and nothing here reads the file for it again.
func TestCheckIsReadInTheChild(t *testing.T) {
	guarded(t)
	script := func(name, body string) string {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\ncat >/dev/null\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	file := []byte("name: from-the-file\nservices:\n  web:\n    image: nginx:1\n    command: x ${FROM_THE_FILE:-a}\n")

	guardPath = script("says", `echo '{"name":"from-the-child","variables":[{"Name":"FROM_THE_CHILD","Required":true}]}'`)
	seen, stack, err := Check(t.Context(), Input{File: file})
	if err != nil {
		t.Fatal(err)
	}
	if seen.Name != "from-the-child" || len(seen.Variables) != 1 || seen.Variables[0].Name != "FROM_THE_CHILD" || !seen.Variables[0].Required {
		t.Errorf("Check = %+v, want what the child said", seen)
	}
	if len(seen.Exposed) != 0 {
		t.Errorf("Check read the file here: %+v", seen.Exposed)
	}
	if stack == nil || stack.Name != "from-the-file" {
		t.Errorf("stack = %+v, want Parse's own reading of the file", stack)
	}

	// One that says more than a file could, to fill the parent's memory.
	guardPath = script("floods", fmt.Sprintf("head -c %d /dev/zero", maxInspection+1))
	if _, _, err := Check(t.Context(), Input{File: file}); !errors.Is(err, ErrCostly) {
		t.Errorf("a child that says too much = %v, want it stopped", err)
	}

	guardPath = script("garbles", "echo not-json")
	_, _, err = Check(t.Context(), Input{File: file})
	if err == nil || errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "about the server") {
		t.Errorf("a child that makes no sense = %v, want it said to be the server's fault", err)
	}

	// Parse asks for nothing, so what a child prints goes nowhere.
	guardPath = script("floods-anyway", "echo not-json")
	if _, err := Parse(t.Context(), file, nil); err != nil {
		t.Errorf("Parse = %v, want it to ignore what it didn't ask for", err)
	}
}

// A real child says what the file says of itself, refused or not, and a file
// made to cost a lot here costs nothing more than the child's reading of it.
func TestCheckOfACostlyFileIsQuick(t *testing.T) {
	guarded(t)
	start := time.Now()
	seen, stack, err := Check(t.Context(), Input{File: costlyToInspect()})
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("Check took %v, which holds one of two turns at reading from everyone else", took)
	}
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "secrets") {
		t.Errorf("Check = %.200v, want it refused for its secrets", err)
	}
	if stack != nil || len(seen.Variables) != 1 || seen.Variables[0].Name != "a" {
		t.Errorf("Check = %+v, %+v", seen.Variables, stack)
	}
	if len(seen.Exposed) != 0 {
		t.Errorf("a command keeps what it names by name, so Exposed = %+v", seen.Exposed)
	}
}

// Someone who leaves takes the reading of their file with them.
func TestLeavingStopsACheck(t *testing.T) {
	guarded(t)
	guardPath = ""
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	file := []byte("name: shop\nservices:\n  web:\n    image: nginx:1\n    command: x ${A:-b}\n")
	if _, _, err := Check(ctx, Input{File: file}); !errors.Is(err, context.Canceled) {
		t.Errorf("Check = %v, want it stopped", err)
	}
	if seen := inspect(ctx, Input{File: file}); len(seen.Variables) != 0 || len(seen.Exposed) != 0 {
		t.Errorf("inspect read on for no one: %+v", seen)
	}
	// Each walk stops by itself too, as a walk is what takes the time.
	if _, _, err := interpolate(ctx, map[string]any{"a": "$A"}, map[string]string{"A": "b"}); !errors.Is(err, context.Canceled) {
		t.Errorf("interpolate = %v, want it stopped", err)
	}
	if _, err := parse(ctx, Input{File: file}); !errors.Is(err, context.Canceled) {
		t.Errorf("parse = %v, want it stopped", err)
	}
	in := &interpolator{ctx: ctx, missing: map[string]bool{}}
	if _, err := in.walk(place{}, "$A"); !errors.Is(err, context.Canceled) || len(in.missing) != 0 {
		t.Errorf("the walk = %v, having read %v; want it to stop before it reads anything", err, in.missing)
	}
	if got, _ := exposedIn(ctx, map[string]any{"services": map[string]any{"web": map[string]any{"command": "$A"}}}); len(got) != 0 {
		t.Errorf("exposedIn read on for no one: %+v", got)
	}
}

// The child reads all a file reads: a value that costs too much is stopped there
// from an .env as from the request, and a relative path doesn't end the read before
// what comes after it, once it knows where the file is.
func TestTheChildReadsAllThatIsGiven(t *testing.T) {
	guarded(t)
	guardCPU = 1
	long := strings.Repeat("x", 60000)
	costly := []byte("services:\n  slow:\n    image: nginx:1\n    command: ${A}${A}${A}${A}${A}${A}${A}${A}${A}${A}${A}${A}${A}${A}${A}${A}\n")
	_, err := guard(t.Context(), Input{File: costly, Env: "A=" + long + "\n"}, false)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "more time or memory") {
		t.Errorf("a costly value in an .env = %.300v, want it stopped", err)
	}
	behind := []byte("services:\n  slow:\n    image: nginx:1\n    volumes: [\"./data:/data\"]\n    command: ${A}${A}${A}${A}${A}${A}${A}${A}${A}${A}${A}${A}${A}${A}${A}${A}\n")
	_, err = guard(t.Context(), Input{File: behind, Variables: map[string]string{"A": long}, Directory: "/srv/app"}, false)
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "more time or memory") {
		t.Errorf("a costly value behind a relative path = %.300v, want it stopped", err)
	}
}

// The new keys take no more than the rest of a file does: each costs in proportion
// to what it takes of the budget a file has.
func TestTheNewKeysCostWhatTheFileDoes(t *testing.T) {
	guarded(t)
	build := func(head string, each func(i int) string, tail string) []byte {
		var b strings.Builder
		b.WriteString(head)
		for i := 0; b.Len() < MaxFile-len(tail)-64; i++ {
			b.WriteString(each(i))
		}
		b.WriteString(tail)
		return []byte(b.String())
	}
	for name, file := range map[string][]byte{
		"capabilities": build("services:\n  web:\n    image: nginx:1\n    cap_add:\n", func(int) string { return "      - NET_ADMIN\n" }, ""),
		"groups":       build("services:\n  web:\n    image: nginx:1\n    group_add:\n", func(i int) string { return "      - \"" + strconv.Itoa(i) + "\"\n" }, ""),
		"aliases":      build("services:\n  web:\n    image: nginx:1\n    networks:\n      back:\n        aliases:\n", func(i int) string { return "          - a" + strconv.Itoa(i) + "\n" }, "networks: {back: {}}\n"),
		"networks":     build("services:\n  web:\n    image: nginx:1\n    networks:\n", func(i int) string { return "      n" + strconv.Itoa(i) + ": {aliases: [a]}\n" }, ""),
		"externals":    build("services:\n  web:\n    image: nginx:1\nnetworks:\n", func(i int) string { return "  n" + strconv.Itoa(i) + ": {external: true}\n" }, ""),
		"builds":       build("services:\n", func(i int) string { return "  s" + strconv.Itoa(i) + ": {image: 'nginx:1', build: .}\n" }, ""),
		"limits":       build("services:\n", func(i int) string { return "  s" + strconv.Itoa(i) + ": {image: 'nginx:1', cpus: 1, mem_limit: 64M}\n" }, ""),
		"names": build("services:\n", func(i int) string {
			return "  s" + strconv.Itoa(i) + ": {image: 'nginx:1', container_name: c" + strconv.Itoa(i) + "}\n"
		}, ""),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := guard(t.Context(), Input{File: file}, false); err != nil {
				t.Errorf("a file of %d KiB: %.300v", len(file)>>10, err)
			}
		})
	}
}

// A test binary ignores GuardCommand and runs every test again, so it must read in its
// own process. UseGuard is safe to call here: this asks it what it would run, and runs
// nothing.
func TestATestBinaryReadsInItsOwnProcess(t *testing.T) {
	for exe, want := range map[string]string{
		"/usr/local/bin/dockwell":        "/proc/self/exe",
		"/tmp/go-build123/b001/cli.test": "",
		"":                               "/proc/self/exe", // a path that can't be read keeps the child
	} {
		if got := guardFor(exe); got != want {
			t.Errorf("guardFor(%q) = %q, want %q", exe, got, want)
		}
	}
	t.Cleanup(func() { guardPath = "" })
	guardPath = "set by something else"
	UseGuard()
	if guardPath != "" {
		t.Errorf("in a test binary, UseGuard sets the child to %q", guardPath)
	}
}

// A reader never starts another, in any binary. The child here is a script that writes
// down its environment and exits, never this test binary, so a mistake costs one
// short process, and neither half can start a second.
func TestAReaderNeverStartsAnother(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	fake := filepath.Join(dir, "fake")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nenv > "+seen+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	guardPath = fake
	t.Cleanup(func() { guardPath = "" })

	// A first reader starts its child, and marks it.
	if _, err := guard(t.Context(), Input{}, false); err != nil {
		t.Fatalf("a first reader = %v", err)
	}
	env, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("the child ran, and wrote nothing: %v", err)
	}
	if !strings.Contains(string(env), guardChildEnv+"=1\n") {
		t.Errorf("the child isn't marked: %s", env)
	}

	// A process so marked starts nothing, and says why.
	if err := os.Remove(seen); err != nil {
		t.Fatal(err)
	}
	t.Setenv(guardChildEnv, "1")
	_, err = guard(t.Context(), Input{}, false)
	if err == nil || !strings.Contains(err.Error(), "never starts another") {
		t.Errorf("a marked process = %v, want it refused", err)
	}
	if _, err := os.Stat(seen); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a marked process started a child: %v", err)
	}
}
