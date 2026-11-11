// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/service"
)

// Bounds on what a build's log sends, so a build can't crowd out the calls on the
// agent's stream (ADR 0046).
const (
	// maxLine is the most of one line that is sent; the rest is cut.
	maxLine = 4 << 10
	// flushEvery is how often lines go, and flushBytes how much one update holds.
	flushEvery = 250 * time.Millisecond
	flushBytes = 64 << 10
	// perSecond is what a build's log may send in a second. Past it, lines are
	// dropped and counted.
	perSecond = 256 << 10
	// maxPending is the most that waits to be read, of a line with no end yet, and
	// maxPartials the most of all the steps' together, however many run at once.
	maxPending  = 64 << 10
	maxPartials = 1 << 20
	// maxStatus is the most of one of BuildKit's statuses that waits for its end:
	// 1 MiB of a step's output in base64, and what's around it.
	maxStatus = 2 << 20
)

// stepName matches a Dockerfile step as BuildKit names it: "[4/9] RUN npm ci", or
// "[build 2/5] COPY . ." in a stage with a name.
var stepName = regexp.MustCompile(`^\[(?:[^\]\s]+ )?\d+/\d+\] `)

// solveStatus is one line of buildctl's --progress rawjson: client.SolveStatus,
// whose fields have no JSON names of their own.
type solveStatus struct {
	Vertexes []struct {
		Digest    string
		Name      string
		Started   *time.Time
		Completed *time.Time
		Cached    bool
		Error     string
	}
	Logs []struct {
		Vertex    string
		Stream    int
		Data      []byte
		Timestamp time.Time
	}
}

// vertex is what the report knows of one of BuildKit's vertices.
type vertex struct {
	name      string
	step      int // its place among the Dockerfile's steps, from 1; 0 for BuildKit's own
	started   bool
	completed bool
	cached    bool
	partial   []byte // a line it has begun and not ended
	last      string // the last whole line it printed
}

// report reads what a builder says on standard error, and tells whoever follows
// the build: its log, line by line, where it stands, and how it failed.
type report struct {
	ctx     context.Context //nolint:containedctx // for the build's updates, for as long as it runs
	req     service.BuildRequest
	secrets []string
	now     func() time.Time

	mu       sync.Mutex
	buf      bytes.Buffer
	phase    string
	skipping bool   // the rest of a status too long to keep is dropped
	failed   string // what the script said stopped it
	pruned   bool   // the script pruned the cache as the build ended
	subject  string
	author   string
	vertices map[string]*vertex
	order    []string // the Dockerfile's steps, by digest, as they came
	failing  *vertex
	stepErr  string
	said     []string // lines that came after BuildKit gave up, which say why
	tail     []string // the last lines of the log, for a failure with nothing better
	number   int64
	pending  []resource.BuildLine
	size     int
	window   time.Time
	spent    int
	dropped  int64
	start    time.Time
	// builder is what builds it, once the reading or the script has said, and
	// detection what the reading found.
	builder   string
	detection *resource.BuildDetection
}

func newReport(ctx context.Context, req service.BuildRequest, now func() time.Time) *report {
	return &report{ctx: ctx, req: req, now: now, vertices: map[string]*vertex{}, secrets: secretForms(req.Secrets)}
}

// secretForms are the forms a build's secrets take in its log (binding.Lines).
func secretForms(secrets map[string]string) []string {
	values := make([]string, 0, len(secrets))
	for _, v := range secrets {
		values = append(values, v)
	}
	return binding.Lines(values)
}

// hide masks the build's secrets in what the builder said, as it comes and before
// anything keeps it: a line, the line a step said last, or BuildKit's words.
func (r *report) hide(text string) string {
	if len(r.secrets) == 0 {
		return text
	}
	return binding.Hide(text, r.secrets)
}

// Write takes what the builder printed, a line at a time. One of BuildKit's
// statuses may hold up to 1 MiB of a step's output, in base64, so a line that is one
// waits for its end up to maxStatus; past that it is dropped and counted, with the
// rest of it, and never read raw, as no mask reads what's in base64.
func (r *report) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf.Write(b)
	for {
		i := bytes.IndexByte(r.buf.Bytes(), '\n')
		if i < 0 {
			switch {
			case r.skipping:
				r.buf.Reset()
			case r.isStatus(r.buf.Bytes()) && r.buf.Len() > maxStatus:
				r.buf.Reset()
				r.skipping = true
				r.dropped++
			case !r.isStatus(r.buf.Bytes()) && r.buf.Len() > maxPending:
				r.line(string(r.buf.Next(maxPending)))
				continue
			}
			return len(b), nil
		}
		line := r.buf.Next(i + 1)
		if r.skipping {
			// The end of a status too long to keep.
			r.skipping = false
			continue
		}
		r.line(strings.TrimRight(string(line), "\r\n"))
	}
}

// isStatus reports what may be one of BuildKit's statuses: a line in the build's
// phase that starts as JSON does.
func (r *report) isStatus(line []byte) bool {
	return r.phase == "build" && len(line) > 0 && line[0] == '{'
}

// line reads one line the builder printed.
func (r *report) line(line string) {
	if rest, ok := strings.CutPrefix(line, marker); ok {
		r.said = nil
		r.markerLine(rest)
		return
	}
	if r.isStatus([]byte(line)) {
		// Read before anything is masked, as a mask in its JSON, such as a short
		// secret inside a timestamp, would stop it parsing: what it holds is
		// masked field by field as it's read (status).
		var s solveStatus
		if json.Unmarshal([]byte(line), &s) == nil {
			r.status(&s)
			return
		}
		// One that doesn't parse holds a step's output in base64, which no mask
		// reads: dropped and counted, never kept.
		r.dropped++
		return
	}
	line = r.hide(line)
	if r.phase == "build" {
		// What buildctl says once BuildKit gives up, such as "error: failed to solve".
		r.said = append(r.said, line)
		if len(r.said) > 8 {
			r.said = r.said[1:]
		}
	}
	r.add(0, line)
}

// markerLine reads what the script says of itself.
func (r *report) markerLine(rest string) {
	verb, arg, _ := strings.Cut(rest, " ")
	switch verb {
	case "phase":
		r.phase = arg
		if arg == "build" && r.start.IsZero() {
			r.start = r.now()
			resource.ReportBuild(r.ctx, resource.BuildUpdate{State: resource.BuildBuilding, Start: r.start})
		}
	case "pruned":
		r.pruned = true
	case "apparmor":
		r.add(0, "Each step runs under the server's AppArmor profile "+arg+", as Docker's containers do")
	case "noapparmor":
		r.add(0, "This server has AppArmor on without Docker's profile, "+AppArmorProfile+", so the build's steps run under seccomp's profile alone, as Docker's containers do here")
	case "fail":
		r.failed = arg
	case "builder":
		r.builder = arg
		if arg == resource.BuilderDockerfile && r.detection == nil {
			r.detection = &resource.BuildDetection{Builder: arg, Dockerfile: filepath.Join(r.req.Context, dockerfile(r.req.Dockerfile))}
			resource.ReportBuild(r.ctx, resource.BuildUpdate{State: resource.BuildBuilding, Detection: r.detection})
		}
	case "commit":
		a, s, _ := strings.Cut(arg, " ")
		author, _ := base64.StdEncoding.DecodeString(a)
		subject, _ := base64.StdEncoding.DecodeString(s)
		r.author, r.subject = cut(string(author), 200), cut(string(subject), 500)
		resource.ReportBuild(r.ctx, resource.BuildUpdate{State: resource.BuildBuilding, Author: r.author, Subject: r.subject})
	}
}

// reading says the build reads the commit first, to say how it builds.
func (r *report) reading() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.add(0, "Reading the commit, to say how it builds")
}

// detectedAs says what reading the commit found, with the build's secrets masked in
// all of it, as Railpack's words can hold what it read: the log says what Railpack
// warned of, and why it can't build it, as Railpack said.
func (r *report) detectedAs(d *resource.BuildDetection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d = r.hideDetection(d)
	r.builder, r.detection = d.Builder, d
	if d.Builder == resource.BuilderRailpack {
		r.add(0, "Railpack "+cmpOr(d.Railpack, RailpackVersion)+" read it: "+cmpOr(strings.Join(d.Providers, ", "), "nothing it knows"))
		for _, w := range d.Warnings {
			r.add(0, w)
		}
		for _, line := range strings.Split(d.Error, "\n") {
			if strings.TrimSpace(line) != "" {
				r.add(0, line)
			}
		}
	}
	resource.ReportBuild(r.ctx, resource.BuildUpdate{State: resource.BuildBuilding, Detection: d})
}

// hideDetection is d with the build's secrets masked in each of its words.
func (r *report) hideDetection(d *resource.BuildDetection) *resource.BuildDetection {
	out := *d
	out.Providers = slices.Clone(d.Providers)
	out.Warnings = slices.Clone(d.Warnings)
	out.Packages = slices.Clone(d.Packages)
	out.Steps = make([]resource.DetectedStep, len(d.Steps))
	for i := range out.Providers {
		out.Providers[i] = r.hide(out.Providers[i])
	}
	for i := range out.Warnings {
		out.Warnings[i] = r.hide(out.Warnings[i])
	}
	for i := range out.Packages {
		p := &out.Packages[i]
		p.Name, p.Requested, p.Resolved, p.Source = r.hide(p.Name), r.hide(p.Requested), r.hide(p.Resolved), r.hide(p.Source)
	}
	for i, st := range d.Steps {
		out.Steps[i] = resource.DetectedStep{Name: r.hide(st.Name), Commands: slices.Clone(st.Commands)}
		for j := range out.Steps[i].Commands {
			out.Steps[i].Commands[j] = r.hide(out.Steps[i].Commands[j])
		}
	}
	out.Dockerfile, out.BaseImage, out.Start, out.Error = r.hide(d.Dockerfile), r.hide(d.BaseImage), r.hide(d.Start), r.hide(d.Error)
	return &out
}

// isStep reports a vertex that is one of the build's steps, as people count them:
// a Dockerfile's instruction, or a command of Railpack's plan, and none of
// BuildKit's own, such as loading the context or exporting the image.
func (r *report) isStep(name string) bool {
	if r.builder != resource.BuilderRailpack {
		return stepName.MatchString(name)
	}
	if strings.HasPrefix(name, "[") {
		return false
	}
	for _, p := range buildkitOwn {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// buildkitOwn starts the names of the vertices BuildKit and Railpack's frontend
// make of their own, which aren't the plan's steps.
var buildkitOwn = []string{
	"load build definition", "loading ", "docker-image://", "exporting", "resolve image config",
	"sending tarball", "naming to", "copy /", "merge ", "local://",
}

// status reads one of BuildKit's updates.
func (r *report) status(s *solveStatus) {
	moved := false
	for _, v := range s.Vertexes {
		x := r.vertices[v.Digest]
		if x == nil {
			x = &vertex{name: r.hide(v.Name)}
			r.vertices[v.Digest] = x
			if r.isStep(v.Name) {
				r.order = append(r.order, v.Digest)
				x.step = len(r.order)
				moved = true
			}
		}
		if v.Started != nil && !x.started {
			x.started, moved = true, true
			r.add(x.step, x.name)
		}
		if v.Cached && !x.cached {
			x.cached, moved = true, true
			r.add(x.step, "CACHED")
		}
		if v.Error != "" && r.failing == nil {
			r.failing, r.stepErr, moved = x, r.hide(v.Error), true
			r.flushPartial(x)
			r.add(x.step, "ERROR "+r.stepErr)
		}
		if v.Completed != nil && !x.completed {
			x.completed, moved = true, true
			r.flushPartial(x)
			if v.Error == "" && !x.cached && v.Started != nil {
				r.add(x.step, "DONE "+human.Duration(v.Completed.Sub(*v.Started).Round(100*time.Millisecond)))
			}
		}
	}
	for _, l := range s.Logs {
		x := r.vertices[l.Vertex]
		if x == nil {
			x = &vertex{}
			r.vertices[l.Vertex] = x
		}
		data := slices.Concat(x.partial, l.Data)
		x.partial = nil
		for {
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				break
			}
			text := r.hide(strings.TrimRight(string(data[:i]), "\r"))
			x.last = text
			r.add(x.step, text)
			data = data[i+1:]
		}
		if len(data) > maxPending {
			r.add(x.step, string(data))
			data = nil
		}
		x.partial = data
	}
	r.boundPartials()
	if moved {
		r.reportSteps()
	}
}

// boundPartials ends the longest lines that have no end yet, while they come to
// more than maxPartials together, so steps that run at once can't hold more.
func (r *report) boundPartials() {
	total := 0
	for _, x := range r.vertices {
		total += len(x.partial)
	}
	for total > maxPartials {
		var longest *vertex
		for _, x := range r.vertices {
			if longest == nil || len(x.partial) > len(longest.partial) {
				longest = x
			}
		}
		total -= len(longest.partial)
		r.flushPartial(longest)
	}
}

// flushPartial ends the line a vertex had begun, as it has ended.
func (r *report) flushPartial(x *vertex) {
	if len(x.partial) > 0 {
		text := r.hide(strings.TrimRight(string(x.partial), "\r"))
		x.last = text
		r.add(x.step, text)
		x.partial = nil
	}
}

// reportSteps says how far the build is.
func (r *report) reportSteps() {
	steps, cached, step, name := r.steps()
	resource.ReportBuild(r.ctx, resource.BuildUpdate{State: resource.BuildBuilding, Steps: steps, Cached: cached, Step: step, StepName: name})
	if name != "" {
		resource.Progress(r.ctx, fmt.Sprintf("Building step %d of %d, %s", step, steps, name))
	}
}

// steps is how many steps the build has, how many came from the cache, and the
// one it is on: the newest that started and hasn't finished, or the one that
// failed.
func (r *report) steps() (steps, cached, step int, name string) {
	for _, d := range r.order {
		x := r.vertices[d]
		if x.cached {
			cached++
		}
		if x.started && !x.completed {
			step, name = x.step, words(x.name)
		}
	}
	if r.failing != nil && r.failing.step > 0 {
		step, name = r.failing.step, words(r.failing.name)
	}
	return len(r.order), cached, step, name
}

// words is a step as people read it: "RUN npm ci" for "[4/9] RUN npm ci".
func words(name string) string {
	if loc := stepName.FindStringIndex(name); loc != nil {
		return cut(name[loc[1]:], 200)
	}
	return cut(name, 200)
}

// add puts a line in the log, with the build's secrets masked and cut to its
// bound, within what the log may send each second.
func (r *report) add(step int, text string) {
	text = cut(r.hide(text), maxLine)
	r.number++
	r.tail = append(r.tail, text)
	if len(r.tail) > 5 {
		r.tail = r.tail[1:]
	}
	now := r.now()
	if now.Sub(r.window) >= time.Second {
		r.window, r.spent = now, 0
	}
	if r.spent+len(text) > perSecond {
		r.dropped++
		return
	}
	r.spent += len(text)
	r.pending = append(r.pending, resource.BuildLine{Number: r.number, Time: now, Step: step, Text: text})
	r.size += len(text)
	if r.size >= flushBytes {
		r.sendLocked()
	}
}

// send sends the lines that wait.
func (r *report) send() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sendLocked()
}

func (r *report) sendLocked() {
	if len(r.pending) == 0 && r.dropped == 0 {
		return
	}
	resource.ReportBuild(r.ctx, resource.BuildUpdate{State: resource.BuildBuilding, Lines: r.pending, Dropped: r.dropped})
	r.pending, r.size, r.dropped = nil, 0, 0
}

// follow sends what waits every flushEvery until stop is called.
func (r *report) follow() (stop func()) {
	done := make(chan struct{})
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		t := time.NewTicker(flushEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				r.send()
			}
		}
	}()
	return func() {
		close(done)
		<-ended
		r.mu.Lock()
		if r.buf.Len() > 0 {
			r.line(strings.TrimRight(r.buf.String(), "\r\n"))
			r.buf.Reset()
		}
		for _, x := range r.vertices {
			r.flushPartial(x)
		}
		r.sendLocked()
		r.mu.Unlock()
	}
}

// facts is what the report knows when the build ends.
type facts struct {
	failed           string
	subject, author  string
	steps, cached    int
	step             int
	stepName         string
	failedLine       string
	stepError        string
	said             []string
	tail             []string
	start            time.Time
	reachedBuildKit  bool
	failedOutOfSteps bool
	memory           int64
	pruned           bool
	detection        *resource.BuildDetection
}

func (r *report) facts() facts {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := facts{failed: r.failed, subject: r.subject, author: r.author, said: r.said, tail: r.tail, start: r.start, stepError: r.stepErr, reachedBuildKit: r.phase == "build" || r.phase == "prune", pruned: r.pruned, detection: r.detection}
	f.steps, f.cached, f.step, f.stepName = r.steps()
	if x := r.failing; x != nil {
		f.failedLine = x.last
		if f.failedLine == "" {
			f.failedLine = lastWords(r.said)
		}
		f.failedOutOfSteps = x.step == 0
		f.stepName = words(x.name)
	}
	return f
}

// lastWords is the last line of lines, or nothing.
func lastWords(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// cut cuts s to at most n bytes, on a character's edge.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
