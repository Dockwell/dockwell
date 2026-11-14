// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
)

// oldTaken is the record of what was taken on as an agent from before ADR 0096
// keeps it, with no references: any Take or Forget it makes rewrites the whole
// record so.
type oldTaken struct {
	Project string `json:"project"`
	Service string `json:"service"`
	Name    string `json:"name,omitempty"`
	Compose string `json:"compose,omitempty"`
	Leads   string `json:"leads,omitempty"`
}

// A record that lost its references, as an older agent's write of it does, reads a
// taken-on container's fields that hold a value as Unread, so neither what the agent
// reports nor drift's words carry the value (ADR 0096). The review of slice 1 found
// this.
func TestARecordThatLostItsReferencesShowsNoValue(t *testing.T) {
	e := dockertest.New(t)
	e.AddLocalImage("mariadb:11", dockertest.Image{Config: docker.Config{Cmd: []string{"mariadbd"}}})
	c := e.Client()
	record := filepath.Join(t.TempDir(), "adopted.json")
	c.Adopted = &docker.Adopted{Path: record}
	d := &Docker{Client: c, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1}
	const pw = "k8Jd2mQxR4"
	id, err := c.CreateContainer(t.Context(), "legacy_db", &docker.CreateRequest{Config: docker.Config{
		Image: "mariadb:11", Env: []string{"MARIADB_ROOT_PASSWORD=" + pw},
		Healthcheck: &docker.Healthcheck{Test: []string{"CMD-SHELL", "mariadb-admin ping -uroot -p" + pw}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.StartContainer(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	read, err := d.ReadAdoption(t.Context(), taken, id)
	if err != nil {
		t.Fatal(err)
	}
	spec := read.Spec.(*resourcev1.Container)
	if err := d.Adopt(t.Context(), taken, id, spec, resource.Decision{}); err != nil {
		t.Fatal(err)
	}
	// The older agent rewrites the record.
	b, _ := os.ReadFile(record)
	var old map[string]oldTaken
	if err := json.Unmarshal(b, &old); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(old)
	if err := os.WriteFile(record, b, 0o600); err != nil {
		t.Fatal(err)
	}
	// Upgraded again.
	c2 := e.Client()
	c2.Adopted = &docker.Adopted{Path: record}
	d2 := &Docker{Client: c2, Settle: time.Millisecond, Poll: time.Millisecond, Soak: -1}
	got, err := d2.Inspect(t.Context(), taken)
	if err != nil {
		t.Fatal(err)
	}
	if test := got.GetHealthcheck().GetTest(); len(test) != 1 || test[0] != binding.Unread {
		t.Errorf("the check reads back as %q", test)
	}
	for _, ch := range resource.Diff(spec, got) {
		if strings.Contains(fmt.Sprint(ch.From, ch.To), pw) {
			t.Errorf("drift's words hold the value: %+v", ch)
		}
	}
}

// What Docker says of a container that won't start, and its state's error, are
// masked by the values it was taken on with, as it quotes what it couldn't run.
func TestAStartsWordsHideATakenOnValue(t *testing.T) {
	const pw = "k8Jd2mQxR4"
	c := &docker.Container{}
	c.Config.Env = []string{"RUN_TOKEN=" + pw}
	c.Config.Labels = map[string]string{docker.LabelAdopted: "legacy"}
	c.State.Error = `exec: "` + pw + `": executable file not found in $PATH`
	if got := binding.Hide(c.State.Error, valuesInContainer(c)); strings.Contains(got, pw) {
		t.Errorf("the state's error reads %q", got)
	}
	err := hideIn(&docker.Error{Message: c.State.Error}, valuesInContainer(c))
	if strings.Contains(err.Error(), pw) {
		t.Errorf("the start's error reads %q", err)
	}
}
