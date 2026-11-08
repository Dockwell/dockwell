// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/cli/apicli"
)

type stubImports struct {
	dockwellv1connect.UnimplementedImportServiceHandler
	got *dockwellv1.ImportOnePanelRequest
}

func (s *stubImports) ImportOnePanel(_ context.Context, req *connect.Request[dockwellv1.ImportOnePanelRequest]) (*connect.Response[dockwellv1.ImportOnePanelResponse], error) {
	s.got = req.Msg
	return connect.NewResponse(&dockwellv1.ImportOnePanelResponse{
		Found: &dockwellv1.OnePanel{Version: "v2.4.2", BaseDir: "/opt", Records: "/opt/1panel/db/agent.db"},
		Steps: []*dockwellv1.ImportStep{
			{Kind: dockwellv1.ImportKind_IMPORT_KIND_APP, Name: "wordpress", Summary: "WordPress 6.7.1 from 1Panel's store, running", Action: dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON, Becomes: "a Compose project, wordpress, taken on as it runs, with no restart"},
			{Kind: dockwellv1.ImportKind_IMPORT_KIND_WEBSITE, Name: "blog.example.com", Summary: "a static site", Action: dockwellv1.ImportAction_IMPORT_ACTION_MAKE, Becomes: "a website serving /opt/1panel/www/sites/blog/index, in place", WaitsFor: dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_WEBSITES},
			{Kind: dockwellv1.ImportKind_IMPORT_KIND_PANEL, Name: "Firewall", Summary: "the rules 1Panel writes to the server's firewall", Action: dockwellv1.ImportAction_IMPORT_ACTION_LEAVE, Reason: "Once Dockwell has firewall rules, it reads them where they are."},
		},
	}), nil
}

// dockwell import 1panel --plan-only says what an import would do, grouped by when
// each thing moves. It plans nothing yet, so there's nothing to leave planned.
func TestImport1PanelPlanOnly(t *testing.T) {
	stub := &stubImports{}
	mux := http.NewServeMux()
	mux.Handle(dockwellv1connect.NewImportServiceHandler(stub))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	env := map[string]string{apicli.EnvURL: srv.URL, apicli.EnvToken: wellFormedToken(), "XDG_CONFIG_HOME": t.TempDir()}
	r := run(t, env, "import", "1panel", "--server-id", "srv_1", "--plan-only")
	if r.code != exitOK {
		t.Fatalf("= %d:\n%s%s", r.code, r.out, r.err)
	}
	if stub.got.GetServerId() != "srv_1" {
		t.Errorf("asked %v", stub.got)
	}
	for _, want := range []string{
		"1Panel v2.4.2, under /opt, read from /opt/1panel/db/agent.db. Nothing was changed.",
		"In this plan, taken on as it runs, with no restart (1)",
		"  App wordpress: WordPress 6.7.1 from 1Panel's store, running",
		"    Becomes a Compose project, wordpress, taken on as it runs, with no restart.",
		"Later, once Dockwell has websites (1)",
		"  Website blog.example.com: a static site",
		"Stays with 1Panel (1)",
		"  Firewall: the rules 1Panel writes",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("the output has no %q:\n%s", want, r.out)
		}
	}
}
