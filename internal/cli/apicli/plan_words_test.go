// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"regexp"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
)

// planOf renders one planned resource, as PlanView writes it.
func planOf(t *testing.T, r *dockwellv1.PlannedResource) string {
	t.Helper()
	var b strings.Builder
	PlanView(&b, &dockwellv1.Plan{Resources: []*dockwellv1.PlannedResource{r}})
	t.Logf("\n%s", b.String())
	return b.String()
}

func linkOf(sets ...*resourcev1.LinkSet) *resourcev1.Link {
	return &resourcev1.Link{Label: "Storage", Connection: "uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3, Prefix: "ghost/", Sets: sets}
}

var uploadsKeeps = &dockwellv1.LinkStorage{Connection: "uploads", ConnectionLabel: "Uploads", Prefix: "ghost/"}

// A set renamed, its part kept, still says what it reads (the slice 3 review, M1).
func TestPlanViewARenamedSetSaysWhatItReads(t *testing.T) {
	was := linkOf(&resourcev1.LinkSet{Name: "S3_BUCKET", Part: resourcev1.LinkPart_LINK_PART_BUCKET})
	now := linkOf(&resourcev1.LinkSet{Name: "BUCKET_NAME", Part: resourcev1.LinkPart_LINK_PART_BUCKET})
	changes := resourcepb.Changes(resource.Diff(was, now))
	t.Logf("diff: %v", changes)
	got := planOf(t, &dockwellv1.PlannedResource{
		Id: "lnk", Resource: &dockwellv1.ResourceRef{Kind: "link", Name: "storage"}, Action: dockwellv1.PlanAction_PLAN_ACTION_UPDATE,
		Storage: uploadsKeeps, StorageBefore: uploadsKeeps, Changes: changes, Sets: now.GetSets(), SetsBefore: was.GetSets(),
	})
	if !strings.Contains(got, "BUCKET_NAME") || !strings.Contains(got, "reads the bucket from Uploads, in place of S3_BUCKET") {
		t.Errorf("a renamed set doesn't say what it reads:\n%s", got)
	}
}

// A set whose part changes, its name kept, is named, and reads as a part change
// (the slice 3 review, M1 and L1).
func TestPlanViewASetReadingAnotherPartIsNamed(t *testing.T) {
	was := linkOf(&resourcev1.LinkSet{Name: "S3_BUCKET", Part: resourcev1.LinkPart_LINK_PART_BUCKET})
	now := linkOf(&resourcev1.LinkSet{Name: "S3_BUCKET", Part: resourcev1.LinkPart_LINK_PART_ENDPOINT})
	changes := resourcepb.Changes(resource.Diff(was, now))
	t.Logf("diff: %v", changes)
	got := planOf(t, &dockwellv1.PlannedResource{
		Id: "lnk", Resource: &dockwellv1.ResourceRef{Kind: "link", Name: "storage"}, Action: dockwellv1.PlanAction_PLAN_ACTION_UPDATE,
		Storage: uploadsKeeps, StorageBefore: uploadsKeeps, Changes: changes, Sets: now.GetSets(), SetsBefore: was.GetSets(),
	})
	if !strings.Contains(got, "~ S3_BUCKET") || !strings.Contains(got, "reads the endpoint from Uploads, in place of the bucket") {
		t.Errorf("a set whose part changes isn't named:\n%s", got)
	}
}

// A value that reads another part of the same connection says which part it read
// (the slice 3 review, L1).
func TestPlanViewAValueReadingAnotherPartSaysWhichItRead(t *testing.T) {
	was := &resourcev1.Variable{From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Connection{Connection: &resourcev1.ConnectionRef{Name: "uploads", Link: "storage", Part: resourcev1.LinkPart_LINK_PART_BUCKET}}}}
	now := &resourcev1.Variable{From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Connection{Connection: &resourcev1.ConnectionRef{Name: "uploads", Link: "storage", Part: resourcev1.LinkPart_LINK_PART_ENDPOINT}}}}
	changes := resourcepb.Changes(resource.Diff(was, now))
	got := planOf(t, &dockwellv1.PlannedResource{
		Id: "var", Resource: &dockwellv1.ResourceRef{Kind: "variable", Name: "S3_X"}, Action: dockwellv1.PlanAction_PLAN_ACTION_UPDATE,
		Reads:       &dockwellv1.ConnectionRead{Connection: "uploads", ConnectionLabel: "Uploads", Part: resourcev1.LinkPart_LINK_PART_ENDPOINT, Link: "storage"},
		ReadsBefore: &dockwellv1.ConnectionRead{Connection: "uploads", ConnectionLabel: "Uploads", Part: resourcev1.LinkPart_LINK_PART_BUCKET, Link: "storage"},
		Changes:     changes,
	})
	if strings.Contains(got, "from Uploads, in place of Uploads") {
		t.Errorf("a value reading another part says it replaces the connection with itself:\n%s", got)
	}
}

// A value that moves between two connections with one label shows (the slice 3
// review, M2).
func TestPlanViewAValueMovingBetweenLikeLabelsShows(t *testing.T) {
	was := &resourcev1.Variable{From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Connection{Connection: &resourcev1.ConnectionRef{Name: "uploads", Link: "storage", Part: resourcev1.LinkPart_LINK_PART_BUCKET}}}}
	now := &resourcev1.Variable{From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Connection{Connection: &resourcev1.ConnectionRef{Name: "uploads-eu", Link: "storage", Part: resourcev1.LinkPart_LINK_PART_BUCKET}}}}
	changes := resourcepb.Changes(resource.Diff(was, now))
	got := planOf(t, &dockwellv1.PlannedResource{
		Id: "var", Resource: &dockwellv1.ResourceRef{Kind: "variable", Name: "S3_BUCKET"}, Action: dockwellv1.PlanAction_PLAN_ACTION_UPDATE,
		Reads:       &dockwellv1.ConnectionRead{Connection: "uploads-eu", ConnectionLabel: "Uploads", Part: resourcev1.LinkPart_LINK_PART_BUCKET, Link: "storage"},
		ReadsBefore: &dockwellv1.ConnectionRead{Connection: "uploads", ConnectionLabel: "Uploads", Part: resourcev1.LinkPart_LINK_PART_BUCKET, Link: "storage"},
		Changes:     changes,
	})
	if !strings.Contains(got, "the bucket from Uploads (uploads-eu), in place of Uploads (uploads)") {
		t.Errorf("a value moving between two connections labelled alike shows no change:\n%s", got)
	}
}

// A value that stops reading a connection leaves no raw from.connection row.
func TestPlanViewAValueThatStopsReadingLeavesNoRawRow(t *testing.T) {
	was := &resourcev1.Variable{From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Connection{Connection: &resourcev1.ConnectionRef{Name: "uploads", Link: "storage", Part: resourcev1.LinkPart_LINK_PART_BUCKET}}}}
	now := &resourcev1.Variable{Value: "my-bucket"}
	changes := resourcepb.Changes(resource.Diff(was, now))
	got := planOf(t, &dockwellv1.PlannedResource{
		Id: "var", Resource: &dockwellv1.ResourceRef{Kind: "variable", Name: "S3_BUCKET"}, Action: dockwellv1.PlanAction_PLAN_ACTION_UPDATE,
		ReadsBefore: &dockwellv1.ConnectionRead{Connection: "uploads", ConnectionLabel: "Uploads", Part: resourcev1.LinkPart_LINK_PART_BUCKET, Link: "storage"},
		Changes:     changes,
	})
	if strings.Contains(got, "from.connection") {
		t.Errorf("a raw from.connection row stays:\n%s", got)
	}
}

// A database link's URL set says its template and its TLS words are its own, with
// no raw sets[n] row (the slice 3 review, M1).
func TestPlanViewAURLSetsTemplateIsItsOwn(t *testing.T) {
	now := &resourcev1.Link{Label: "Database", Service: "db", Sets: []*resourcev1.LinkSet{
		{Name: "DATABASE_URL", Part: resourcev1.LinkPart_LINK_PART_URL, Template: "postgres://{user}:{password}@{host}:{port}/{database}", TlsOn: "require", TlsOff: "disable"},
	}}
	changes := resourcepb.Changes(resource.Diff(nil, now))
	got := planOf(t, &dockwellv1.PlannedResource{
		Id: "lnk", Resource: &dockwellv1.ResourceRef{Kind: "link", Name: "db"}, Action: dockwellv1.PlanAction_PLAN_ACTION_CREATE, Changes: changes, Sets: now.GetSets(),
	})
	if strings.Contains(got, "sets[") || !strings.Contains(got, "DATABASE_URL's template") || !strings.Contains(got, "DATABASE_URL with TLS") || !strings.Contains(got, "+ DATABASE_URL") {
		t.Errorf("a raw sets row stays:\n%s", got)
	}
}

// A set added between two others is one set added, not a rename and an add (the
// slice 3 re-check, N1).
func TestPlanViewASetAddedInTheMiddleIsAdded(t *testing.T) {
	b := &resourcev1.LinkSet{Name: "S3_BUCKET", Part: resourcev1.LinkPart_LINK_PART_BUCKET}
	e := &resourcev1.LinkSet{Name: "S3_ENDPOINT", Part: resourcev1.LinkPart_LINK_PART_ENDPOINT}
	r := &resourcev1.LinkSet{Name: "S3_REGION", Part: resourcev1.LinkPart_LINK_PART_REGION}
	was, now := linkOf(b, r), linkOf(b, e, r)
	got := planOf(t, &dockwellv1.PlannedResource{
		Id: "lnk", Resource: &dockwellv1.ResourceRef{Kind: "link", Name: "storage"}, Action: dockwellv1.PlanAction_PLAN_ACTION_UPDATE,
		Storage: uploadsKeeps, StorageBefore: uploadsKeeps, Changes: resourcepb.Changes(resource.Diff(was, now)), Sets: now.GetSets(), SetsBefore: was.GetSets(),
	})
	if strings.Contains(got, "in place of") || strings.Contains(got, "S3_REGION") || strings.Contains(got, "S3_BUCKET") ||
		!regexp.MustCompile(`\+ S3_ENDPOINT +reads the endpoint from Uploads\n`).MatchString(got) {
		t.Errorf("adding S3_ENDPOINT reads as more than that:\n%s", got)
	}
}

// A set removed from the front is one set removed (the slice 3 re-check, N1).
func TestPlanViewASetRemovedFromTheFrontIsRemoved(t *testing.T) {
	b := &resourcev1.LinkSet{Name: "S3_BUCKET", Part: resourcev1.LinkPart_LINK_PART_BUCKET}
	r := &resourcev1.LinkSet{Name: "S3_REGION", Part: resourcev1.LinkPart_LINK_PART_REGION}
	was, now := linkOf(b, r), linkOf(r)
	got := planOf(t, &dockwellv1.PlannedResource{
		Id: "lnk", Resource: &dockwellv1.ResourceRef{Kind: "link", Name: "storage"}, Action: dockwellv1.PlanAction_PLAN_ACTION_UPDATE,
		Storage: uploadsKeeps, StorageBefore: uploadsKeeps, Changes: resourcepb.Changes(resource.Diff(was, now)), Sets: now.GetSets(), SetsBefore: was.GetSets(),
	})
	if strings.Contains(got, "in place of") || strings.Contains(got, "S3_REGION") ||
		!regexp.MustCompile(`- S3_BUCKET +reads the bucket from Uploads\n`).MatchString(got) {
		t.Errorf("removing S3_BUCKET reads as more than that:\n%s", got)
	}
}

// A set renamed while it reads another part, beside a set that stays, is one gone and
// one come, as neither alone says what the other was (the slice 3 re-check, N1).
func TestPlanViewARenameThatReadsAnotherPartIsAnAddAndARemove(t *testing.T) {
	b := &resourcev1.LinkSet{Name: "S3_BUCKET", Part: resourcev1.LinkPart_LINK_PART_BUCKET}
	r := &resourcev1.LinkSet{Name: "S3_REGION", Part: resourcev1.LinkPart_LINK_PART_REGION}
	e := &resourcev1.LinkSet{Name: "S3_ENDPOINT", Part: resourcev1.LinkPart_LINK_PART_ENDPOINT}
	was, now := linkOf(b, r), linkOf(b, e)
	got := planOf(t, &dockwellv1.PlannedResource{
		Id: "lnk", Resource: &dockwellv1.ResourceRef{Kind: "link", Name: "storage"}, Action: dockwellv1.PlanAction_PLAN_ACTION_UPDATE,
		Storage: uploadsKeeps, StorageBefore: uploadsKeeps, Changes: resourcepb.Changes(resource.Diff(was, now)), Sets: now.GetSets(), SetsBefore: was.GetSets(),
	})
	if strings.Contains(got, "in place of") || strings.Contains(got, "S3_BUCKET") ||
		!strings.Contains(got, "+ S3_ENDPOINT") || !strings.Contains(got, "- S3_REGION") {
		t.Errorf("a rename to another part reads as:\n%s", got)
	}
}

// A plan from before the plan kept a link's sets whole writes their fields as it
// did, as nothing in it says which set a lone name or part belongs to (the slice 3
// re-check, N2, on the review's R1 and R2).
func TestPlanViewAnOlderPlansSetsReadAsTheyDid(t *testing.T) {
	for _, tc := range []struct{ was, now *resourcev1.LinkSet }{
		{&resourcev1.LinkSet{Name: "S3_BUCKET", Part: resourcev1.LinkPart_LINK_PART_BUCKET}, &resourcev1.LinkSet{Name: "BUCKET_NAME", Part: resourcev1.LinkPart_LINK_PART_BUCKET}},
		{&resourcev1.LinkSet{Name: "S3_BUCKET", Part: resourcev1.LinkPart_LINK_PART_BUCKET}, &resourcev1.LinkSet{Name: "S3_BUCKET", Part: resourcev1.LinkPart_LINK_PART_ENDPOINT}},
	} {
		changes := resourcepb.Changes(resource.Diff(linkOf(tc.was), linkOf(tc.now)))
		got := planOf(t, &dockwellv1.PlannedResource{
			Id: "lnk", Resource: &dockwellv1.ResourceRef{Kind: "link", Name: "storage"}, Action: dockwellv1.PlanAction_PLAN_ACTION_UPDATE,
			Storage: uploadsKeeps, StorageBefore: uploadsKeeps, Changes: changes,
		})
		for _, c := range changes {
			if !regexp.MustCompile(`~ ` + regexp.QuoteMeta(c.GetPath()) + ` +` + regexp.QuoteMeta(c.GetFrom()) + ` → ` + regexp.QuoteMeta(c.GetTo())).MatchString(got) {
				t.Errorf("an older plan's %s reads:\n%s", c.GetPath(), got)
			}
		}
		if strings.Contains(got, "reads") {
			t.Errorf("an older plan's sets read in words it can't back:\n%s", got)
		}
	}
}
