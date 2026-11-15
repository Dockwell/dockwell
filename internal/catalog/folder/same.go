// SPDX-License-Identifier: AGPL-3.0-only

package folder

import (
	"strings"

	"go.yaml.in/yaml/v4"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/registry"
)

// The same app in several catalogs (ADR 0078): WordPress from Dockwell's own, from
// 1Panel's app store and from Coolify's templates is one app, shown once, with the
// catalog to install it from chosen at install. Matching is conservative, as ADR
// 0029 asks: a wrong merge installs the wrong software, and a missed one costs a
// card.

// SameApp says two entries of one ID are the same app: the same name, in any case,
// and the same image for the service a person opens, by its repository. Anything
// less is another app, which a source leaves out, as one would take over from the
// other. The control plane reads a source with it, and dockwell-catalog rates counts
// what a server shows with it.
func SameApp(a, b *catalogv1.Entry) bool {
	if a == nil || b == nil || a.GetId() != b.GetId() || !strings.EqualFold(strings.TrimSpace(a.GetName()), strings.TrimSpace(b.GetName())) {
		return false
	}
	image := WebImage(a)
	return image != "" && image == WebImage(b)
}

// AppKey is what SameApp compares of an entry, so a read of a source that depends on
// it is read again when it changes.
func AppKey(e *catalogv1.Entry) string {
	if e == nil {
		return ""
	}
	return strings.ToLower(e.GetName()) + "\x00" + WebImage(e)
}

// WebImage is the repository of the image an entry's web service runs, as Docker
// names it, such as "wordpress" or "ghcr.io/acme/web", or of its one service when it
// names none. Empty when it can't tell.
func WebImage(e *catalogv1.Entry) string {
	var file struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if yaml.Unmarshal([]byte(e.GetCompose()), &file) != nil {
		return ""
	}
	service := e.GetWeb().GetService()
	if service == "" && len(file.Services) == 1 {
		for name := range file.Services {
			service = name
		}
	}
	s, ok := file.Services[service]
	if !ok {
		return ""
	}
	ref, err := registry.Parse(strings.TrimSpace(s.Image))
	if err != nil {
		return ""
	}
	return ref.Name()
}
