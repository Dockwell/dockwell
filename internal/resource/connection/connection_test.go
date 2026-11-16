// SPDX-License-Identifier: AGPL-3.0-only

package connection

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

func aBucket() *resourcev1.ConnectionSpec {
	return &resourcev1.ConnectionSpec{
		DisplayName: "Uploads", Kind: resourcev1.LinkKind_LINK_KIND_S3,
		Reach: resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS, Projects: []string{"prj_notes"},
		S3: &resourcev1.ConnectionS3{
			Endpoint: "https://account.r2.cloudflarestorage.com/", Bucket: "uploads",
			AccessKeyId: "AKIDEXAMPLE", SecretKey: "the-secret",
		},
	}
}

// A connection to a bucket is taken as it's written, its endpoint without a
// trailing slash and its region auto unless it says; a new one asks for its keys.
func TestABucketConnectionIsTaken(t *testing.T) {
	c := aBucket()
	if err := (Type{}).Validate(c); err != nil {
		t.Fatal(err)
	}
	if err := (Type{}).CheckNew(c); err != nil {
		t.Fatal(err)
	}
	if c.GetS3().GetEndpoint() != "https://account.r2.cloudflarestorage.com" || c.GetS3().GetRegion() != "auto" {
		t.Errorf("taken as %v", c.GetS3())
	}
	c.S3.SecretKey = ""
	if err := (Type{}).CheckNew(c); err == nil {
		t.Error("a new connection with no secret key is taken")
	}
}

// What can't reach a bucket, or reach no one as it says, is refused, in words that
// hold nothing of its keys.
func TestAConnectionThatCantReachABucketIsRefused(t *testing.T) {
	for want, change := range map[string]func(*resourcev1.ConnectionSpec){
		"S3 storage for now":                  func(c *resourcev1.ConnectionSpec) { c.Kind = resourcev1.LinkKind_LINK_KIND_SMTP },
		"isn't an endpoint":                   func(c *resourcev1.ConnectionSpec) { c.S3.Endpoint = "ftp://files.example.com?key=the-secret" },
		"with a user or password in it":       func(c *resourcev1.ConnectionSpec) { c.S3.Endpoint = "https://key:the-secret@files.example.com" },
		"isn't a bucket's name":               func(c *resourcev1.ConnectionSpec) { c.S3.Bucket = "Up_Loads" },
		"isn't a region's name":               func(c *resourcev1.ConnectionSpec) { c.S3.Region = "Eu Central" },
		"isn't a public address":              func(c *resourcev1.ConnectionSpec) { c.S3.PublicUrl = "files.example.com" },
		"names none":                          func(c *resourcev1.ConnectionSpec) { c.Projects = nil },
		"isn't a project's ID":                func(c *resourcev1.ConnectionSpec) { c.Projects = []string{"notes"} },
		"only where it reaches the ones":      func(c *resourcev1.ConnectionSpec) { c.Reach = resourcev1.ConnectionReach_CONNECTION_REACH_EVERY },
		"a few words on one line":             func(c *resourcev1.ConnectionSpec) { c.DisplayName = "two\nlines" },
		"needs its endpoint, bucket and keys": func(c *resourcev1.ConnectionSpec) { c.S3 = nil },
	} {
		c := aBucket()
		change(c)
		err := (Type{}).Validate(c)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
		if err != nil && strings.Contains(err.Error(), "the-secret") {
			t.Errorf("%s: the refusal says the secret key", want)
		}
	}
}

// An update that leaves the keys out keeps them while they'd go where they went;
// one that moves the connection elsewhere needs them again, as does one that stops
// putting the bucket in the path, which sends them to the bucket's own host.
func TestAnUpdateKeepsTheKeysOnlyWhereTheyWent(t *testing.T) {
	was := aBucket()
	was.S3.PathStyle = true
	if err := (Type{}).Validate(was); err != nil {
		t.Fatal(err)
	}
	renamed := proto.CloneOf(was)
	renamed.DisplayName, renamed.S3.AccessKeyId, renamed.S3.SecretKey = "Notes' uploads", "", ""
	kept, err := (Type{}).Keep(was, renamed)
	if err != nil || kept.Reason != "kept_key" || renamed.GetS3().GetSecretKey() != "the-secret" || renamed.GetS3().GetAccessKeyId() != "AKIDEXAMPLE" {
		t.Errorf("renamed, kept %v, %v: %v", kept, err, renamed.GetS3())
	}
	for what, change := range map[string]func(*resourcev1.ConnectionS3){
		"endpoint":   func(s *resourcev1.ConnectionS3) { s.Endpoint = "https://elsewhere.example.com" },
		"bucket":     func(s *resourcev1.ConnectionS3) { s.Bucket = "other" },
		"region":     func(s *resourcev1.ConnectionS3) { s.Region = "eu-central-1" },
		"access key": func(s *resourcev1.ConnectionS3) { s.AccessKeyId = "AKIDOTHER" },
		"host":       func(s *resourcev1.ConnectionS3) { s.PathStyle = false },
	} {
		moved := proto.CloneOf(was)
		moved.S3.SecretKey = ""
		change(moved.S3)
		if _, err := (Type{}).Keep(was, moved); err == nil {
			t.Errorf("a new %s kept the old secret key", what)
		}
	}
}

// An update with a new secret and no access key ID is refused: it never saves an
// empty ID, which every app linked to the connection would start with.
func TestANewSecretAloneIsRefused(t *testing.T) {
	was := &resourcev1.ConnectionSpec{
		Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "auto", Bucket: "uploads", AccessKeyId: "AKIDOLD", SecretKey: "old", PathStyle: true},
	}
	to := &resourcev1.ConnectionSpec{
		Kind: resourcev1.LinkKind_LINK_KIND_S3, Reach: resourcev1.ConnectionReach_CONNECTION_REACH_EVERY,
		S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "auto", Bucket: "uploads", SecretKey: "new", PathStyle: true},
	}
	if err := (Type{}).Validate(to); err != nil {
		t.Fatal(err)
	}
	if _, err := (Type{}).Keep(was, to); err == nil || !strings.Contains(err.Error(), "access key's ID") {
		t.Errorf("an update with a new secret alone was kept, %v: %v", err, to.GetS3())
	}
	if to.GetS3().GetAccessKeyId() != "" {
		t.Errorf("an update with a new secret alone took an access key ID: %v", to.GetS3())
	}
}

// Reach is said, never implied: unset reaches no project.
func TestReachIsSaid(t *testing.T) {
	c := aBucket()
	if !Reaches(c, "prj_notes") || Reaches(c, "prj_other") {
		t.Error("a connection to named projects reaches others, or not its own")
	}
	c.Reach, c.Projects = resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED, nil
	if Reaches(c, "prj_notes") {
		t.Error("an unset reach reaches a project")
	}
	c.Reach = resourcev1.ConnectionReach_CONNECTION_REACH_EVERY
	if !Reaches(c, "prj_new") {
		t.Error("every project doesn't reach a new one")
	}
}

// A connection records whether an install admin chose where it goes, whatever its
// edit says: a new one, or one moved to another endpoint, region or path style, is
// the proposer's; one whose address stays keeps who chose it (#710).
func TestWhoChoseItsAddressDecidesItsPrivateReach(t *testing.T) {
	for _, admin := range []bool{true, false} {
		c := aBucket()
		c.SavedBelowAdmin = admin
		(Type{}).StampSaver(nil, c, admin)
		if c.GetSavedBelowAdmin() == admin || Private(c) != admin {
			t.Errorf("new, by an install admin %v: below an admin %v, private %v", admin, c.GetSavedBelowAdmin(), Private(c))
		}
	}
	below := aBucket()
	below.SavedBelowAdmin = true
	renamed := aBucket()
	renamed.DisplayName = "Renamed"
	(Type{}).StampSaver(below, renamed, true)
	if Private(renamed) {
		t.Error("an admin's rename gave a developer's address private reach")
	}
	for what, move := range map[string]func(*resourcev1.ConnectionS3){
		"endpoint":   func(s *resourcev1.ConnectionS3) { s.Endpoint = "https://elsewhere.example.com" },
		"region":     func(s *resourcev1.ConnectionS3) { s.Region = "eu-central-1" },
		"path style": func(s *resourcev1.ConnectionS3) { s.PathStyle = !s.GetPathStyle() },
	} {
		moved := aBucket()
		move(moved.S3)
		(Type{}).StampSaver(below, moved, true)
		if !Private(moved) {
			t.Errorf("an admin's new %s stays a developer's", what)
		}
	}
}
