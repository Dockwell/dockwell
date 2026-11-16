// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"

	"github.com/dockwell/dockwell/internal/store"
)

// Renaming a connection's label is the connection's change alone: a value that reads it
// reads it by one label on both sides, so nothing reads as moving to another bucket
// (the slice 3 review, H1).
func TestRenamingAConnectionMovesNothing(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	env := listedEnv(t, b)
	c := changeClient(b, b.origin)
	conn := func(label string) *dockwellv1.Edit {
		return connectionEdit("uploads", &resourcev1.ConnectionSpec{
			DisplayName: label, Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
			S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret},
		})
	}
	apply(t, c, propose(t, c, "Add uploads", conn("Uploads"),
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "link", Scope: env, Name: "storage"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
				Label: "Storage", Connection: "uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Prefix: "ghost/",
				Sets: []*resourcev1.LinkSet{{Name: "S3_BUCKET", Part: resourcev1.LinkPart_LINK_PART_BUCKET}},
			}}},
		},
		&dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "variable", Scope: env, Name: "S3_BUCKET"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Variable{Variable: &resourcev1.Variable{From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Connection{Connection: &resourcev1.ConnectionRef{
				Scope: orgID(t, f), Name: "uploads", Part: resourcev1.LinkPart_LINK_PART_BUCKET, Link: "storage",
			}}}}}},
		},
	))
	renamed := propose(t, c, "Rename uploads", conn("Media library"))
	for _, r := range renamed.GetPlan().GetResources() {
		t.Logf("%s %s: action %v, reads %v, before %v, storage %v, before %v, changes %v", r.GetResource().GetKind(), r.GetResource().GetName(), r.GetAction(),
			r.GetReads(), r.GetReadsBefore(), r.GetStorage(), r.GetStorageBefore(), r.GetChanges())
		if a, w := r.GetReads(), r.GetReadsBefore(); a != nil && w != nil && a.GetConnection() == w.GetConnection() && a.GetConnectionLabel() != w.GetConnectionLabel() {
			t.Errorf("%s reads %q in place of %q, though it reads the same connection %s: the panel says it moved", r.GetResource().GetName(), a.GetConnectionLabel(), w.GetConnectionLabel(), a.GetConnection())
		}
		if a, w := r.GetStorage(), r.GetStorageBefore(); a != nil && w != nil && a.GetConnection() == w.GetConnection() && a.GetConnectionLabel() != w.GetConnectionLabel() {
			t.Errorf("link %s keeps its files on %q in place of %q, though on the same connection: the panel says it moved", r.GetResource().GetName(), a.GetConnectionLabel(), w.GetConnectionLabel())
		}
	}
}

// A link whose set is renamed carries its sets whole on both sides, so a row can say
// what the renamed name reads; one whose sets don't change carries none (the slice
// 3 review, M1).
func TestAPlanCarriesALinksSetsWhole(t *testing.T) {
	t.Parallel()
	f := startFull(t, store.SQLite, nil)
	b := signedIn(t, f)
	env := listedEnv(t, b)
	c := changeClient(b, b.origin)
	link := func(name, label string) *dockwellv1.Edit {
		return &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: "link", Scope: env, Name: "storage"},
			Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_Link{Link: &resourcev1.Link{
				Label: label, Connection: "uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Prefix: "ghost/",
				Sets: []*resourcev1.LinkSet{{Name: name, Part: resourcev1.LinkPart_LINK_PART_BUCKET}},
			}}},
		}
	}
	apply(t, c, propose(t, c, "Add uploads", connectionEdit("uploads", &resourcev1.ConnectionSpec{
		Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Bucket: "uploads", AccessKeyId: "AKIDEXAMPLE", SecretKey: connectionSecret},
	}), link("S3_BUCKET", "Storage")))
	renamed := propose(t, c, "Rename the bucket's name", link("BUCKET_NAME", "Storage"))
	relabelled := propose(t, c, "Relabel the link", link("S3_BUCKET", "Files"))
	for _, r := range renamed.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "link" {
			if a, w := r.GetSets(), r.GetSetsBefore(); len(a) != 1 || a[0].GetName() != "BUCKET_NAME" || a[0].GetPart() != resourcev1.LinkPart_LINK_PART_BUCKET || len(w) != 1 || w[0].GetName() != "S3_BUCKET" {
				t.Errorf("the renamed set's link carries %v, before %v", a, w)
			}
		}
	}
	for _, r := range relabelled.GetPlan().GetResources() {
		if r.GetResource().GetKind() == "link" && (len(r.GetSets()) != 0 || len(r.GetSetsBefore()) != 0) {
			t.Errorf("a link whose sets don't change carries %v, before %v", r.GetSets(), r.GetSetsBefore())
		}
	}
}
