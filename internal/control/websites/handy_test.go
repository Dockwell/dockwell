// SPDX-License-Identifier: AGPL-3.0-only

package websites

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

func TestVisitorNotesDescribeEverydayChanges(t *testing.T) {
	t.Parallel()
	prev := &resourcev1.Website{
		Serves: &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/srv/site"}},
	}
	pausedNext := &resourcev1.Website{
		Serves:    &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/srv/site"}},
		Paused:    true,
		PauseMode: resourcev1.WebsitePauseMode_WEBSITE_PAUSE_MODE_MAINTENANCE,
	}
	var pauseMsgs []string
	for _, n := range visitorNotes("env_1", prev, pausedNext) {
		pauseMsgs = append(pauseMsgs, n.Message)
	}
	if !strings.Contains(strings.Join(pauseMsgs, "\n"), "maintenance page") {
		t.Fatalf("expected pause note, got %v", pauseMsgs)
	}

	rulesNext := &resourcev1.Website{
		Serves: &resourcev1.Website_Folder{Folder: &resourcev1.WebsiteFolder{Root: "/srv/site"}},
		Rules: &resourcev1.WebsiteRules{
			BasicAuth: &resourcev1.WebsiteBasicAuth{
				Users: []*resourcev1.WebsiteBasicAuthUser{{Username: "alice", Secret: "WEBSITE_AUTH_ALICE"}},
			},
			Cors:           &resourcev1.WebsiteCors{Enabled: true, AllowedOrigins: []string{"https://app.example.com"}},
			RateLimit:      &resourcev1.WebsiteRateLimit{Enabled: true, RequestsPerMinute: 120, Burst: 20},
			PathRules:      []*resourcev1.WebsitePathRule{{From: "/old", To: "/new", Kind: resourcev1.WebsitePathRuleKind_WEBSITE_PATH_RULE_KIND_REDIRECT, Permanent: true}},
			SubpathProxies: []*resourcev1.WebsiteSubpathProxy{{Path: "/api/*", Url: "http://192.0.2.10:3000"}},
			Headers:        []*resourcev1.WebsiteHeader{{Name: "X-Frame-Options", Value: "DENY"}},
			StaticCache:    &resourcev1.WebsiteStaticCache{Enabled: true, MaxAgeSeconds: 86400},
		},
		CustomCaddy: "respond /healthz 200",
	}
	var ruleMsgs []string
	for _, n := range visitorNotes("env_1", prev, rulesNext) {
		ruleMsgs = append(ruleMsgs, n.Message)
	}
	joined := strings.Join(ruleMsgs, "\n")
	for _, want := range []string{
		"Basic Auth",
		"CORS",
		"120 requests/min",
		"redirect and rewrite",
		"/api/*",
		"response header",
		"Static files are cached",
		"custom Caddy directives",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected visitor notes to mention %q, got:\n%s", want, joined)
		}
	}
}
