// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
)

// discards counts the changes the CLI discards.
type discards struct {
	dockwellv1connect.UnimplementedChangeServiceHandler
	n atomic.Int32
}

func (d *discards) DiscardChange(context.Context, *connect.Request[dockwellv1.DiscardChangeRequest]) (*connect.Response[dockwellv1.DiscardChangeResponse], error) {
	d.n.Add(1)
	return connect.NewResponse(&dockwellv1.DiscardChangeResponse{}), nil
}

// A server with nothing on it leaves by a plan with no other change, which the CLI
// keeps to apply, where it discards a plan that changes nothing (ADR 0090).
func TestARemovalIsNeverAPlanOfNothing(t *testing.T) {
	d := &discards{}
	_, h := dockwellv1connect.NewChangeServiceHandler(d)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	plan := func(c *dockwellv1.Change) (string, int32) {
		t.Helper()
		var out, errs bytes.Buffer
		d.n.Store(0)
		r := &runner{env: &Env{Out: &out, Err: &errs}, opts: &options{planOnly: true}, client: &Client{URL: srv.URL, HTTP: srv.Client()}, command: "dockwell servers remove"}
		if err := r.planned(t.Context(), &dockwellv1.PlanRemoveServerResponse{Change: c}, c); err != nil {
			t.Fatal(err)
		}
		return errs.String(), d.n.Load()
	}
	removal := &dockwellv1.Change{Id: "chg_remove", State: dockwellv1.ChangeState_CHANGE_STATE_PLANNED, Plan: &dockwellv1.Plan{
		Removes: &dockwellv1.ServerRemoval{ServerId: "srv_2", ServerName: "hz-2"},
		Servers: []*dockwellv1.ServerPlan{{ServerId: "srv_2", ServerName: "hz-2", Removed: true}},
	}}
	if said, n := plan(removal); n != 0 || strings.Contains(said, "Nothing to change") || !strings.Contains(said, "chg_remove is planned") {
		t.Errorf("an empty server's removal: discarded %d times, said %q", n, said)
	}
	nothing := &dockwellv1.Change{Id: "chg_same", State: dockwellv1.ChangeState_CHANGE_STATE_PLANNED, Plan: &dockwellv1.Plan{}}
	if said, n := plan(nothing); n != 1 || !strings.Contains(said, "Nothing to change") {
		t.Errorf("a plan of nothing: discarded %d times, said %q", n, said)
	}
}
