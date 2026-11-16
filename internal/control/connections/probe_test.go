// SPDX-License-Identifier: AGPL-3.0-only

package connections

import (
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// A check reaches a private address beyond the app's own networks only where an
// install admin saved the connection (#710).
func TestACheckReachesPrivateAddressesOnlyForAnAdminsConnection(t *testing.T) {
	c := &resourcev1.ConnectionSpec{S3: &resourcev1.ConnectionS3{Endpoint: "http://10.0.0.5:9000", Bucket: "uploads", AccessKeyId: "AKID", SecretKey: "key"}}
	if got := probeOf(c, "notes/", false); !got.Private || got.S3.Prefix != "notes/" || got.S3.SecretKey != "key" {
		t.Errorf("an admin's connection is checked with %+v", got)
	}
	c.SavedBelowAdmin = true
	if got := probeOf(c, "notes/", false); got.Private {
		t.Error("a connection saved below an admin is checked reaching private addresses")
	}
}
