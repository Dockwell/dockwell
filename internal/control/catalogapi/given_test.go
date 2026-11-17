// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"slices"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// A pin keeps the names of the inputs a person gave a value, adding each that's
// given since; one from before keeps none, so its values all count as given (the
// SMTP re-check's N1).
func TestAPinKeepsWhatWasGiven(t *testing.T) {
	t.Parallel()
	installed := givenNames(nil, map[string]string{"MAIL_FROM": "notes@example.com", "SMTP_PORT": "2525"})
	if !slices.Equal(installed.GetNames(), []string{"MAIL_FROM", "SMTP_PORT"}) {
		t.Errorf("an install gives %v", installed.GetNames())
	}
	if none := givenNames(nil, nil); none == nil || len(none.GetNames()) != 0 {
		t.Errorf("an install with nothing typed gives %v, want an empty record", none)
	}
	upgraded := givenNames(&resourcev1.CatalogPin{Given: installed}, map[string]string{"SMTP_HOST": "smtp.example.com", "MAIL_FROM": "x@example.com"})
	if !slices.Equal(upgraded.GetNames(), []string{"MAIL_FROM", "SMTP_HOST", "SMTP_PORT"}) {
		t.Errorf("an upgrade gives %v", upgraded.GetNames())
	}
	if before := givenNames(&resourcev1.CatalogPin{Values: map[string]string{"MAIL_FROM": "a@example.com"}}, map[string]string{"SMTP_HOST": "h"}); before != nil {
		t.Errorf("a pin from before keeps %v, want no record", before)
	}
}
