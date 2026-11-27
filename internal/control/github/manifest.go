// SPDX-License-Identifier: AGPL-3.0-only

package github

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxAppName is the longest name GitHub takes for an App.
const maxAppName = 34

// orgName is what GitHub takes as an organization's login.
var orgName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$`)

// Manifest is the App Dockwell asks GitHub to make, in GitHub's manifest format.
type Manifest struct {
	Name        string            `json:"name"`
	URL         string            `json:"url"`
	Hook        ManifestHook      `json:"hook_attributes"`
	RedirectURL string            `json:"redirect_url"`
	SetupURL    string            `json:"setup_url"`
	Public      bool              `json:"public"`
	Permissions map[string]string `json:"default_permissions"`
	// Events are what the App subscribes to. GitHub sends every App its
	// installation and installation_repositories events without one.
	Events []string `json:"default_events"`
}

// ManifestHook is the App's webhook.
type ManifestHook struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

// NewManifest describes the App for a panel at panel, for connection id: named
// "Dockwell" and the panel's host, which GitHub lets you change on its page,
// reading contents and metadata alone, with its webhook on the panel. Its setup
// page names the connection, as GitHub sends the browser back to it with an
// installation's ID alone.
func NewManifest(panel *url.URL, id string) Manifest {
	base := strings.TrimSuffix(panel.String(), "/")
	name := "Dockwell " + panel.Hostname()
	for utf8.RuneCountInString(name) > maxAppName {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return Manifest{
		Name:        strings.TrimRight(name, " .-"),
		URL:         base,
		Hook:        ManifestHook{URL: base + "/hooks/github/" + url.PathEscape(id), Active: true},
		RedirectURL: base + "/settings/integrations/github/created",
		SetupURL:    base + "/settings/integrations/github/" + url.PathEscape(id) + "/installed",
		Public:      false,
		Permissions: map[string]string{string(Contents): "read", string(Metadata): "read", string(Checks): "write"},
		Events:      []string{"push"},
	}
}

// JSON is the manifest as the form posts it.
func (m Manifest) JSON() string {
	b, _ := json.Marshal(m)
	return string(b)
}

// NewAppURL is where the browser posts the manifest: the person's own account,
// or the organization org's, which owns its App and its repositories.
func NewAppURL(web, org, state string) (string, error) {
	base := strings.TrimSuffix(web, "/")
	q := "?state=" + url.QueryEscape(state)
	if org == "" {
		return base + "/settings/apps/new" + q, nil
	}
	if !orgName.MatchString(org) {
		return "", errors.New("that isn't an organization's name on GitHub: letters, digits and single hyphens, 39 at most")
	}
	return base + "/organizations/" + url.PathEscape(org) + "/settings/apps/new" + q, nil
}

// InstallURL is where the browser goes to install the App slug, picking repositories.
func InstallURL(web, slug string) string {
	return strings.TrimSuffix(web, "/") + "/apps/" + url.PathEscape(slug) + "/installations/new"
}
