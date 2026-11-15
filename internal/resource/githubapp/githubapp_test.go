// SPDX-License-Identifier: AGPL-3.0-only

package githubapp_test

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/githubapp"
)

const pemKey = "-----BEGIN RSA PRIVATE KEY-----\nMIIE...\n-----END RSA PRIVATE KEY-----\n"

func connected() *resourcev1.GitHubApp {
	return &resourcev1.GitHubApp{
		AppId: 42, Slug: "dockwell-deploy", Owner: "acme", OwnerIsOrganization: true, ClientId: "Iv1.abc",
		PrivateKey: pemKey, WebhookSecret: "whsec", KeyFingerprint: "SHA256:abc", HookId: "ghw_0123456789abcdef",
	}
}

// What GitHub gave is taken, and an installation's projects are kept sorted once.
func TestAnAppAsGitHubGaveIt(t *testing.T) {
	t.Parallel()
	a := connected()
	a.Reaches = []*resourcev1.GitHubInstallationReach{
		{InstallationId: 9, Projects: []string{"prj_b", "prj_a", "prj_b"}},
		{InstallationId: 3},
	}
	if err := (githubapp.Type{}).Validate(a); err != nil {
		t.Fatal(err)
	}
	if err := (githubapp.Type{}).CheckNew(a); err != nil {
		t.Fatal(err)
	}
	if a.GetReaches()[0].GetInstallationId() != 3 || strings.Join(a.GetReaches()[1].GetProjects(), ",") != "prj_a,prj_b" {
		t.Errorf("the reaches are %v", a.GetReaches())
	}
}

// What can't be an App Dockwell made is refused, saying nothing of its key.
func TestWhatIsntAnAppIsRefused(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*resourcev1.GitHubApp){
		"a slug with a slash":        func(a *resourcev1.GitHubApp) { a.Slug = "acme/deploy" },
		"a key that isn't one":       func(a *resourcev1.GitHubApp) { a.PrivateKey = "hunter2" },
		"a secret with a line break": func(a *resourcev1.GitHubApp) { a.WebhookSecret = "a\nb" },
		"a webhook ID with a slash":  func(a *resourcev1.GitHubApp) { a.HookId = "ghw_../x" },
		"an installation twice": func(a *resourcev1.GitHubApp) {
			a.Reaches = []*resourcev1.GitHubInstallationReach{{InstallationId: 3}, {InstallationId: 3}}
		},
		"a project by name": func(a *resourcev1.GitHubApp) {
			a.Reaches = []*resourcev1.GitHubInstallationReach{{InstallationId: 3, Projects: []string{"shop"}}}
		},
	} {
		a := connected()
		change(a)
		err := (githubapp.Type{}).Validate(a)
		if err == nil {
			t.Errorf("%s was taken", name)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "whsec") {
			t.Errorf("%s: the refusal says %v", name, err)
		}
	}
	if err := (githubapp.Type{}).CheckNew(&resourcev1.GitHubApp{AppId: 42, Slug: "x"}); err == nil {
		t.Error("an App with no key was made by hand")
	}
}

// An edit that says only which projects an installation reaches keeps the rest;
// one that names another App, or another owner, is refused.
func TestAnEditKeepsTheKeyAndSecrets(t *testing.T) {
	t.Parallel()
	was := connected()
	was.PreviousWebhookSecret, was.PreviousUntil = "old", 1700000000
	edit := &resourcev1.GitHubApp{Reaches: []*resourcev1.GitHubInstallationReach{{InstallationId: 3, Projects: []string{"prj_a"}}}}
	kept, err := (githubapp.Type{}).Keep(was, edit)
	if err != nil || kept.Reason != "kept_key" {
		t.Fatalf("the edit kept %v, %v", kept, err)
	}
	if edit.GetPrivateKey() != pemKey || edit.GetWebhookSecret() != "whsec" || edit.GetPreviousWebhookSecret() != "old" ||
		edit.GetAppId() != 42 || edit.GetSlug() != "dockwell-deploy" || edit.GetHookId() != "ghw_0123456789abcdef" || !edit.GetOwnerIsOrganization() || edit.GetKeyFingerprint() != "SHA256:abc" {
		t.Errorf("the edit became %v", edit)
	}
	newKey := &resourcev1.GitHubApp{PrivateKey: pemKey + "2", KeyFingerprint: "SHA256:new"}
	if _, err := (githubapp.Type{}).Keep(was, newKey); err != nil || newKey.GetKeyFingerprint() != "SHA256:new" || newKey.GetWebhookSecret() != "whsec" {
		t.Errorf("a new key became %v, %v", newKey, err)
	}
	if _, err := (githubapp.Type{}).Keep(was, &resourcev1.GitHubApp{AppId: 43}); err == nil {
		t.Error("an edit naming another App was kept")
	}
	if _, err := (githubapp.Type{}).Keep(was, &resourcev1.GitHubApp{Owner: "someone-else"}); err == nil {
		t.Error("an edit naming another owner was kept")
	}
}

// An installation reaches every project until the App says which, and then those.
func TestAnInstallationReachesEveryProjectUntilNarrowed(t *testing.T) {
	t.Parallel()
	a := connected()
	if !githubapp.Reaches(a, 3, "prj_a") {
		t.Error("an installation the App says nothing of reaches nothing")
	}
	a.Reaches = []*resourcev1.GitHubInstallationReach{{InstallationId: 3, Projects: []string{"prj_a"}}, {InstallationId: 4}}
	if !githubapp.Reaches(a, 3, "prj_a") || githubapp.Reaches(a, 3, "prj_b") || githubapp.Reaches(a, 4, "prj_a") {
		t.Error("a narrowed installation reaches what it wasn't given")
	}
}
