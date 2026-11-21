// SPDX-License-Identifier: AGPL-3.0-only

package snapshot

import (
	"errors"
	"sync"

	"go.yaml.in/yaml/v4"
)

var cloudflaredImage = sync.OnceValues(func() (string, error) {
	data, err := files.ReadFile("entries/cloudflared.yaml")
	if err != nil {
		return "", err
	}
	var entry struct{ Compose string }
	if err := yaml.Unmarshal(data, &entry); err != nil {
		return "", err
	}
	var spec struct {
		Services map[string]struct{ Image string }
	}
	if err := yaml.Unmarshal([]byte(entry.Compose), &spec); err != nil {
		return "", err
	}
	if image := spec.Services["tunnel"].Image; image != "" {
		return image, nil
	}
	return "", errors.New("the catalog cloudflared entry has no tunnel image")
})

// CloudflaredImage is the tunnel image from the built-in catalog entry.
func CloudflaredImage() (string, error) { return cloudflaredImage() }
