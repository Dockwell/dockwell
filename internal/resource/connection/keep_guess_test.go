// SPDX-License-Identifier: AGPL-3.0-only

package connection

import (
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// An edit that gives an access key's ID and leaves the secret out is refused in the
// same words whether the ID is the saved one or not, so no edit tests a guess at the
// ID without a passkey (the review's L2).
func TestKeepRefusesAKeyIDAloneRightOrWrong(t *testing.T) {
	saved := &resourcev1.ConnectionSpec{Kind: resourcev1.LinkKind_LINK_KIND_S3, S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "auto", Bucket: "uploads", AccessKeyId: "AKIDSAVED", SecretKey: "s"}}
	try := func(id string) error {
		edit := &resourcev1.ConnectionSpec{Kind: resourcev1.LinkKind_LINK_KIND_S3, S3: &resourcev1.ConnectionS3{Endpoint: "https://s3.example.com", Region: "auto", Bucket: "uploads", AccessKeyId: id}}
		_, err := Type{}.Keep(saved, edit)
		return err
	}
	const want = "give a new key's ID and its secret together, or leave both out to keep the saved key"
	for _, id := range []string{"AKIDSAVED", "AKIDGUESS"} {
		if err := try(id); err == nil || err.Error() != want {
			t.Errorf("an ID alone, %s: %v, want %q", id, err, want)
		}
	}
	if err := try(""); err != nil {
		t.Errorf("no keys at all: %v, want them kept", err)
	}
}
