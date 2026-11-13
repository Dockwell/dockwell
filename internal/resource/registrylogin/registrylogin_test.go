// SPDX-License-Identifier: AGPL-3.0-only

package registrylogin

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

const ecrRegistry = "123456789012.dkr.ecr.eu-west-1.amazonaws.com"

// A registry is written as an image names it, whatever was typed.
func TestALoginsRegistryIsWrittenAsAnImageNamesIt(t *testing.T) {
	for typed, want := range map[string]string{
		"ghcr.io":                         "ghcr.io",
		"https://GHCR.io/":                "ghcr.io",
		"index.docker.io":                 "docker.io",
		"https://registry-1.docker.io/v2": "docker.io",
		"registry.example.com:5000":       "registry.example.com:5000",
		"not a host":                      "",
		"":                                "",
	} {
		if got := Host(typed); got != want {
			t.Errorf("Host(%q) = %q, want %q", typed, got, want)
		}
	}
}

func TestALoginIsValidated(t *testing.T) {
	ok := func() *resourcev1.RegistryLogin {
		return &resourcev1.RegistryLogin{Registry: "https://ghcr.io", Username: "ci", Password: "token", TokenHost: "GHCR.io"}
	}
	l := ok()
	if err := (Type{}).Validate(l); err != nil {
		t.Fatalf("a good login: %v", err)
	}
	if l.GetRegistry() != "ghcr.io" || l.GetTokenHost() != "ghcr.io" {
		t.Errorf("Validate wrote %q and %q", l.GetRegistry(), l.GetTokenHost())
	}
	for what, change := range map[string]func(*resourcev1.RegistryLogin){
		"no registry":                 func(l *resourcev1.RegistryLogin) { l.Registry = "" },
		"no username":                 func(l *resourcev1.RegistryLogin) { l.Username = "" },
		"a project by name":           func(l *resourcev1.RegistryLogin) { l.Projects = []string{"shop"} },
		"an ECR host with a password": func(l *resourcev1.RegistryLogin) { l.Registry = ecrRegistry },
		"an AWS key for another host": func(l *resourcev1.RegistryLogin) {
			l.Username, l.Password, l.Ecr = "", "", &resourcev1.RegistryLoginEcr{AccessKey: "AKIA", SecretKey: "s"}
		},
		"a token host with a path":        func(l *resourcev1.RegistryLogin) { l.TokenHost = "not a host" },
		"Docker Hub as its token service": func(l *resourcev1.RegistryLogin) { l.TokenHost = "docker.io" },
	} {
		l := ok()
		change(l)
		if err := (Type{}).Validate(l); err == nil {
			t.Errorf("%s was taken", what)
		}
	}
	e := &resourcev1.RegistryLogin{Registry: ecrRegistry, Ecr: &resourcev1.RegistryLoginEcr{AccessKey: "AKIA", SecretKey: "s"}}
	if err := (Type{}).Validate(e); err != nil || e.GetEcr().GetRegion() != "eu-west-1" {
		t.Errorf("an ECR login: %v, region %q", err, e.GetEcr().GetRegion())
	}
	e.Ecr.Region = "us-east-1"
	if err := (Type{}).Validate(e); err == nil {
		t.Error("an ECR login in another region than its registry's was taken")
	}
	if err := (Type{}).CheckNew(&resourcev1.RegistryLogin{Registry: "ghcr.io", Username: "ci"}); err == nil {
		t.Error("a new login with no password was taken")
	}
}

// An update that leaves the password out keeps it, while the login goes where it
// went; one that moves it to another registry, token service or user asks for it
// again, so a kept password is never sent somewhere new.
func TestAnUpdateKeepsThePasswordWhileItGoesWhereItWent(t *testing.T) {
	was := &resourcev1.RegistryLogin{Registry: "ghcr.io", Username: "ci", Password: "token", TokenHost: "ghcr.io", Projects: []string{"prj_1"}}
	is := &resourcev1.RegistryLogin{Registry: "ghcr.io", Username: "ci", TokenHost: "ghcr.io", Projects: []string{"prj_1", "prj_2"}}
	kept, err := (Type{}).Keep(was, is)
	if err != nil || is.GetPassword() != "token" || kept.Reason == "" {
		t.Fatalf("Keep = %+v, %v, password %q", kept, err, is.GetPassword())
	}
	if strings.Contains(kept.Words, "token") {
		t.Error("what Keep says holds the password")
	}
	for what, moved := range map[string]*resourcev1.RegistryLogin{
		"registry":      {Registry: "registry.example.com", Username: "ci", TokenHost: "ghcr.io"},
		"token service": {Registry: "ghcr.io", Username: "ci", TokenHost: "auth.example.com"},
		"user":          {Registry: "ghcr.io", Username: "someone", TokenHost: "ghcr.io"},
	} {
		if _, err := (Type{}).Keep(was, moved); err == nil || moved.GetPassword() != "" {
			t.Errorf("a login moved to another %s kept its password", what)
		}
	}
	// A new password replaces the old one.
	rotated := &resourcev1.RegistryLogin{Registry: "ghcr.io", Username: "ci", Password: "new", TokenHost: "ghcr.io"}
	if _, err := (Type{}).Keep(was, rotated); err != nil || rotated.GetPassword() != "new" {
		t.Errorf("a new password: %v, %q", err, rotated.GetPassword())
	}
	ecrWas := &resourcev1.RegistryLogin{Registry: ecrRegistry, Ecr: &resourcev1.RegistryLoginEcr{AccessKey: "AKIA", SecretKey: "s", Region: "eu-west-1"}}
	ecrIs := &resourcev1.RegistryLogin{Registry: ecrRegistry, Ecr: &resourcev1.RegistryLoginEcr{Region: "eu-west-1"}}
	if _, err := (Type{}).Keep(ecrWas, ecrIs); err != nil || ecrIs.GetEcr().GetSecretKey() != "s" || ecrIs.GetEcr().GetAccessKey() != "AKIA" {
		t.Errorf("an ECR update that left the key out: %v, %+v", err, ecrIs.GetEcr())
	}
}

// An ECR region goes into the address of ECR's API, so it's a region's name alone.
// (Security review of #584, L2.)
func TestAnECRRegionIsARegionsName(t *testing.T) {
	for _, bad := range []string{"eu-west-1.attacker.example", "eu-west-1/x", "EU-WEST-1", "eu", "eu-west", "1-west-1"} {
		if IsRegion(bad) {
			t.Errorf("%q reads as a region", bad)
		}
		e := &resourcev1.RegistryLogin{Registry: ecrRegistry, Ecr: &resourcev1.RegistryLoginEcr{AccessKey: "AKIA", SecretKey: "s", Region: bad}}
		if err := (Type{}).Validate(e); err == nil {
			t.Errorf("an ECR login in %q was taken", bad)
		}
	}
	for _, good := range []string{"eu-west-1", "us-gov-west-1", "ap-southeast-2", "cn-north-1"} {
		if !IsRegion(good) {
			t.Errorf("%q doesn't read as a region", good)
		}
	}
	if _, ok := ECRRegion("123456789012.dkr.ecr.eu-west-1.attacker.amazonaws.com"); ok {
		t.Error("a host with more than a region in its place reads as ECR's")
	}
}
