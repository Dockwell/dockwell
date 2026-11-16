// SPDX-License-Identifier: AGPL-3.0-only

// Package connection is the "connection" kind: one set of values of one kind that
// something uses, an app through a link, such as a bucket of S3-compatible storage
// and the keys that reach it. It lives in the organization, on the control plane:
// its secrets go to a server only sealed with the values of an app linked to it, or
// for the check of it from where the app is. See
// docs/adr/0099-every-kind-of-thing-an-app-needs-met-the-same-three-ways.md.
package connection

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the connection kind.
const Kind resource.Kind = "connection"

// Type is the kind.
type Type struct{}

var (
	_ resource.Type        = Type{}
	_ resource.Rotates     = Type{}
	_ resource.Keeper      = Type{}
	_ resource.NewChecker  = Type{}
	_ resource.ReadThrough = Type{}
)

// Kind returns "connection".
func (Type) Kind() resource.Kind { return Kind }

// Level is the organization: a connection says which projects may use it.
func (Type) Level() resource.Level { return resource.LevelOrganization }

// IDPrefix is "cnx".
func (Type) IDPrefix() string { return "cnx" }

// New returns an empty Connection.
func (Type) New() proto.Message { return &resourcev1.ConnectionSpec{} }

// ReadThrough says a variable or a secret reads a connection's part through a link,
// so a new key takes in every app that reads it, in the same plan.
func (Type) ReadThrough() {}

// RotatesSensitive says setting a key always plans as a change, never compared with
// what's there, so a plan can't confirm a guess at it (ADR 0022).
func (Type) RotatesSensitive() {}

// CheckName takes what fits in a URL and on a command line.
func (Type) CheckName(name string) error {
	return resource.CheckLabel("connection", name, "outline-uploads")
}

// bucketName is a bucket's name as every S3 service takes it.
var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// region is an S3 region's name, such as auto or eu-central-1.
var region = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// maxDisplayName bounds what the panel calls one: a few words.
const maxDisplayName = 60

// Validate refuses what can't reach a bucket, and a reach that says nothing, in words
// that hold nothing of the keys.
func (Type) Validate(spec proto.Message) error {
	c := spec.(*resourcev1.ConnectionSpec) //nolint:forcetypeassert // the kind's own message
	name := strings.TrimSpace(c.GetDisplayName())
	switch {
	case strings.ContainsAny(name, "\r\n") || len([]rune(name)) > maxDisplayName:
		return fmt.Errorf("a connection's name is a few words on one line, at most %d characters", maxDisplayName)
	case c.GetKind() != resourcev1.LinkKind_LINK_KIND_S3:
		return errors.New("a connection reaches S3 storage for now. Set its kind to LINK_KIND_S3")
	case c.GetS3() == nil:
		return errors.New("a connection to S3 storage needs its endpoint, bucket and keys")
	}
	c.DisplayName = name
	if err := checkReach(c); err != nil {
		return err
	}
	return checkS3(c.GetS3())
}

// checkReach refuses projects named where none are read, and a reach to named
// projects that names none.
func checkReach(c *resourcev1.ConnectionSpec) error {
	for _, p := range c.GetProjects() {
		if !strings.HasPrefix(p, "prj_") {
			return fmt.Errorf("%q isn't a project's ID. A connection names the projects that may use it by ID, such as prj_…", p)
		}
	}
	switch c.GetReach() {
	case resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS:
		if len(c.GetProjects()) == 0 {
			return errors.New("a connection that reaches named projects names none. Name them, or reach none")
		}
	case resourcev1.ConnectionReach_CONNECTION_REACH_EVERY, resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED:
		if len(c.GetProjects()) > 0 {
			return errors.New("a connection names projects only where it reaches the ones it names. Set its reach to CONNECTION_REACH_PROJECTS, or leave projects out")
		}
	default:
		return fmt.Errorf("a connection's reach is none, projects or every, not %s", c.GetReach())
	}
	return nil
}

// checkS3 refuses an endpoint that isn't a URL to a host, a bucket's or a region's
// name no S3 service takes, and a public address that isn't one.
func checkS3(s *resourcev1.ConnectionS3) error {
	u, err := url.Parse(s.GetEndpoint())
	switch {
	case s.GetEndpoint() == "":
		return errors.New("a connection to S3 storage needs its endpoint, such as https://<account>.r2.cloudflarestorage.com")
	case err == nil && u.User != nil:
		// It may hold a key, so it isn't said back.
		return errors.New("an endpoint with a user or password in it isn't taken, as the keys go in their own fields. Give its URL alone, such as https://<account>.r2.cloudflarestorage.com")
	case err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http"):
		return errors.New("that isn't an endpoint. Give its URL, such as https://<account>.r2.cloudflarestorage.com, with no user, query or fragment")
	case !bucketName.MatchString(s.GetBucket()) || strings.Contains(s.GetBucket(), ".."):
		return fmt.Errorf("%q isn't a bucket's name: 3 to 63 lowercase letters, digits, dots and dashes, starting and ending with a letter or digit", s.GetBucket())
	case s.GetRegion() != "" && !region.MatchString(s.GetRegion()):
		return fmt.Errorf("%q isn't a region's name, such as auto or eu-central-1", s.GetRegion())
	}
	s.Endpoint = strings.TrimRight(u.String(), "/")
	if s.GetRegion() == "" {
		s.Region = "auto"
	}
	if p := s.GetPublicUrl(); p != "" {
		pu, err := url.Parse(p)
		if err != nil || pu.Host == "" || pu.User != nil || (pu.Scheme != "https" && pu.Scheme != "http") {
			return fmt.Errorf("%q isn't a public address. Give its URL, such as https://files.example.com", p)
		}
	}
	return nil
}

// CheckNew asks a new connection for its keys. An update can leave them out, which
// Keep fills in, so Validate can't.
func (Type) CheckNew(spec proto.Message) error {
	s := spec.(*resourcev1.ConnectionSpec).GetS3() //nolint:forcetypeassert // Validate saw it
	if s.GetAccessKeyId() == "" || s.GetSecretKey() == "" {
		return errors.New("a connection to S3 storage needs an access key's ID and its secret")
	}
	return nil
}

// Keep holds on to the keys when an update leaves them out, so a form can change a
// connection's name or reach without asking for its keys again. It keeps only while
// where they go stays: the same endpoint, region and bucket, and the bucket still in
// the path where it was, as a key is never sent somewhere it wasn't saved to go.
// Turning path style off sends it to the bucket's own host, a name under the
// endpoint's that on a shared domain may be someone else's. A new secret comes with
// its key's ID, as a new ID comes with its secret.
func (Type) Keep(current, desired proto.Message) (resource.Kept, error) {
	was, ok := current.(*resourcev1.ConnectionSpec)
	is, same := desired.(*resourcev1.ConnectionSpec)
	if !ok || !same {
		return resource.Kept{}, nil
	}
	from, to := was.GetS3(), is.GetS3()
	if from == nil || to == nil {
		return resource.Kept{}, nil
	}
	if to.GetSecretKey() != "" {
		// A key is its ID and its secret together: a new secret alone would be
		// saved beside no ID, and every app linked to it would start with an empty one.
		if to.GetAccessKeyId() == "" {
			return resource.Kept{}, errors.New("a new secret key needs its access key's ID too. Give both, even when the ID stays the same")
		}
		return resource.Kept{}, nil
	}
	switch {
	case to.GetEndpoint() != from.GetEndpoint() || to.GetRegion() != from.GetRegion() || to.GetBucket() != from.GetBucket():
		return resource.Kept{}, errors.New("the connection moves to another endpoint, region or bucket, so it needs its secret key again: Dockwell won't send what it kept to where it hasn't sent it before")
	case from.GetPathStyle() && !to.GetPathStyle():
		return resource.Kept{}, errors.New("the connection stops putting the bucket in the path, so its requests go to the bucket's own host, and it needs its secret key again: Dockwell won't send what it kept to where it hasn't sent it before. Give the key, or keep path style on")
	case to.GetAccessKeyId() != "" && to.GetAccessKeyId() != from.GetAccessKeyId():
		return resource.Kept{}, errors.New("a new access key needs its secret too")
	}
	to.AccessKeyId, to.SecretKey = from.GetAccessKeyId(), from.GetSecretKey()
	return resource.Kept{Reason: "kept_key", Words: "keeps the key it was saved with, as the update left that out"}, nil
}

// StampSaver records whether an install admin chose where the connection goes,
// whatever the edit says: below one, its checks reach no private address beyond
// the app's own networks (#710). An edit that keeps its endpoint, region and path
// style, as a rename or a wider reach does, keeps who chose them; one that moves
// it, or a new one, is the proposer's.
func (Type) StampSaver(current, spec proto.Message, installAdmin bool) {
	c, ok := spec.(*resourcev1.ConnectionSpec)
	if !ok {
		return
	}
	if was, ok := current.(*resourcev1.ConnectionSpec); ok && !movesAddress(was.GetS3(), c.GetS3()) {
		c.SavedBelowAdmin = was.GetSavedBelowAdmin()
		return
	}
	c.SavedBelowAdmin = !installAdmin
}

// movesAddress says whether an edit sends a connection somewhere else: another
// endpoint, region or path style.
func movesAddress(from, to *resourcev1.ConnectionS3) bool {
	return to.GetEndpoint() != from.GetEndpoint() || to.GetRegion() != from.GetRegion() || to.GetPathStyle() != from.GetPathStyle()
}

// Private says whether a check of the connection may reach a private address beyond
// the app's own networks: where an install admin saved it (ADR 0099).
func Private(c *resourcev1.ConnectionSpec) bool {
	return !c.GetSavedBelowAdmin()
}

// Reaches reports a connection a project may use.
func Reaches(c *resourcev1.ConnectionSpec, project string) bool {
	switch c.GetReach() {
	case resourcev1.ConnectionReach_CONNECTION_REACH_EVERY:
		return true
	case resourcev1.ConnectionReach_CONNECTION_REACH_PROJECTS:
		for _, p := range c.GetProjects() {
			if p == project {
				return true
			}
		}
	case resourcev1.ConnectionReach_CONNECTION_REACH_UNSPECIFIED:
	}
	return false
}

// Part is one of a connection's values, by the part a link sets, and whether it's
// one of its keys, which go only as a secret.
func Part(c *resourcev1.ConnectionSpec, part resourcev1.LinkPart) (value string, secret, ok bool) {
	s := c.GetS3()
	switch part {
	case resourcev1.LinkPart_LINK_PART_ENDPOINT:
		return s.GetEndpoint(), false, true
	case resourcev1.LinkPart_LINK_PART_REGION:
		return s.GetRegion(), false, true
	case resourcev1.LinkPart_LINK_PART_BUCKET:
		return s.GetBucket(), false, true
	case resourcev1.LinkPart_LINK_PART_ACCESS_KEY_ID:
		return s.GetAccessKeyId(), true, true
	case resourcev1.LinkPart_LINK_PART_SECRET_KEY:
		return s.GetSecretKey(), true, true
	case resourcev1.LinkPart_LINK_PART_PUBLIC_URL:
		return s.GetPublicUrl(), false, true
	default:
		return "", false, false
	}
}
