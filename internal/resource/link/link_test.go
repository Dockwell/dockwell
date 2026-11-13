// SPDX-License-Identifier: AGPL-3.0-only

package link_test

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/link"
)

// A version is compared at each comparator's own precision, so a range reads as a
// person writes it.
func TestTakes(t *testing.T) {
	for _, c := range []struct {
		versions, version string
		want              bool
	}{
		{"", "anything", true},
		{">=8.0", "8.4.11", true},
		{">=8.0", "5.7.44", false},
		{">=10.6", "10.6.21", true},
		{">=10.6", "10.5.99", false},
		{">=10.6 <12", "11.4.4-MariaDB", true},
		{">=10.6 <12", "12.3.3", false},
		{"<=8.4", "8.4.11", true},
		{"<=8.4", "9.0.1", false},
		{">8.0", "8.0.36", false},
		{">8.0", "8.1", true},
		{"=16", "16.4", true},
		{"16", "17.0", false},
		{">=8.0", "latest", false},
		{">=8.0", "8", true},
	} {
		if got := link.Takes(c.versions, c.version); got != c.want {
			t.Errorf("Takes(%q, %q) = %v, want %v", c.versions, c.version, got, c.want)
		}
	}
}

func TestCheckVersions(t *testing.T) {
	for _, ok := range []string{"", ">=8.0", ">=10.6 <12", "=16", "16"} {
		if err := link.CheckVersions(ok); err != nil {
			t.Errorf("CheckVersions(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{">=8.x", "~8", ">= 8", "8.0-beta"} {
		if err := link.CheckVersions(bad); err == nil {
			t.Errorf("CheckVersions(%q) took it", bad)
		}
	}
}

func validLink() *resourcev1.Link {
	return &resourcev1.Link{
		Label: "Database", Service: "db", Engine: resourcev1.LinkEngine_LINK_ENGINE_MARIADB, Version: "12.3.3",
		Accepts:      []*resourcev1.LinkAccept{{Engine: resourcev1.LinkEngine_LINK_ENGINE_MARIADB, Versions: ">=10.6"}},
		DatabaseName: "wordpress", User: "wordpress",
		Sets: []*resourcev1.LinkSet{
			{Name: "DB_ADDRESS", Part: resourcev1.LinkPart_LINK_PART_ADDRESS},
			{Name: "MARIADB_PASSWORD", Part: resourcev1.LinkPart_LINK_PART_PASSWORD},
		},
	}
}

func TestValidate(t *testing.T) {
	if err := (link.Type{}).Validate(validLink()); err != nil {
		t.Fatal(err)
	}
	for want, change := range map[string]func(*resourcev1.Link){
		"label":        func(l *resourcev1.Link) { l.Label = "two\nlines" },
		"service":      func(l *resourcev1.Link) { l.Service = "" },
		"engine of":    func(l *resourcev1.Link) { l.Engine = resourcev1.LinkEngine_LINK_ENGINE_UNSPECIFIED },
		"engines its":  func(l *resourcev1.Link) { l.Accepts = nil },
		"compare with": func(l *resourcev1.Link) { l.Accepts[0].Versions = "about 10" },
		"database":     func(l *resourcev1.Link) { l.DatabaseName = "Word-Press" },
		"sets":         func(l *resourcev1.Link) { l.Sets = nil },
		"twice":        func(l *resourcev1.Link) { l.Sets = append(l.Sets, l.Sets[0]) },
		"holds":        func(l *resourcev1.Link) { l.Sets[0].Part = resourcev1.LinkPart_LINK_PART_UNSPECIFIED },
		"template":     func(l *resourcev1.Link) { l.Sets[0].Template = "{host}" },
		"can read":     func(l *resourcev1.Link) { l.Sets[0].Name = "db-host" },
	} {
		l := validLink()
		change(l)
		err := (link.Type{}).Validate(l)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
}

// A link reads the service that meets it, so the engine keeps that service while it's
// linked to.
func TestReferences(t *testing.T) {
	refs := (link.Type{}).References(validLink())
	if len(refs) != 1 || refs[0].Kind != "service" || refs[0].Name != "db" || !refs[0].Apart {
		t.Fatalf("references %+v", refs)
	}
}

func TestSecret(t *testing.T) {
	for _, c := range []struct {
		set  *resourcev1.LinkSet
		want bool
	}{
		{&resourcev1.LinkSet{Part: resourcev1.LinkPart_LINK_PART_PASSWORD}, true},
		{&resourcev1.LinkSet{Part: resourcev1.LinkPart_LINK_PART_URL}, true},
		{&resourcev1.LinkSet{Part: resourcev1.LinkPart_LINK_PART_URL, Template: "redis://{host}:{port}"}, false},
		{&resourcev1.LinkSet{Part: resourcev1.LinkPart_LINK_PART_URL, Template: "postgres://{user}:{password}@{address}"}, true},
		{&resourcev1.LinkSet{Part: resourcev1.LinkPart_LINK_PART_HOST}, false},
	} {
		if got := link.Secret(c.set); got != c.want {
			t.Errorf("Secret(%v) = %v", c.set, got)
		}
	}
}
