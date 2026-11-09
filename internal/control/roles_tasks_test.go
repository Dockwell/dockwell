// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A task's name is found among the caller's projects, as its ID is checked.
// Its runs, including their output and server, belong to that task. Narrowing
// leaves them whole and seals each page token for the caller.
func TestTaskNamesAndRunsStayWithinTheirProjects(t *testing.T) {
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		s := newReachStand(t, backend)
		ctx := t.Context()
		tasks := dockwellv1connect.NewTaskServiceClient(s.b, s.b.origin, connect.WithProtoJSON())
		makeTask := func(env, name, output string) string {
			t.Helper()
			spec := shellTask("echo "+output, "@daily")
			spec.Paused = true
			edit := taskEdit(name, spec)
			edit.Resource.Scope = env
			made := propose(t, s.c, "Make "+name, edit)
			applyAsRoot(t, s.b, made)
			return resourceIn(t, made, "task")
		}
		foreign := makeTask(s.production, "daily", "default-output")
		own := makeTask(s.staging, "daily", "blog-output")
		makeTask(s.production, "outside", "outside-output")
		for range 3 {
			ranTask(t, tasks, own)
		}
		ranTask(t, tasks, foreign)
		before, err := tasks.GetTask(ctx, connect.NewRequest(&dockwellv1.GetTaskRequest{Id: own}))
		if err != nil {
			t.Fatal(err)
		}
		logs, _ := logToken(t, s.b, dockwellv1.TokenGrant_TOKEN_GRANT_LOGS)
		noLogs := createToken(t, s.b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY)
		writes := createToken(t, s.b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
		script := dockwellv1connect.NewTaskServiceClient(bearer{token: logs, rt: s.b.client.Transport}, s.b.origin, connect.WithProtoJSON())
		s.hold(in(s.blogID, store.RoleViewer))

		var firstToken string
		for _, client := range []dockwellv1connect.TaskServiceClient{tasks, script} {
			list, err := client.ListTasks(ctx, connect.NewRequest(&dockwellv1.ListTasksRequest{}))
			if err != nil || len(list.Msg.GetTasks()) != 1 || !proto.Equal(list.Msg.GetTasks()[0], before.Msg.GetTask()) {
				t.Errorf("blog's task list = %v, %v; want its complete task alone", list, err)
			}
			for _, req := range []*dockwellv1.GetTaskRequest{
				{Id: own},
				{Id: "daily"},
				{Id: "daily", Project: "blog"},
				{Id: "daily", Project: s.blogID, Environment: s.staging},
				{Id: "daily", Environment: s.staging},
				{Id: "daily", Project: "blog", Environment: "staging"},
			} {
				got, err := client.GetTask(ctx, connect.NewRequest(req))
				if err != nil || !proto.Equal(got.Msg.GetTask(), before.Msg.GetTask()) {
					t.Errorf("reading %v = %v, %v; want blog's task with its servers", req, got, err)
				}
			}
			_, missing := client.GetTask(ctx, connect.NewRequest(&dockwellv1.GetTaskRequest{Id: "missing", Project: "blog"}))
			notFound(t, "a missing task", missing)
			for _, req := range []*dockwellv1.GetTaskRequest{
				{Id: foreign},
				{Id: "outside"},
				{Id: "outside", Environment: "production"},
				{Id: "daily", Project: "default"},
				{Id: "daily", Project: s.defaultID, Environment: s.production},
				{Id: "missing"},
				{Id: "tsk_01NOTHINGHERE0000000000000"},
				{Id: "daily", Project: "missing"},
				{Id: "daily", Project: "blog", Environment: "missing"},
			} {
				_, got := client.GetTask(ctx, connect.NewRequest(req))
				notFound(t, "an unreachable or missing task "+req.String(), got)
				if got != nil && missing != nil && got.Error() != missing.Error() {
					t.Errorf("%v says %q, a missing task %q", req, got, missing)
				}
				_, err := client.ListTaskRuns(ctx, connect.NewRequest(&dockwellv1.ListTaskRunsRequest{
					Id: req.GetId(), Project: req.GetProject(), Environment: req.GetEnvironment(),
				}))
				notFound(t, "runs outside blog's reach", err)
			}
			// An ID and a name filter the same approved server, never an install object.
			for _, server := range []string{s.server, "hz-1"} {
				pageToken := ""
				var seen []string
				for range 4 {
					page, err := client.ListTaskRuns(ctx, connect.NewRequest(&dockwellv1.ListTaskRunsRequest{
						Id: own, Server: server, PageSize: 1, PageToken: pageToken,
					}))
					if err != nil {
						t.Fatal(err)
					}
					if len(page.Msg.GetRuns()) != 1 {
						t.Fatalf("one run per page = %v, want a complete run", page.Msg)
					}
					run := page.Msg.GetRuns()[0]
					if run.GetTaskId() != own || run.GetServerId() != s.server || !slices.Contains(outputOf(run), "blog-output") || slices.Contains(seen, run.GetId()) {
						t.Errorf("a task's run is incomplete, foreign or repeated: %v", run)
					}
					seen = append(seen, run.GetId())
					pageToken = page.Msg.GetNextPageToken()
					if pageToken == "" {
						break
					}
					if !strings.HasPrefix(pageToken, "dwp1.") || strings.Contains(pageToken, run.GetId()) {
						t.Errorf("page token isn't sealed: %q", pageToken)
					}
					firstToken = pageToken
				}
				if len(seen) != 3 {
					t.Errorf("paged runs = %v, want all three", seen)
				}
			}
			_, err = client.ListTaskRuns(ctx, connect.NewRequest(&dockwellv1.ListTaskRunsRequest{Id: own, PageToken: "dwp1.invalid"}))
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("a forged page token = %v", err)
			}
			_, err = client.ListTaskRuns(ctx, connect.NewRequest(&dockwellv1.ListTaskRunsRequest{Id: foreign, PageToken: firstToken}))
			notFound(t, "an own page token cannot reach another task", err)
		}

		without := dockwellv1connect.NewTaskServiceClient(bearer{token: noLogs, rt: s.b.client.Transport}, s.b.origin, connect.WithProtoJSON())
		if _, err := s.f.auth.OpenPageToken("usr_keeper", firstToken); err == nil {
			t.Error("another person could open a task run's page token")
		}
		if _, err := without.GetTask(ctx, connect.NewRequest(&dockwellv1.GetTaskRequest{Id: own})); err != nil {
			t.Errorf("a token without logs reading its task's summary: %v", err)
		}
		_, err = without.ListTaskRuns(ctx, connect.NewRequest(&dockwellv1.ListTaskRunsRequest{Id: own}))
		if connect.CodeOf(err) != connect.CodePermissionDenied || reasonOf(err) != "token_logs" {
			t.Errorf("a token without logs reading run output: %v", err)
		}
		for _, headers := range []map[string]string{nil, {"Authorization": "Bearer " + logs}} {
			if code, _, body := callREST(t, s.b, http.MethodGet, "/v1/tasks", headers, ""); code != http.StatusOK || !strings.Contains(body, own) || strings.Contains(body, foreign) {
				t.Errorf("REST task list = %d %s", code, body)
			}
			if code, _, body := callREST(t, s.b, http.MethodGet, "/v1/tasks/daily?project=blog", headers, ""); code != http.StatusOK || !strings.Contains(body, own) {
				t.Errorf("REST task name = %d %s", code, body)
			}
			if code, reason, body := callREST(t, s.b, http.MethodGet, "/v1/tasks/daily?project=default", headers, ""); code != http.StatusNotFound || reason != "not_found" {
				t.Errorf("REST foreign task = %d %s %s", code, reason, body)
			}
			code, _, body := callREST(t, s.b, http.MethodGet, "/v1/tasks/daily/runs?pageSize=1", headers, "")
			page := &dockwellv1.ListTaskRunsResponse{}
			if err := protojson.Unmarshal([]byte(body), page); code != http.StatusOK || err != nil || len(page.GetRuns()) != 1 || page.GetNextPageToken() == "" {
				t.Errorf("REST run page = %d %s, %v", code, body, err)
			} else if code, _, body := callREST(t, s.b, http.MethodGet, "/v1/tasks/daily/runs?pageSize=1&pageToken="+url.QueryEscape(page.GetNextPageToken()), headers, ""); code != http.StatusOK || !strings.Contains(body, "blog-output") {
				t.Errorf("REST next run page = %d %s", code, body)
			}
		}
		// A task may run root commands, so a developer or viewer cannot run it now.
		writeClient := dockwellv1connect.NewTaskServiceClient(bearer{token: writes, rt: s.b.client.Transport}, s.b.origin, connect.WithProtoJSON())
		for _, role := range []string{store.RoleViewer, store.RoleDeveloper} {
			s.hold(in(s.blogID, role))
			for _, client := range []dockwellv1connect.TaskServiceClient{tasks, writeClient} {
				_, err := client.RunTask(ctx, connect.NewRequest(&dockwellv1.RunTaskRequest{Id: own}))
				needsRole(t, "a "+role+" manually running a task", err)
			}
			if code, reason, body := callREST(t, s.b, http.MethodPost, "/v1/tasks/"+own+":run", nil, "{"); code != http.StatusForbidden || reason != "needs_role" {
				t.Errorf("REST Run now is refused before parsing = %d %s %s", code, reason, body)
			}
		}
		// Once the role moves, neither task IDs, names nor old cursors keep access.
		s.hold(in(s.defaultID, store.RoleViewer))
		_, err = tasks.ListTaskRuns(ctx, connect.NewRequest(&dockwellv1.ListTaskRunsRequest{Id: own, PageToken: firstToken}))
		notFound(t, "runs after the project grant moved", err)
		got, err := script.GetTask(ctx, connect.NewRequest(&dockwellv1.GetTaskRequest{Id: "daily"}))
		if err != nil || got.Msg.GetTask().GetId() != foreign {
			t.Errorf("a token's task name after its maker's grant moved = %v, %v", got, err)
		}
		s.hold(onInstall(store.RoleViewer))
		_, err = tasks.GetTask(ctx, connect.NewRequest(&dockwellv1.GetTaskRequest{Id: "daily"}))
		if reasonOf(err) != "choice" || !strings.Contains(err.Error(), "blog and default") {
			t.Errorf("a name in two reachable projects = %v, want a choice between them", err)
		}
		list, err := tasks.ListTasks(ctx, connect.NewRequest(&dockwellv1.ListTasksRequest{}))
		if err != nil || len(list.Msg.GetTasks()) != 3 {
			t.Errorf("an install viewer's task list = %v, %v, want all three", list, err)
		}
	})
}
