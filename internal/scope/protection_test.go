// SPDX-License-Identifier: AGPL-3.0-only

package scope_test

import (
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/scope"
)

// Protection as written comes first; an environment from before it was
// written is protected when it's named production (ADR 0049).
func TestProtectionIsReadAsWrittenThenByName(t *testing.T) {
	for _, tt := range []struct {
		name string
		p    resourcev1.Protection
		want bool
	}{
		{"production", resourcev1.Protection_PROTECTION_UNSPECIFIED, true},
		{"staging", resourcev1.Protection_PROTECTION_UNSPECIFIED, false},
		{"production", resourcev1.Protection_PROTECTION_OPEN, false},
		{"prod", resourcev1.Protection_PROTECTION_PROTECTED, true},
	} {
		if got := scope.Protected(tt.name, &resourcev1.Environment{Protection: tt.p}); got != tt.want {
			t.Errorf("%s with %v: protected %v, want %v", tt.name, tt.p, got, tt.want)
		}
	}
	if !scope.ProtectedSpec("staging", []byte{0xff}) {
		t.Error("a spec that can't be read reads open, want protected")
	}
}
