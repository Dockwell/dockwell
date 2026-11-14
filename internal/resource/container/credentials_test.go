// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// Made-up hashes and tokens, in the forms htpasswd, crypt and their issuers write.
const (
	md5Hash    = "admin:$apr1$Ab3xQ9zK$Xl1R5uYbVj8mWqT0pNc4e/"
	bcryptHash = "$2y$05$abcdefghijklmnopqrstuuJ0fK3zP9wQx7vB1nM5cR8tY2uI4oE6a"
	shaHash    = "admin:{SHA}W6ph5Mm5Pz8GgiULbPgzG37mj9g="
)

// A plan shows a container's labels as they are, so a label that holds a credential
// counts as shown: by its key, as a proxy's basic auth is, or by a value that is one
// whatever it's called. What a proxy's ordinary labels say is not.
func TestALabelThatHoldsACredentialShows(t *testing.T) {
	for key, value := range map[string]string{
		"traefik.http.middlewares.auth.basicauth.users":                         md5Hash,
		"traefik.http.middlewares.auth.digestauth.users":                        "admin:realm:5f4dcc3b5aa765d61d8327deb882cf99",
		"caddy.basic_auth.admin":                                                bcryptHash,
		"caddy_1.route.basicauth.admin":                                         bcryptHash,
		"caddy.basicauth":                                                       "/admin/*",
		"traefik.http.middlewares.h.headers.customrequestheaders.Authorization": "Bearer abcdefghijklmnop0123",
		"backup.api_key":                                                        "on",
		"app.users":                                                             shaHash,
		"backup.users":                                                          bcryptHash,
		"site.users":                                                            `"$argon2id$v=19$m=65536,t=3,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNo"`,
		"caddy_0.basic_auth.admin":                                              `"` + bcryptHash + `"`,
		"deploy.header":                                                         "Basic YWRtaW46czNjcjd2YWx1ZQ==",
		"deploy.hook":                                                           "https://ghp_" + strings.Repeat("A1b2", 9) + "@git.example.com/app.git",
	} {
		got := CredentialLabels(&resourcev1.Container{Labels: map[string]string{key: value}})
		if !slices.Equal(got, []string{CredentialIn + key}) {
			t.Errorf("the label %s says %v", key, got)
		}
		for _, s := range got {
			if strings.Contains(s, value) {
				t.Errorf("what %s says shows its value: %q", key, s)
			}
		}
	}

	ordinary := map[string]string{
		"traefik.http.routers.app.rule":                                    "Host(`app.example.com`)",
		"traefik.http.services.app.loadbalancer.passhostheader":            "true",
		"traefik.http.middlewares.strip.replacepathregex.replacement":      "/$1$2",
		"traefik.http.middlewares.h.headers.customresponseheaders.X-Frame": "DENY",
		"com.docker.compose.config-hash":                                   strings.Repeat("ab12", 16),
		"caddy":                                                            "app.example.com",
		"caddy.reverse_proxy":                                              "{{upstreams 3000}}",
		"coolify.applicationId":                                            "12",
		"description":                                                      "Basic setup",
		"backup.password":                                                  "",
	}
	if got := CredentialLabels(&resourcev1.Container{Labels: ordinary}); len(got) > 0 {
		t.Errorf("ordinary labels say %v", got)
	}
}
