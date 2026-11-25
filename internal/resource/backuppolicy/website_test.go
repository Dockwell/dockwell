// SPDX-License-Identifier: AGPL-3.0-only

package backuppolicy

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

func TestWebsiteFolderPolicyValidation(t *testing.T) {
	t.Parallel()
	var typ Type
	spec := &resourcev1.BackupPolicy{WebsiteFolder: true}
	if err := typ.Validate(spec); err != nil {
		t.Fatalf("website_folder policy failed validation: %v", err)
	}
	if spec.GetEvery() != resourcev1.BackupPolicy_EVERY_DAY || spec.GetAt() != "03:00" || spec.GetKeep() != DefaultKeep {
		t.Fatalf("defaults not filled: %+v", spec)
	}
	refs := typ.References(spec)
	if len(refs) != 1 || refs[0].Kind != "service" || refs[0].Name != "web" {
		t.Fatalf("unexpected references for website_folder policy: %+v", refs)
	}

	if err := typ.Validate(&resourcev1.BackupPolicy{Volume: "data", WebsiteFolder: true}); err == nil || !strings.Contains(err.Error(), "one target") {
		t.Fatalf("expected conflict between volume and website_folder, got %v", err)
	}
	if err := typ.Validate(&resourcev1.BackupPolicy{Link: "db", WebsiteFolder: true}); err == nil || !strings.Contains(err.Error(), "one target") {
		t.Fatalf("expected conflict between link and website_folder, got %v", err)
	}
}
