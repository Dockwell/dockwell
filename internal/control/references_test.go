// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// redisFile has the shape real files give a password in a command, which 1Panel's store
// has too, and none of its words, as it is GPL-3.0.
const redisFile = "services:\n  redis:\n    image: redis:8\n" +
	"    command: redis-server ${REDIS_PASSWORD:+--requirepass \"${REDIS_PASSWORD}\"}\n" +
	"    healthcheck:\n      test: [\"CMD\", \"redis-cli\", \"-a\", \"${REDIS_PASSWORD}\", \"ping\"]\n"

// everyReadSays asks everything a read-only token can read of an environment, and gives
// back all of it as text: what a change planned and what became of it, each service and
// its revisions, an export, the timeline and what servers say of health.
func everyReadSays(t *testing.T, ro connect.HTTPClient, origin string, changeIDs ...string) string {
	t.Helper()
	var all strings.Builder
	say := func(m proto.Message) {
		b, err := protojson.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(b)
		all.WriteByte('\n')
	}
	c := changeClient(ro, origin)
	for _, id := range changeIDs {
		got, err := c.GetChange(t.Context(), connect.NewRequest(&dockwellv1.GetChangeRequest{Id: id}))
		if err != nil {
			t.Fatal(err)
		}
		say(got.Msg)
	}
	listed, err := c.ListChanges(t.Context(), connect.NewRequest(&dockwellv1.ListChangesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	say(listed.Msg)
	drift, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	say(drift.Msg)
	s := dockwellv1connect.NewServiceServiceClient(ro, origin, connect.WithProtoJSON())
	services, err := s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	say(services.Msg)
	for _, svc := range services.Msg.GetServices() {
		revisions, err := s.ListServiceRevisions(t.Context(), connect.NewRequest(&dockwellv1.ListServiceRevisionsRequest{Id: svc.GetId()}))
		if err != nil {
			t.Fatal(err)
		}
		say(revisions.Msg)
	}
	exported, err := dockwellv1connect.NewComposeServiceClient(ro, origin, connect.WithProtoJSON()).ExportCompose(t.Context(), connect.NewRequest(&dockwellv1.ExportComposeRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	say(exported.Msg)
	for _, ev := range listEvents(t, eventClient(ro, origin), &dockwellv1.ListEventsRequest{}) {
		say(ev)
	}
	return all.String()
}

// noValueIn fails where a value is in what is read, as it is or in any encoding.
func noValueIn(t *testing.T, what, text string, values ...string) {
	t.Helper()
	for _, value := range values {
		for _, enc := range encodings(value) {
			if strings.Contains(text, enc) {
				t.Errorf("%s holds a value, as %q", what, enc)
			}
		}
	}
}

// noValueStored fails where a value is in any row of any table.
func noValueStored(t *testing.T, f *fullServer, values ...string) {
	t.Helper()
	for table, rows := range storetest.Dump(t, f.db) {
		for _, row := range rows {
			noValueIn(t, "the table "+table, row, values...)
		}
	}
}

func commandOf(e *dockertest.Engine, project, name string) []string {
	c := e.Newest(project, name)
	if c == nil {
		return nil
	}
	return c.Config.Cmd
}

// A password a Compose file puts in a command runs, and shows nowhere: not in a plan, the
// store, an export or anything a read-only token reads. What each of those shows is the
// name. A rotation is a change, which says so without the value, and undoing it goes back
// to the value it had. An export writes ${NAME}, importing that again with the value plans
// nothing, and a read-back sees nothing to do.
func TestAPasswordInACommandStaysOutOfEverything(t *testing.T) {
	storetest.Each(t, testAPasswordInACommandStaysOutOfEverything)
}

func testAPasswordInACommandStaysOutOfEverything(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.AddLocalImage("redis:8", dockertest.Image{})
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	c := changeClient(b, b.origin)
	const name = "REDIS_PASSWORD"
	const first, second, third = "s3cr3t7Hq2mXv9Lr4Kd8Zs", "r0tated5Lt1oGy7Ce3Bn9Wf", "sentButN0tApplied6Qd2"
	values := []string{first, second, third}

	// From an .env, as a file beside it.
	made := importIn(t, cc, &dockwellv1.ImportComposeRequest{Compose: redisFile, Env: name + "=" + first + "\n"})
	notes := func(ch *dockwellv1.Change) []*dockwellv1.PlanNote {
		var out []*dockwellv1.PlanNote
		for _, n := range ch.GetPlan().GetNotes() {
			if n.GetReason() == "exposed_variable" {
				out = append(out, n)
			}
		}
		return out
	}
	if got := notes(made); len(got) != 0 {
		t.Errorf("the plan warns of what it kept out: %v", got)
	}
	shows := func(ch *dockwellv1.Change, path string) string {
		t.Helper()
		for _, r := range ch.GetPlan().GetResources() {
			for _, change := range r.GetChanges() {
				if change.GetPath() == path {
					return change.GetTo()
				}
			}
		}
		return ""
	}
	if got := shows(made, "app.command"); !strings.Contains(got, "${"+name+"}") {
		t.Errorf("an approver reads the command as %q", got)
	}
	if got := shows(made, "app.healthcheck.test"); !strings.Contains(got, "${"+name+"}") {
		t.Errorf("an approver reads the test as %q", got)
	}
	apply(t, c, made)
	project := docker.Project(listedEnv(t, b))
	if got := commandOf(e, project, "redis"); !slices.Contains(got, first) || !slices.Contains(got, "--requirepass") {
		t.Fatalf("Docker runs %q; the test didn't run what it meant to", got)
	}
	if got := e.Newest(project, "redis").Config.Healthcheck.Test; !slices.Contains(got, first) {
		t.Fatalf("Docker checks %q", got)
	}
	ro := bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY), rt: b.client.Transport}
	noValueIn(t, "what a read-only token reads", everyReadSays(t, ro, b.origin, made.GetId()), values...)
	noValueStored(t, f, values...)
	drift, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil || len(drift.Msg.GetDrift()) != 0 {
		t.Errorf("drift right after the change = %v, %v", drift.Msg.GetDrift(), err)
	}
	id := e.Newest(project, "redis").ID

	// The same file with the same value plans nothing, from the request as from an .env.
	for what, req := range map[string]*dockwellv1.ImportComposeRequest{
		"from an .env":   {Compose: redisFile, Env: name + "=" + first + "\n"},
		"from variables": {Compose: redisFile, Variables: map[string]string{name: first}},
	} {
		again := importIn(t, cc, req)
		if !plansNothing(again.GetPlan()) {
			t.Errorf("importing it again %s plans %v", what, again.GetPlan().GetResources())
		}
		discard(t, c, again)
	}

	// An export writes the name. Taking it back with the value plans nothing, and without it asks.
	exported, err := cc.ExportCompose(t.Context(), connect.NewRequest(&dockwellv1.ExportComposeRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	file := exported.Msg.GetCompose()
	if !strings.Contains(file, "${"+name+"}") || !slices.Contains(exported.Msg.GetOmitted(), "redis."+name) {
		t.Errorf("the export says %v:\n%s", exported.Msg.GetOmitted(), file)
	}
	if _, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: "default", Compose: file})); err == nil || !strings.Contains(err.Error(), "the Compose file uses "+name+", with no value") {
		t.Errorf("importing the export with no value: %v", err)
	}
	back := importIn(t, cc, &dockwellv1.ImportComposeRequest{Project: "default", Compose: file, Variables: map[string]string{name: first}})
	if !plansNothing(back.GetPlan()) {
		t.Errorf("importing the export plans %v", back.GetPlan().GetResources())
	}
	discard(t, c, back)

	// A new value is a change that says so, and reaches what runs.
	planned := importIn(t, cc, &dockwellv1.ImportComposeRequest{Compose: redisFile, Variables: map[string]string{name: third}})
	if plansNothing(planned.GetPlan()) {
		t.Error("a new value plans nothing")
	}
	if planned.GetPlan().GetFingerprint() == back.GetPlan().GetFingerprint() {
		t.Error("a plan's fingerprint doesn't depend on the value it sets")
	}
	discard(t, c, planned)
	rotated := importIn(t, cc, &dockwellv1.ImportComposeRequest{Compose: redisFile, Variables: map[string]string{name: second}})
	var changed []string
	for _, r := range rotated.GetPlan().GetResources() {
		for _, change := range r.GetChanges() {
			changed = append(changed, change.GetPath())
			if !change.GetSensitive() {
				t.Errorf("%s changes, and isn't masked: %v", change.GetPath(), change)
			}
		}
	}
	if !slices.Equal(changed, []string{"app.env[" + name + "]"}) {
		t.Errorf("a new value changes %v", changed)
	}
	apply(t, c, rotated)
	if got := commandOf(e, project, "redis"); !slices.Contains(got, second) || slices.Contains(got, first) {
		t.Errorf("after the change Docker runs %q", got)
	}
	if e.Newest(project, "redis").ID == id {
		t.Error("the change didn't make a new container")
	}

	// An undo puts back the value it had, and says no more than the change did.
	undo, err := c.UndoChange(t.Context(), connect.NewRequest(&dockwellv1.UndoChangeRequest{Id: rotated.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, undo.Msg.GetChange())
	if got := commandOf(e, project, "redis"); !slices.Contains(got, first) {
		t.Errorf("after the undo Docker runs %q", got)
	}
	noValueIn(t, "what a read-only token reads", everyReadSays(t, ro, b.origin, made.GetId(), rotated.GetId(), undo.Msg.GetChange().GetId()), values...)
	noValueStored(t, f, values...)
	drift, err = c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil || len(drift.Msg.GetDrift()) != 0 {
		t.Errorf("drift after it all = %v, %v", drift.Msg.GetDrift(), err)
	}
}

// A service's own secret, named in its command: the value goes only to where it runs, a
// rotation reaches it and a rollback keeps today's value. It can't be guessed at through
// the file or the plan, and a file names it with no value of its own.
func TestASecretInACommandIsARotationAndNoOracle(t *testing.T) {
	storetest.Each(t, testASecretInACommandIsARotationAndNoOracle)
}

func secretNaming(image string) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "redis"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs: &resourcev1.Service_App{App: &resourcev1.Container{
				Image: image, Command: []string{"redis-server", "--requirepass", "${PW}"},
				References: []string{"command"},
			}},
			Secrets: map[string]string{"PW": "DB_PASSWORD"},
		}}},
	}
}

func testASecretInACommandIsARotationAndNoOracle(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addDockerServer(t)
	hz1.AddLocalImage("redis:8", dockertest.Image{})
	hz1.AddLocalImage("redis:9", dockertest.Image{})
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	s := dockwellv1connect.NewServiceServiceClient(b, b.origin, connect.WithProtoJSON())
	const first, second = "pw-alpha-7Hq2mXv9Lr4Kd8Zs", "pw-beta-3Fn6pWc1Tj5Yb2Ug"
	values := []string{first, second}
	made := apply(t, c, propose(t, c, "Run redis", secretSpec("DB_PASSWORD", first), secretNaming("redis:8")))
	project := docker.Project(listedEnv(t, b))
	if got := commandOf(hz1, project, "redis"); !slices.Equal(got, []string{"redis-server", "--requirepass", first}) {
		t.Fatalf("Docker runs %q", got)
	}
	ro := bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_ONLY), rt: b.client.Transport}
	noValueIn(t, "what a read-only token reads", everyReadSays(t, ro, b.origin, made.GetId()), values...)
	drift, err := c.ListDrift(t.Context(), connect.NewRequest(&dockwellv1.ListDriftRequest{}))
	if err != nil || len(drift.Msg.GetDrift()) != 0 {
		t.Errorf("drift right after the change = %v, %v", drift.Msg.GetDrift(), err)
	}

	noValueStored(t, f, values...)

	// An export names it, and taking it back needs no value and plans nothing.
	exported, err := cc.ExportCompose(t.Context(), connect.NewRequest(&dockwellv1.ExportComposeRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	file := exported.Msg.GetCompose()
	if !strings.Contains(file, "${PW}") || strings.Contains(file, first) {
		t.Errorf("the export says:\n%s", file)
	}
	same := importIn(t, cc, &dockwellv1.ImportComposeRequest{Project: "default", Compose: file})
	if !plansNothing(same.GetPlan()) {
		t.Errorf("importing the export plans %v", same.GetPlan().GetResources())
	}
	discard(t, c, same)

	// A guess in a file is refused as set twice, whether right or wrong, in the same words.
	guess := func(value string) string {
		t.Helper()
		_, err := cc.ImportCompose(t.Context(), connect.NewRequest(&dockwellv1.ImportComposeRequest{Project: "default", Compose: file, Variables: map[string]string{"PW": value}}))
		if err == nil {
			t.Fatalf("a file gave the secret a value, and it took it")
		}
		return err.Error()
	}
	if right, wrong := guess(first), guess("not-the-password"); right != wrong || !strings.Contains(right, "from a secret too") {
		t.Errorf("the right guess says %q, and a wrong one %q", right, wrong)
	} else if strings.Contains(right, first) {
		t.Errorf("it repeats the value: %q", right)
	}

	// As a literal in the command, a token's plan reads alike whether it is right or wrong.
	script := bearer{token: createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE), rt: b.client.Transport}
	tc := changeClient(script, b.origin)
	literal := func(value string) *dockwellv1.Plan {
		t.Helper()
		edit := secretNaming("redis:8")
		edit.Spec.GetService().GetApp().Command = []string{"redis-server", "--requirepass", value}
		edit.Spec.GetService().GetApp().References = nil
		edit.Spec.GetService().Secrets = nil
		ch := propose(t, tc, "Guess", edit)
		discard(t, tc, ch)
		return ch.GetPlan()
	}
	right, wrong := literal(first), literal("not-the-password")
	if right.GetSummary().GetChange()+right.GetSummary().GetReplace() == 0 {
		t.Errorf("the right guess plans %v", right.GetSummary())
	}
	if !proto.Equal(right.GetSummary(), wrong.GetSummary()) {
		t.Errorf("the right guess plans %v, and a wrong one %v", right.GetSummary(), wrong.GetSummary())
	}
	steps := func(p *dockwellv1.Plan) []string {
		var out []string
		for _, server := range p.GetServers() {
			for _, step := range server.GetSteps() {
				var paths []string
				for _, change := range step.GetChanges() {
					paths = append(paths, change.GetPath())
				}
				out = append(out, step.GetAction().String()+" "+strings.Join(paths, " "))
			}
		}
		return out
	}
	if !slices.Equal(steps(right), steps(wrong)) {
		t.Errorf("the right guess's steps are %v, and a wrong one's %v", steps(right), steps(wrong))
	}

	// A rotation reaches it, and a deploy after that carries it on.
	rotated := apply(t, c, propose(t, c, "Rotate", secretSpec("DB_PASSWORD", second)))
	if got := commandOf(hz1, project, "redis"); !slices.Equal(got, []string{"redis-server", "--requirepass", second}) {
		t.Errorf("after the rotation Docker runs %q", got)
	}
	apply(t, c, propose(t, c, "A new image", secretNaming("redis:9")))
	if got := commandOf(hz1, project, "redis"); !slices.Equal(got, []string{"redis-server", "--requirepass", second}) {
		t.Errorf("after the deploy Docker runs %q", got)
	}

	// A rollback puts back what the revision ran, at today's value.
	services, err := s.ListServices(t.Context(), connect.NewRequest(&dockwellv1.ListServicesRequest{}))
	if err != nil || len(services.Msg.GetServices()) != 1 {
		t.Fatalf("ListServices = %v, %v", services, err)
	}
	web := services.Msg.GetServices()[0]
	back, err := s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: 1}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, back.Msg.GetChange())
	got := hz1.Newest(project, "redis")
	if got.Config.Image != "redis:8" || !slices.Equal(got.Config.Cmd, []string{"redis-server", "--requirepass", second}) {
		t.Errorf("after the rollback Docker runs %s %q", got.Config.Image, got.Config.Cmd)
	}
	// The guess above is what someone wrote in a command as text, which a plan shows on
	// purpose, so this holds for what no one wrote there.
	noValueIn(t, "what a read-only token reads", everyReadSays(t, ro, b.origin, made.GetId(), rotated.GetId(), back.Msg.GetChange().GetId()), second)
	noValueStored(t, f, second)

	// What a service names can't be deleted while it does, as for anything it uses, and a
	// revision that names what has gone since can't come back.
	_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Delete it", Edits: []*dockwellv1.Edit{
		{Resource: &dockwellv1.ResourceRef{Kind: "secret", Name: "DB_PASSWORD"}, Delete: true},
	}}))
	if err == nil || !strings.Contains(err.Error(), "uses") {
		t.Errorf("deleting what it names = %v", err)
	}
	plain := &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Name: "redis"}, Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
		Runs: &resourcev1.Service_App{App: &resourcev1.Container{Image: "redis:8", Command: []string{"redis-server"}}},
	}}}}
	apply(t, c, propose(t, c, "Stop using it", plain, &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: "secret", Name: "DB_PASSWORD"}, Delete: true}))
	if _, err := s.RollBackService(t.Context(), connect.NewRequest(&dockwellv1.RollBackServiceRequest{Id: web.GetId(), Revision: 1})); err == nil || !strings.Contains(err.Error(), "DB_PASSWORD") {
		t.Errorf("rolling back to what names a deleted secret = %v", err)
	}
}

// A server gets the values of what runs on it, and a name in a command is no way round
// that: another server asking for this service's values is told no, by the same keeper.
func TestANameInACommandTakesAValueNowhereElse(t *testing.T) {
	storetest.Each(t, testANameInACommandTakesAValueNowhereElse)
}

func testANameInACommandTakesAValueNowhereElse(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addServerNamed(t, "hz-1", nil, nil)
	hz1.AddLocalImage("redis:8", dockertest.Image{})
	one := f.lastDocker
	hz2 := f.addServerNamed(t, "hz-2", nil, nil)
	c := changeClient(b, b.origin)
	edit := secretNaming("redis:8")
	edit.Servers = []string{one.id.ServerID}
	const value = "pw-only-for-hz-1-3Fn6pWc1"
	apply(t, c, propose(t, c, "Run redis", secretSpec("DB_PASSWORD", value), edit))
	project := docker.Project(listedEnv(t, b))
	if got := commandOf(hz1, project, "redis"); !slices.Contains(got, value) {
		t.Fatalf("hz-1 runs %q", got)
	}
	if len(hz2.Containers()) != 0 {
		t.Errorf("hz-2 has %v", hz2.Containers())
	}
	var spec proto.Message
	ref := resource.Ref{Kind: service.Kind, Scope: listedEnv(t, b), Name: "redis"}
	err := f.store.View(t.Context(), func(tx *store.Tx) error {
		r, err := tx.GetResource(t.Context(), string(service.Kind), ref.Scope, ref.Name)
		if err != nil {
			return err
		}
		spec, err = f.keeper.Decode(t.Context(), tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.keeper.Values(context.Background(), "srv_someone_else", ref, spec); err == nil || strings.Contains(err.Error(), value) {
		t.Errorf("another server asking = %v", err)
	}
}

// What is left of a value a file writes as text, and only that, comes with a warning that
// says why and what to do. What a label says it used stays, and reads back.
func TestWhatStaysTextIsWarnedOfAndNothingElse(t *testing.T) {
	storetest.Each(t, testWhatStaysTextIsWarnedOfAndNothingElse)
}

func testWhatStaysTextIsWarnedOfAndNothingElse(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.AddLocalImage("vllm:1", dockertest.Image{})
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	c := changeClient(b, b.origin)
	file := "services:\n  vllm:\n    image: vllm:1\n    command: ${START_COMMAND}\n"
	vars := map[string]string{"START_COMMAND": "--model foo --port 8000"}
	checked, err := cc.CheckCompose(t.Context(), connect.NewRequest(&dockwellv1.CheckComposeRequest{Compose: file}))
	if err != nil {
		t.Fatal(err)
	}
	if got := checked.Msg.GetExposedVariables(); len(got) != 0 {
		t.Errorf("before it has a value, the check warns of %v", got)
	}
	checked, err = cc.CheckCompose(t.Context(), connect.NewRequest(&dockwellv1.CheckComposeRequest{Compose: file, Variables: vars}))
	if err != nil {
		t.Fatal(err)
	}
	if got := checked.Msg.GetExposedVariables(); len(got) != 1 || got[0].GetName() != "START_COMMAND" || got[0].GetField() != "command" {
		t.Errorf("once it has one, the check warns of %v", got)
	}
	ch := importIn(t, cc, &dockwellv1.ImportComposeRequest{Compose: file, Variables: vars})
	notes := ch.GetPlan().GetNotes()
	var note *dockwellv1.PlanNote
	for _, n := range notes {
		if n.GetReason() == "exposed_variable" {
			note = n
		}
	}
	want := "START_COMMAND from the request's variables is in vllm's command, so anyone who can read this change or export this environment can read it. " +
		"It is more than one word there, and one name can't stand for that. To keep it hidden, write the command as a list, with each word its own variable"
	if note == nil || note.GetMessage() != want {
		t.Errorf("the plan says %v, want %q", note, want)
	}
	apply(t, c, ch)
	if got := commandOf(e, docker.Project(listedEnv(t, b)), "vllm"); !slices.Equal(got, []string{"--model", "foo", "--port", "8000"}) {
		t.Errorf("Docker runs %q", got)
	}
}

// The label a container carries holds names, and goes round through an export as it
// is, $ and all, so a container docker compose makes from one reads back the same.
func TestAReferencesLabelReadsBackFromDockerCompose(t *testing.T) {
	storetest.Each(t, testAReferencesLabelReadsBackFromDockerCompose)
}

func testAReferencesLabelReadsBackFromDockerCompose(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	e.AddLocalImage("redis:8", dockertest.Image{})
	cc := dockwellv1connect.NewComposeServiceClient(b, b.origin, connect.WithProtoJSON())
	c := changeClient(b, b.origin)
	const value = "s3cr3t7Hq2mXv9Lr4Kd8Zs"
	file := "services:\n  redis:\n    image: redis:8\n    command: [redis-server, --requirepass, \"${PW}\", --dir, \"$$HOME\"]\n"
	apply(t, c, importIn(t, cc, &dockwellv1.ImportComposeRequest{Compose: file, Variables: map[string]string{"PW": value}}))
	project := docker.Project(listedEnv(t, b))
	running := e.Newest(project, "redis")
	var words map[string][]string
	if err := json.Unmarshal([]byte(running.Config.Labels[container.LabelReferences]), &words); err != nil {
		t.Fatalf("the label reads %q: %v", running.Config.Labels[container.LabelReferences], err)
	}
	if !slices.Equal(words["command"], []string{"redis-server", "--requirepass", "${PW}", "--dir", "$$HOME"}) {
		t.Errorf("the label holds %v", words)
	}
	if want := []string{"redis-server", "--requirepass", value, "--dir", "$HOME"}; !slices.Equal(running.Config.Cmd, want) {
		t.Errorf("Docker runs %q, want %q", running.Config.Cmd, want)
	}
	exported, err := cc.ExportCompose(t.Context(), connect.NewRequest(&dockwellv1.ExportComposeRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(exported.Msg.GetCompose(), `dev.dockwell.references: '{"command":["redis-server","--requirepass","$${PW}","--dir","$$$$HOME"]}'`) {
		t.Errorf("the export's label reads:\n%s", exported.Msg.GetCompose())
	}
}
