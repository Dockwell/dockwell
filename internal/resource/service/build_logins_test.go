// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// A build names the registry logins its base images pull with by their names, each
// once, a handful at most (ADR 0086).
func TestABuildNamesItsLoginsOnce(t *testing.T) {
	build := func(names ...string) *resourcev1.ImageBuild {
		return &resourcev1.ImageBuild{Source: &resourcev1.ImageBuild_Git{Git: &resourcev1.GitSource{Url: "https://github.com/acme/web", Ref: "main"}}, RegistryLogins: names}
	}
	if err := CheckBuild(build("ghcr-ci", "hub")); err != nil {
		t.Errorf("two logins: %v", err)
	}
	for what, b := range map[string]*resourcev1.ImageBuild{
		"one named twice":       build("ghcr-ci", "ghcr-ci"),
		"a name that isn't one": build("GHCR CI"),
		"nine":                  build("a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9"),
	} {
		if err := CheckBuild(b); err == nil || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: %v", what, err)
		}
	}
}
