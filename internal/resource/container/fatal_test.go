// SPDX-License-Identifier: AGPL-3.0-only

package container_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// A container that stops right after it starts says why where a server's own fatal
// line does, such as MongoDB refusing a kernel, and only its message: an app's own
// words, and a line's attributes, stay in its log, which only someone who may read
// logs reads (ADR 0025).
func TestAContainerThatStopsSaysWhyWhereItsServerDoes(t *testing.T) {
	const (
		kernel  = "MongoDB cannot start: Linux kernel versions 6.19 and newer has a known incompatibility with this version of MongoDB. See https://jira.mongodb.org/browse/SERVER-121912 for more information."
		fatal   = `{"t":{"$date":"2026-11-11T04:28:19.423Z"},"s":"F",  "c":"CONTROL",  "id":12257600,"ctx":"main","msg":"` + kernel + `"}`
		failed  = `{"t":{"$date":"2026-11-11T04:28:19.420Z"},"s":"E",  "c":"STORAGE",  "id":22312,"ctx":"main","msg":"Failed to open the data files","attr":{"secret":"hunter2"}}`
		info    = `{"t":{"$date":"2026-11-11T04:28:19.400Z"},"s":"I",  "c":"CONTROL",  "id":23285,"ctx":"main","msg":"Automatically disabling TLS 1.0"}`
		stopped = "web stopped right after it started, with exit code 1. Check its command and variables, then try again."
	)
	for _, c := range []struct {
		name    string
		prints  []string
		restart bool
		want    string
	}{
		{"MongoDB's fatal line is what it says", []string{info, fatal}, false, "web stopped right after it started, with exit code 1, saying: " + kernel},
		{"a fatal line wins over an error before it", []string{failed, fatal, info}, false, "web stopped right after it started, with exit code 1, saying: " + kernel},
		{"an error line alone, without its attributes", []string{info, failed}, false, "web stopped right after it started, with exit code 1, saying: Failed to open the data files."},
		{"a container that keeps restarting says it too", []string{fatal}, true, "web keeps restarting: it exited with code 1, saying: " + kernel},
		{"an app's own words stay in its log", []string{"DATABASE_URL isn't set"}, false, stopped},
		{"as do lines that only look like MongoDB's", []string{`{"s":"F","msg":"not a server's log"}`}, false, stopped},
		{"and a server's ordinary lines say nothing", []string{info}, false, stopped},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := dockertest.New(t)
			const image = "mongo:8.0"
			e.Registry[image] = dockertest.Image{Exits: !c.restart, Prints: c.prints, Config: docker.Config{Cmd: []string{"mongod"}}}
			d := &container.Driver{Runtime: &container.Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1, HealthWait: time.Second}}
			ref := resource.Ref{Kind: container.Kind, Name: "web"}
			spec := &resourcev1.Container{Image: image}
			if err := d.Apply(t.Context(), resource.Step{Ref: ref, Decision: resource.Decision{Action: resource.ActionCreate}}, spec); err != nil {
				t.Fatal(err)
			}
			if c.restart {
				e.SetRestarting("web", 1)
			}
			err := d.Verify(t.Context(), ref, spec)
			plain, _ := resource.Explain(err)
			if plain != c.want || resource.ReasonOf(err) != resource.ReasonExits {
				t.Errorf("it says %q, for %q; want %q", plain, resource.ReasonOf(err), c.want)
			}
			if got := fmt.Sprint(err); strings.Contains(got, "hunter2") {
				t.Errorf("a line's attributes reached the step: %s", got)
			}
		})
	}
}
