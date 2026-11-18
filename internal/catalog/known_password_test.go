// SPDX-License-Identifier: AGPL-3.0-only

package catalog

import (
	"strings"
	"testing"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// The catalog's own check refuses an entry that tells a person the password its app
// starts with, to change by hand, where known_password and the owner's steps change
// it, and a known password nothing changes; words that speak of other passwords pass
// (ADR 0105).
func TestCheckWordsRefusesAPasswordEveryoneKnows(t *testing.T) {
	t.Parallel()
	step := []*resourcev1.OwnerStep{{Step: &resourcev1.OwnerStep_Request{Request: &resourcev1.OwnerRequest{Path: "/api/auth/login", Port: 3000}}}}
	for name, c := range map[string]struct {
		e    *catalogv1.Entry
		want string
	}{
		"Umami's first steps": {
			e:    &catalogv1.Entry{FirstSteps: "It starts with the account admin and the password umami: change it under Settings, Profile, before it has a domain."},
			want: "a password everyone knows",
		},
		"a note of a default password": {
			e:    &catalogv1.Entry{Notes: []string{"Its default password is changeme."}},
			want: "a password everyone knows",
		},
		"a known password nothing changes": {
			e:    &catalogv1.Entry{Owner: &catalogv1.Owner{KnownUser: "admin", KnownPassword: "umami"}},
			want: "no steps change it",
		},
		"Umami with the steps that change it": {
			e: &catalogv1.Entry{Owner: &catalogv1.Owner{KnownUser: "admin", KnownPassword: "umami", PasswordInput: "OWNER_PASSWORD", Steps: step}},
		},
		"a note of another password": {
			e: &catalogv1.Entry{Notes: []string{"Leave OPENLIST_ADMIN_PASSWORD unset: OpenList prints it in its log."}},
		},
		"a feature with passwords": {
			e: &catalogv1.Entry{Features: []string{"Sharing with passwords and expiry"}},
		},
	} {
		err := CheckWords(c.e)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want it refused for %q", name, err, c.want)
		}
	}
}
