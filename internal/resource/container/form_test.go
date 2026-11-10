// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/formtest"
)

// Each check here was one regular expression, with its lengths counted in it. It
// takes what that expression took, to the byte.

// label is the form of a project's, an environment's and a service's name, as the
// expressions had it.
const label = `[a-z]([a-z0-9-]{0,61}[a-z0-9])?`

// labels are names of that kind and near misses, around the length one gets, and
// some with what follows a service's name in its container's.
func labels() []string {
	out := formtest.Corpus("a-z09", "A._ é\n", 63)
	for _, l := range formtest.Corpus("a-z09", "", 61, 63) {
		out = append(out, l+"-1", l+"-042", l+"-", l+"-1a")
	}
	return out
}

func TestIsScopeNameTakesWhatItsExpressionTook(t *testing.T) {
	cases := slices.Concat(labels(), formtest.Joined(labels(), ".", 2, 6000), formtest.Joined(labels(), ".", 3, 1000))
	formtest.Agree(t, `^`+label+`\.`+label+`$`, IsScopeName, cases)
}

func TestIsReadableFormTakesWhatItsExpressionTook(t *testing.T) {
	cases := slices.Concat(labels(), formtest.Joined(labels(), ".", 2, 1000), formtest.Joined(labels(), ".", 3, 12000), formtest.Joined(labels(), ".", 4, 1000))
	formtest.Agree(t, `^`+label+`\.`+label+`\.`+label+`-[0-9]+$`, isReadableForm, cases)
}

func TestIsCheckHostTakesWhatItsExpressionTook(t *testing.T) {
	hosts := formtest.Corpus("a.-Z09", "_: é\n", 253)
	cases := slices.Clone(hosts)
	for _, port := range []string{":", ":8", ":80800", ":808000", ":80:80", ":http"} {
		for _, h := range hosts {
			cases = append(cases, h+port)
		}
	}
	formtest.Agree(t, `^[a-zA-Z0-9]([a-zA-Z0-9.-]{0,251}[a-zA-Z0-9])?(:[0-9]{1,5})?$`, isCheckHost, cases)
}

func TestIsHostnameTakesWhatItsExpressionTook(t *testing.T) {
	parts := formtest.Corpus("a-Z09", "_: é\n", 63)
	cases := slices.Concat(parts, formtest.Joined(parts, ".", 2, 4000), formtest.Joined(parts, ".", 3, 4000))
	formtest.Agree(t, `^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`, isHostname, cases)
}

func TestATagTakesWhatItsExpressionTook(t *testing.T) {
	formtest.Agree(t, `^\w[\w.-]{0,127}$`,
		func(tag string) bool { return len(tag) <= maxTagLen && tagRE.MatchString(tag) },
		formtest.Corpus("a_.-Z09", "/: é\n", 128))
	// And through CheckImage, which is what a spec meets.
	if err := CheckImage("nginx:" + strings.Repeat("1", 128)); err != nil {
		t.Errorf("a tag of 128 characters: %v", err)
	}
	if err := CheckImage("nginx:" + strings.Repeat("1", 129)); err == nil {
		t.Error("a tag of 129 characters was taken")
	}
}

func TestAGroupTakesWhatItsExpressionTook(t *testing.T) {
	cases := slices.Concat(formtest.Corpus("a_.-Z09", "/: é\n", 10, 64), formtest.Corpus("0123456789", "a_", 10, 64))
	formtest.Agree(t, `^(\d{1,10}|[a-zA-Z_][a-zA-Z0-9_.-]{0,63})$`,
		func(group string) bool {
			return validatePrivileges(&resourcev1.Container{GroupAdd: []string{group}}) == nil
		}, cases)
}
