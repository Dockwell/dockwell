// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/agent/desired"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// What the CLI says, as far as the invariants need it.
const (
	connected = "CONNECTION_CONNECTED"
	applied   = "CHANGE_STATE_APPLIED"
	failed    = "CHANGE_STATE_FAILED"
)

type changeJSON struct {
	Change struct {
		ID    string `json:"id"`
		State string `json:"state"`
	} `json:"change"`
}

type serverJSON struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Connection string `json:"connection"`
}

type resourceJSON struct {
	ID       string `json:"id"`
	Resource struct {
		Kind  string `json:"kind"`
		Scope string `json:"scope"`
		Name  string `json:"name"`
	} `json:"resource"`
	Revision string `json:"revision"`
}

type eventJSON struct {
	ID         string    `json:"id"`
	Action     string    `json:"action"`
	Time       time.Time `json:"time"`
	Reconciled *struct {
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Reason  string `json:"reason"`
		Dropped uint32 `json:"dropped"`
	} `json:"reconciled"`
}

// box is one container of a service, as Docker has it.
type box struct {
	ID       string
	Image    string
	Running  bool
	Error    string
	Health   string
	Revision string
	Change   string
	Finished time.Time
}

// services are what chaos-1 runs, by name.
var services = []string{"web", "slow", "spare", "held"}

// boxes are each service's containers, by the service's name.
func (s *stand) boxes(ctx context.Context) (map[string][]box, error) {
	list, err := s.docker.ListContainers(ctx, map[string][]string{"label": {docker.LabelProject + "=" + s.project}})
	if err != nil {
		return nil, err
	}
	out := map[string][]box{}
	for i := range list {
		c, err := s.docker.InspectContainer(ctx, list[i].ID)
		if err != nil {
			if docker.IsNotFound(err) {
				continue // gone between the list and now
			}
			return nil, err
		}
		b := box{
			ID: c.ID, Image: c.Config.Image, Running: c.State.Running || c.State.Restarting, Error: c.State.Error, Finished: c.State.FinishedAt,
			Revision: c.Config.Labels[container.LabelRevision], Change: c.Config.Labels[container.LabelChange],
		}
		if c.State.Health != nil {
			b.Health = c.State.Health.Status
		}
		name := c.Config.Labels[docker.LabelService]
		out[name] = append(out[name], b)
	}
	return out, nil
}

// one is the service's one container, or why there isn't one.
func (s *stand) one(ctx context.Context, svc string) (box, error) {
	all, err := s.boxes(ctx)
	if err != nil {
		return box{}, err
	}
	if n := len(all[svc]); n != 1 {
		return box{}, fmt.Errorf("there are %d containers of %s, want one", n, svc)
	}
	return all[svc][0], nil
}

// want is a service as the store has it: its live revision, the image its spec
// runs, and its last change.
type want struct {
	Revision    int64
	Image       string
	Change      string
	ChangeState string
}

// live is each service as the store has it.
func (s *stand) live(ctx context.Context) (map[string]want, error) {
	var res struct {
		Services []struct {
			Resource struct {
				Scope string `json:"scope"`
				Name  string `json:"name"`
			} `json:"resource"`
			Revision string `json:"revision"`
			Spec     struct {
				App struct {
					Image string `json:"image"`
				} `json:"app"`
			} `json:"spec"`
			ChangeID    string `json:"changeId"`
			ChangeState string `json:"changeState"`
		} `json:"services"`
	}
	if err := s.cliJSON(ctx, &res, "services", "list"); err != nil {
		return nil, err
	}
	out := map[string]want{}
	for _, sv := range res.Services {
		if docker.Project(sv.Resource.Scope) != s.project {
			continue
		}
		rev, _ := strconv.ParseInt(sv.Revision, 10, 64)
		out[sv.Resource.Name] = want{Revision: rev, Image: sv.Spec.App.Image, Change: sv.ChangeID, ChangeState: sv.ChangeState}
	}
	return out, nil
}

// changeState is what the store says of a change.
func (s *stand) changeState(ctx context.Context, id string) (string, error) {
	var c changeJSON
	if err := s.cliJSON(ctx, &c, "changes", "get", id); err != nil {
		return "", err
	}
	return c.Change.State, nil
}

// connection is how the control plane says chaos-1 is connected.
func (s *stand) connection(ctx context.Context) (string, error) {
	var sv struct {
		Server serverJSON `json:"server"`
	}
	if err := s.cliJSON(ctx, &sv, "servers", "get", s.serverID); err != nil {
		return "", err
	}
	return sv.Server.Connection, nil
}

// reconciled are what chaos-1 said it did on its own, on its timeline, newest first.
func (s *stand) reconciled(ctx context.Context) ([]eventJSON, error) {
	var list struct {
		Events []eventJSON `json:"events"`
	}
	if err := s.cliJSON(ctx, &list, "events", "list", "--server-id", s.serverID, "--kinds", "server", "--page-size", "200"); err != nil {
		return nil, err
	}
	out := list.Events[:0]
	for _, e := range list.Events {
		if strings.HasPrefix(e.Action, "server.reconciled_") {
			out = append(out, e)
		}
	}
	return out, nil
}

// drift is what the control plane says differs on chaos-1, or nothing.
func (s *stand) drift(ctx context.Context) (string, error) {
	out, err := s.cli(ctx, "drift", "list", "--server-id", s.serverID, "--json")
	if err != nil {
		return "", err
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(out, &body); err != nil {
		return "", fmt.Errorf("drift list printed what isn't JSON: %w", err)
	}
	for _, v := range body {
		if t := strings.TrimSpace(string(v)); t != "[]" && t != "{}" && t != "null" && t != "" {
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "", nil
}

// kept is what chaos-1's agent keeps of a service: the revision, and the image
// it would make it again from.
type kept struct {
	Revision int64
	Image    string
}

// records are what chaos-1's agent keeps, by service, read with its key while
// it's stopped.
func (s *stand) records() (map[string]kept, error) {
	if s.agent.alive() {
		return nil, errors.New("the agent runs, and its records are read only while it's stopped")
	}
	folder := filepath.Join(s.agentData, "agent")
	st, err := desired.Open(folder, s.serverID, slog.New(slog.DiscardHandler))
	if err != nil {
		return nil, err
	}
	out := map[string]kept{}
	for _, ref := range st.Refs() {
		if ref.Kind != "service" || docker.Project(ref.Scope) != s.project {
			continue
		}
		r, ok, err := st.Read(ref)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var svc resourcev1.Service
		if err := proto.Unmarshal(r.Spec, &svc); err != nil {
			return nil, fmt.Errorf("the record of %s holds what isn't a service: %w", ref.Name, err)
		}
		out[ref.Name] = kept{Revision: r.Revision, Image: svc.GetApp().GetImage()}
	}
	return out, nil
}

// journal is what chaos-1's agent has written down and not yet handed back, as
// its file says.
func (s *stand) journal() ([]desired.Entry, error) {
	b, err := os.ReadFile(filepath.Join(s.agentData, "agent", desired.Folder, desired.JournalFile)) //nolint:gosec // the journal in the stand's own agent folder
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j struct {
		Entries []desired.Entry `json:"entries"`
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, err
	}
	return j.Entries, nil
}
