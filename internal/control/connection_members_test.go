// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/backup/s3/s3test"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/store"
)

// A developer, then a viewer, of one project tries every way at a connection that
// reaches another project. It isn't listed, checked, linked or changed, and every
// refusal reads as for a connection that isn't there, so trying names tells them
// nothing. Nor can a developer reveal a connection's key through their own app's
// secret (ADR 0099).
func TestAConnectionOfAnotherProjectReadsAsNotThere(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	e := f.addDockerServer(t)
	pullsAnything(e)
	env := listedEnv(t, b)
	project := envProject(t, f, env)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	// The app's environment open, as staging is, where a developer applies.
	apply(t, c, propose(t, c, "Open production", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: "environment", Scope: project, Name: "production"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Environment{Environment: &resourcev1.Environment{DisplayName: "Production", Protection: resourcev1.Protection_PROTECTION_OPEN}}},
	}))

	notes := &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "notes"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: &resourcev1.Service{
			Runs:    &resourcev1.Service_App{App: &resourcev1.Container{Image: "acme/notes:1"}},
			Secrets: map[string]string{"S3_SECRET_ACCESS_KEY": "S3_SECRET_ACCESS_KEY"},
		}}},
		Servers: []string{f.lastDocker.id.ServerID},
	}
	apply(t, c, propose(t, c, "Run the notes app", secretSpec("S3_SECRET_ACCESS_KEY", "old-secret"), notes))
	for _, name := range e.Containers() {
		if strings.Contains(name, "notes") {
			got, err := e.Client().InspectContainer(t.Context(), name)
			if err != nil {
				t.Fatal(err)
			}
			for n := range got.NetworkSettings.Networks {
				e.AddNetwork(docker.Network{Name: n, IPAM: docker.IPAM{Config: []docker.IPAMConfig{{Subnet: "127.0.0.0/8"}}}})
			}
		}
	}
	srv := s3test.New(t)
	apply(t, c, propose(t, c, "Add ours", connectionEdit("ours", &resourcev1.ConnectionSpec{
		DisplayName: "Ours", Kind: resourcev1.LinkKind_LINK_KIND_S3,
		Reach: resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS, Projects: []string{project},
		S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "ours", AccessKeyId: "AKIDOURS", SecretKey: connectionSecret, PathStyle: true},
	})))
	apply(t, c, propose(t, c, "Add the data project", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "data"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: "Data"}}},
	}))
	var dataProject string
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		org, err := tx.Organization(t.Context())
		if err != nil {
			return err
		}
		p, err := tx.GetResource(t.Context(), "project", org.ID, "data")
		dataProject = p.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}

	apply(t, c, propose(t, c, "Add theirs", connectionEdit("theirs-private-name", &resourcev1.ConnectionSpec{
		DisplayName: "Theirs", Kind: resourcev1.LinkKind_LINK_KIND_S3,
		Reach: resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS, Projects: []string{dataProject},
		S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "theirs", AccessKeyId: "AKIDTHEIRS", SecretKey: "theirs-secret", PathStyle: true},
	})))

	hold := aMember(t, f)
	for _, role := range []string{store.RoleDeveloper, store.RoleViewer} {
		hold(store.Grant{ProjectID: project, Role: role})
		who := role + " of one project"

		// The list shows ours, by name alone.
		listed, err := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{}))
		if err != nil {
			t.Fatalf("%s lists: %v", who, err)
		}
		raw, _ := protojson.Marshal(listed.Msg)
		if strings.Contains(string(raw), "theirs") || strings.Contains(string(raw), srv.URL()) {
			t.Errorf("%s lists %s", who, raw)
		}
		// Narrowed to the other project, nothing.
		if got, err := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{Project: "data"})); err == nil && len(got.Msg.GetConnections()) > 0 {
			t.Errorf("%s narrowed to data lists %v", who, got.Msg.GetConnections())
		}
		// Check is an admin's.
		if _, err := cc.CheckConnection(t.Context(), connect.NewRequest(&dockwellv1.CheckConnectionRequest{Name: "theirs-private-name"})); err == nil {
			t.Errorf("%s checks theirs", who)
		}
		// ConnectStorage to theirs, and to one that isn't there, read the same.
		_, errTheirs := cc.ConnectStorage(t.Context(), connect.NewRequest(&dockwellv1.ConnectStorageRequest{
			Project: project, App: "notes", Connection: "theirs-private-name",
			Sets: map[string]resourcev1.LinkPart{"S3_SECRET_ACCESS_KEY": resourcev1.LinkPart_LINK_PART_SECRET_KEY},
		}))
		_, errNone := cc.ConnectStorage(t.Context(), connect.NewRequest(&dockwellv1.ConnectStorageRequest{
			Project: project, App: "notes", Connection: "no-such-name",
			Sets: map[string]resourcev1.LinkPart{"S3_SECRET_ACCESS_KEY": resourcev1.LinkPart_LINK_PART_SECRET_KEY},
		}))
		if role == store.RoleDeveloper && (connect.CodeOf(errTheirs) != connect.CodeNotFound || connect.CodeOf(errNone) != connect.CodeNotFound) {
			t.Errorf("%s ConnectStorage: theirs %v, none %v", who, errTheirs, errNone)
		}
		// A link by hand to theirs, and to one that isn't there, read the same.
		linkTo := func(name string) error {
			_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Probe a name", Edits: []*dockwellv1.Edit{{
				Resource: &dockwellv1.ResourceRef{Kind: "link", Scope: env, Name: "guess"},
				Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
					Label: "Storage", Connection: name, Kind: resourcev1.LinkKind_LINK_KIND_S3,
					Sets: []*resourcev1.LinkSet{{Name: "S3_BUCKET", Part: resourcev1.LinkPart_LINK_PART_BUCKET}},
				}}},
			}}}))
			return err
		}
		eTheirs, eNone := linkTo("theirs-private-name"), linkTo("no-such-name")
		if eTheirs == nil || eNone == nil || strings.ReplaceAll(eTheirs.Error(), "theirs-private-name", "X") != strings.ReplaceAll(eNone.Error(), "no-such-name", "X") {
			t.Errorf("%s tells a connection of another project from none by a link:\n  theirs: %v\n  none:   %v", who, eTheirs, eNone)
		}
		// Rotating ours is an admin's of the install.
		_, err = c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Rotate", Edits: []*dockwellv1.Edit{
			connectionEdit("ours", &resourcev1.ConnectionSpec{
				DisplayName: "Ours", Kind: resourcev1.LinkKind_LINK_KIND_S3,
				Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
				S3:    &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "ours", AccessKeyId: "AKIDOURS", SecretKey: "mine-now", PathStyle: true},
			}),
		}}))
		if err == nil {
			t.Errorf("%s changes ours", who)
		}
	}

	// A developer who connects their app to ours can't reveal its key through their
	// app's secret, or through a secret written from it: revealing a connection's
	// value is an admin's, as it's the install's, whichever app reads it. A secret of
	// their own they still reveal, and an admin of the install reveals both.
	hold(store.Grant{ProjectID: project, Role: store.RoleDeveloper})
	planned, err := cc.ConnectStorage(t.Context(), connect.NewRequest(&dockwellv1.ConnectStorageRequest{
		Project: project, App: "notes", Connection: "ours",
		Sets: map[string]resourcev1.LinkPart{"S3_SECRET_ACCESS_KEY": resourcev1.LinkPart_LINK_PART_SECRET_KEY},
	}))
	if err != nil {
		t.Fatalf("a developer connects to ours: %v", err)
	}
	if done := apply(t, c, planned.Msg.GetChange()); done.GetState() != dockwellv1.ChangeState_CHANGE_STATE_APPLIED {
		t.Fatalf("applied %v: %s", done.GetState(), done.GetError())
	}
	apply(t, c, propose(t, c, "Write the storage URL", secretSpec("PLAIN", "a-secret-of-theirs"), fromSecret("STORAGE_URL", &resourcev1.ValueTemplate{
		Text:  "s3://${S3_SECRET_ACCESS_KEY}@storage",
		Names: []*resourcev1.TemplateName{{Name: "S3_SECRET_ACCESS_KEY", Secret: true, Encoding: resourcev1.Encoding_ENCODING_USERINFO}},
	})))
	v := variablesClient(b, b.origin)
	list, err := v.ListVariables(t.Context(), connect.NewRequest(&dockwellv1.ListVariablesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, x := range list.Msg.GetVariables() {
		ids[x.GetName()] = x.GetId()
	}
	reveal := func(name string) (string, error) {
		t.Helper()
		shown, err := v.RevealSecret(t.Context(), connect.NewRequest(&dockwellv1.RevealSecretRequest{Id: ids[name], Proof: stepUp(t, b, ids[name])}))
		if err != nil {
			return "", err
		}
		return shown.Msg.GetValue(), nil
	}
	for _, name := range []string{"S3_SECRET_ACCESS_KEY", "STORAGE_URL"} {
		got, err := reveal(name)
		if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "from the connection ours, so only an admin reveals it") || got != "" {
			t.Errorf("a developer reveals %s, which reads ours: %q, %v", name, got, err)
		}
	}
	if got, err := reveal("PLAIN"); err != nil || got != "a-secret-of-theirs" {
		t.Errorf("a developer reveals a secret of their own: %q, %v", got, err)
	}
	hold(store.Grant{ProjectID: store.Install, Role: store.RoleAdmin})
	if got, err := reveal("S3_SECRET_ACCESS_KEY"); err != nil || got != connectionSecret {
		t.Errorf("an admin of the install reveals the connection's key: %q, %v", got, err)
	}
	if got, err := reveal("STORAGE_URL"); err != nil || !strings.Contains(got, "s3://") {
		t.Errorf("an admin of the install reveals a secret written from it: %q, %v", got, err)
	}
}

// A developer of one project names a connection of another, then one that isn't
// there, through a value that reads it, alone or beside a link, in either order.
// Each answers in the same words, as pinning a value looks the connection up before
// anything else does (ADR 0099).
func TestAValueFromAConnectionOfAnotherProjectReadsAsNotThere(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	env := listedEnv(t, b)
	project := envProject(t, f, env)
	c := changeClient(b, b.origin)
	cc := dockwellv1connect.NewConnectionServiceClient(b, b.origin, connect.WithProtoJSON())
	srv := s3test.New(t)
	apply(t, c, propose(t, c, "Add the data project", &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Name: "data"},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: "Data"}}},
	}))
	var dataProject, org string
	if err := f.store.View(t.Context(), func(tx *store.Tx) error {
		o, err := tx.Organization(t.Context())
		if err != nil {
			return err
		}
		org = o.ID
		p, err := tx.GetResource(t.Context(), "project", o.ID, "data")
		dataProject = p.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	apply(t, c, propose(t, c, "Add theirs", connectionEdit("theirs-private-name", &resourcev1.ConnectionSpec{
		DisplayName: "Theirs", Kind: resourcev1.LinkKind_LINK_KIND_S3,
		Reach: resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS, Projects: []string{dataProject},
		S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "theirs", AccessKeyId: "AKIDTHEIRS", SecretKey: "theirs-secret", PathStyle: true},
	})))

	hold := aMember(t, f)
	hold(store.Grant{ProjectID: project, Role: store.RoleDeveloper})

	try := func(what string, edits func(name string) []*dockwellv1.Edit) {
		t.Helper()
		var got [2]string
		for i, name := range []string{"theirs-private-name", "no-such-name"} {
			_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Probe", Edits: edits(name)}))
			if err == nil {
				got[i] = "<nil>"
				continue
			}
			got[i] = connect.CodeOf(err).String() + ": " + strings.ReplaceAll(err.Error(), name, "X")
		}
		if got[0] != got[1] || !strings.Contains(got[0], "which isn't one this project can use") {
			t.Errorf("%s tells a connection of another project from none:\n  theirs: %s\n  none:   %s", what, got[0], got[1])
		}
	}
	linkEdit := func(name string) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "link", Scope: env, Name: "guess"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
				Label: "Storage", Connection: name, Kind: resourcev1.LinkKind_LINK_KIND_S3,
				Sets: []*resourcev1.LinkSet{{Name: "GUESS_KEY", Part: resourcev1.LinkPart_LINK_PART_SECRET_KEY}},
			}}},
		}
	}
	secretEdit := func(name string) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "secret", Scope: env, Name: "GUESS_KEY"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{From: &resourcev1.ValueFrom{
				Source: &resourcev1.ValueFrom_Connection{Connection: &resourcev1.ConnectionRef{Scope: org, Name: name, Part: resourcev1.LinkPart_LINK_PART_SECRET_KEY, Link: "guess"}},
			}}}},
		}
	}
	varEdit := func(name string) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "variable", Scope: env, Name: "GUESS_BUCKET"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{From: &resourcev1.ValueFrom{
				Source: &resourcev1.ValueFrom_Connection{Connection: &resourcev1.ConnectionRef{Scope: org, Name: name, Part: resourcev1.LinkPart_LINK_PART_BUCKET, Link: "guess"}},
			}}}},
		}
	}
	try("link alone", func(n string) []*dockwellv1.Edit { return []*dockwellv1.Edit{linkEdit(n)} })
	try("link then secret", func(n string) []*dockwellv1.Edit { return []*dockwellv1.Edit{linkEdit(n), secretEdit(n)} })
	try("secret then link", func(n string) []*dockwellv1.Edit { return []*dockwellv1.Edit{secretEdit(n), linkEdit(n)} })
	try("variable then link", func(n string) []*dockwellv1.Edit { return []*dockwellv1.Edit{varEdit(n), linkEdit(n)} })
	try("secret alone", func(n string) []*dockwellv1.Edit { return []*dockwellv1.Edit{secretEdit(n)} })
	// Saving or deleting one is an admin's, whether it's there or not.
	tryAny := func(what string, edits func(name string) []*dockwellv1.Edit) {
		t.Helper()
		var got [2]string
		for i, name := range []string{"theirs-private-name", "no-such-name"} {
			_, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Probe", Edits: edits(name)}))
			got[i] = connect.CodeOf(err).String() + ": " + strings.ReplaceAll(fmt.Sprint(err), name, "X")
		}
		if got[0] != got[1] || !strings.Contains(got[0], "only an admin or the owner can plan this") {
			t.Errorf("%s tells a connection of another project from none:\n  theirs: %s\n  none:   %s", what, got[0], got[1])
		}
	}
	tryAny("connection set", func(n string) []*dockwellv1.Edit {
		return []*dockwellv1.Edit{connectionEdit(n, &resourcev1.ConnectionSpec{
			DisplayName: "Mine", Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			S3: &resourcev1.ConnectionS3{Endpoint: srv.URL(), Bucket: "mine", AccessKeyId: "AKIDMINE", SecretKey: "mine", PathStyle: true},
		})}
	})
	tryAny("connection delete", func(n string) []*dockwellv1.Edit {
		return []*dockwellv1.Edit{{Resource: &dockwellv1.ResourceRef{Kind: "connection", Name: n}, Delete: true}}
	})

	// The list narrowed to another project reads as narrowed to none.
	_, e1 := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{Project: "data"}))
	_, e2 := cc.ListConnections(t.Context(), connect.NewRequest(&dockwellv1.ListConnectionsRequest{Project: "nope"}))
	if connect.CodeOf(e1) != connect.CodeOf(e2) || e1 == nil {
		t.Errorf("the list narrowed to another project, then to none: %v, %v", e1, e2)
	}
}
