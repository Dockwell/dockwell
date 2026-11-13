// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// fromSecret is a secret whose value is written from a template of the environment's
// other values (ADR 0084).
func fromSecret(name string, t *resourcev1.ValueTemplate) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: name},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{
			From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Template{Template: t}},
		}}},
	}
}

// A URL with a password in it is written from the environment's values, each part
// encoded for its place, and a service uses it as a whole value. Setting the password
// anew takes in the URL and the service that uses it, in the same plan, and the
// service starts with the URL written again; no plan or list shows the URL.
func TestAValueWrittenFromOthersFollowsThem(t *testing.T) {
	storetest.Each(t, testAValueWrittenFromOthersFollowsThem)
}

func testAValueWrittenFromOthersFollowsThem(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addNamedServer(t, "hz-1", true)
	c := changeClient(b, b.origin)
	url := &resourcev1.ValueTemplate{
		Text: "postgres://${DB_USER}:${DB_PASSWORD}@${DB_ADDRESS}/${DB_NAME}",
		Names: []*resourcev1.TemplateName{
			{Name: "DB_USER", Encoding: resourcev1.Encoding_ENCODING_USERINFO},
			{Name: "DB_PASSWORD", Secret: true, Encoding: resourcev1.Encoding_ENCODING_USERINFO},
			{Name: "DB_ADDRESS"},
			{Name: "DB_NAME", Encoding: resourcev1.Encoding_ENCODING_PATH},
		},
	}
	apply(t, c, propose(t, c, "Run the shop",
		variableSpec("DB_USER", "app@server"), secretSpec("DB_PASSWORD", "first#one"),
		variableSpec("DB_ADDRESS", "db:5432"), variableSpec("DB_NAME", "shop"),
		fromSecret("DATABASE_URL", url),
		serviceSpec("web", map[string]string{"DATABASE_URL": "DATABASE_URL"}, hz1.id.ServerID),
	))
	env := listedEnv(t, b)
	envOf := func() map[string]string {
		t.Helper()
		got, err := hz1.docker.Inspect(t.Context(), service.Target(resource.Ref{Scope: env, Name: "web"}))
		if err != nil || got == nil {
			t.Fatalf("web: %v, %v", got, err)
		}
		return got.GetEnv()
	}
	if got := envOf()["DATABASE_URL"]; got != "postgres://app%40server:first%23one@db:5432/shop" {
		t.Fatalf("web reads DATABASE_URL = %q", got)
	}

	rotate := propose(t, c, "Set the password anew", secretSpec("DB_PASSWORD", "second/one"))
	changed := map[string]bool{}
	for _, r := range rotate.GetPlan().GetResources() {
		if r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE {
			changed[r.GetResource().GetKind()+" "+r.GetResource().GetName()] = true
		}
		for _, ch := range r.GetChanges() {
			if ch.GetTo() == "postgres://app%40server:second%2Fone@db:5432/shop" || ch.GetTo() == "second/one" {
				t.Errorf("the plan shows a value: %v", ch)
			}
		}
	}
	for _, want := range []string{"secret DB_PASSWORD", "secret DATABASE_URL", "service web"} {
		if !changed[want] {
			t.Errorf("setting the password anew doesn't take in %s: %v", want, changed)
		}
	}
	if got := apply(t, c, rotate); got.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("the rotation ended %v: %s", got.GetState(), got.GetError())
	}
	if got := envOf()["DATABASE_URL"]; got != "postgres://app%40server:second%2Fone@db:5432/shop" {
		t.Errorf("after the rotation web reads DATABASE_URL = %q", got)
	}

	// A template that names what isn't there is refused when it's planned.
	_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{
		Summary: "A URL of nothing",
		Edits: []*dockwellv1.Edit{fromSecret("OTHER_URL", &resourcev1.ValueTemplate{
			Text: "redis://${REDIS_HOST}", Names: []*resourcev1.TemplateName{{Name: "REDIS_HOST"}},
		})},
	}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a template of a value that isn't there = %v", err)
	}
}

// A link's value from another environment reads the password of a database user
// there, as the link would make one: setting it anew takes in the value and the app
// that uses it, across projects. A value from another environment that no link
// writes is refused, and so is one that reads what a service uses there, as an
// administrator's password is.
func TestAValueFromAnotherEnvironmentIsALinksAlone(t *testing.T) {
	storetest.Each(t, testAValueFromAnotherEnvironmentIsALinksAlone)
}

func testAValueFromAnotherEnvironmentIsALinksAlone(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	hz1 := f.addNamedServer(t, "hz-1", true)
	c := changeClient(b, b.origin)
	blog := listedEnv(t, b)
	apply(t, c, propose(t, c, "Add the data project", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "data"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: "Data"}}},
	}))
	var data string
	_ = f.store.View(t.Context(), func(tx *store.Tx) error {
		org, _ := tx.Organization(t.Context())
		p, err := tx.GetResource(t.Context(), "project", org.ID, "data")
		if err != nil {
			return err
		}
		e, err := tx.GetResource(t.Context(), "environment", p.ID, "production")
		data = e.ID
		return err
	})
	in := func(scope string, e *dockwellv1.Edit) *dockwellv1.Edit {
		e.Resource.Scope = scope
		return e
	}
	apply(t, c, propose(t, c, "Run the shared database",
		in(data, secretSpec("ADMIN_PASSWORD", "the-administrators")),
		in(data, secretSpec("BLOG_DB_PASSWORD", "blogs-own-1")),
		in(data, serviceSpec("db", map[string]string{"POSTGRES_PASSWORD": "ADMIN_PASSWORD"}, hz1.id.ServerID)),
	))
	fromData := func(name, link string) *dockwellv1.Edit {
		return in(blog, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Name: "DB_PASSWORD"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: &resourcev1.ValueFrom{
				Source: &resourcev1.ValueFrom_Value{Value: &resourcev1.ValueRef{Environment: data, Secret: true, Name: name, Link: link}},
			}}}},
		})
	}
	theLink := in(blog, &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "db"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
			Label: "Database", Service: "db", MeetsEnvironment: data, Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL, Version: "17",
			Accepts:      []*resourcev1.LinkAccept{{Engine: resourcev1.LinkEngine_LINK_ENGINE_POSTGRESQL}},
			DatabaseName: "blog", User: "blog",
			Sets: []*resourcev1.LinkSet{{Name: "DB_PASSWORD", Part: resourcev1.LinkPart_LINK_PART_PASSWORD}},
		}}},
	})
	refused := func(why string, edits ...*dockwellv1.Edit) {
		t.Helper()
		_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: why, Edits: edits}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), why) {
			t.Errorf("%s: got %v", why, err)
		}
	}
	refused("has no link db", fromData("BLOG_DB_PASSWORD", "db"))
	refused("which service db uses there", theLink, fromData("ADMIN_PASSWORD", "db"))

	apply(t, c, propose(t, c, "Link the blog", theLink, fromData("BLOG_DB_PASSWORD", "db"),
		in(blog, serviceSpec("web", map[string]string{"WORDPRESS_DB_PASSWORD": "DB_PASSWORD"}, hz1.id.ServerID))))
	envOf := func() map[string]string {
		t.Helper()
		got, err := hz1.docker.Inspect(t.Context(), service.Target(resource.Ref{Scope: blog, Name: "web"}))
		if err != nil || got == nil {
			t.Fatalf("web: %v, %v", got, err)
		}
		return got.GetEnv()
	}
	if got := envOf()["WORDPRESS_DB_PASSWORD"]; got != "blogs-own-1" {
		t.Fatalf("web reads %q", got)
	}

	rotate := propose(t, c, "Set the blog's password anew", in(data, secretSpec("BLOG_DB_PASSWORD", "blogs-own-2")))
	changed := map[string]bool{}
	for _, r := range rotate.GetPlan().GetResources() {
		if r.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_NONE {
			changed[r.GetResource().GetKind()+" "+r.GetResource().GetName()+" "+r.GetResource().GetScope()] = true
		}
	}
	for _, want := range []string{"secret BLOG_DB_PASSWORD " + data, "secret DB_PASSWORD " + blog, "service web " + blog} {
		if !changed[want] {
			t.Errorf("setting the blog's password anew doesn't take in %s: %v", want, changed)
		}
	}
	if changed["service db "+data] {
		t.Error("setting the blog's password anew restarts the shared database")
	}
	apply(t, c, rotate)
	if got := envOf()["WORDPRESS_DB_PASSWORD"]; got != "blogs-own-2" {
		t.Errorf("after the rotation web reads %q", got)
	}
}
