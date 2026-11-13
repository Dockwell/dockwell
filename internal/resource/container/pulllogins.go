// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sync"

	"github.com/dockwell/dockwell/internal/registry"
)

// PullLogins remembers which registry login pulled each image, by the image's ID, so
// a copy one app pulled with a login isn't run by another that may not use it, with
// no pull and no login (ADR 0086). It keeps names, never what signs in, in a file
// of the agent's, so it holds across a restart. Nil keeps it in memory alone.
type PullLogins struct {
	// Path is the file it's kept in. Empty keeps it in memory.
	Path string

	mu     sync.Mutex
	loaded bool
	byID   map[string]string
}

// maxPullLogins is how many images it remembers, the newest kept.
const maxPullLogins = 4096

// Who says which login pulled an image, as loginOf names one, and whether it
// knows: an image no login pulled reads as known, with none.
func (p *PullLogins) Who(id string) (string, bool) {
	if p == nil || id == "" {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.load()
	who, ok := p.byID[id]
	return who, ok
}

// Record notes that who pulled the image id, "" for no login.
func (p *PullLogins) Record(id, who string) error {
	if p == nil || id == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.load()
	if was, ok := p.byID[id]; ok && was == who {
		return nil
	}
	if len(p.byID) >= maxPullLogins {
		// The oldest isn't known, so one goes: it then reads as never pulled with
		// a login, as an image from before logins does.
		for k := range p.byID {
			delete(p.byID, k)
			break
		}
	}
	p.byID[id] = who
	return p.save()
}

func (p *PullLogins) load() {
	if p.loaded {
		return
	}
	p.loaded, p.byID = true, map[string]string{}
	if p.Path == "" {
		return
	}
	raw, err := os.ReadFile(p.Path)
	if err != nil {
		return
	}
	var got map[string]string
	if json.Unmarshal(raw, &got) == nil {
		maps.Copy(p.byID, got)
	}
}

func (p *PullLogins) save() error {
	if p.Path == "" {
		return nil
	}
	raw, err := json.Marshal(p.byID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.Path), 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	tmp := p.Path + ".new"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.Path)
}

// pullIdentity names the login a pull signs in with, as PullLogins keeps it: its
// registry, name and username, or "" for none.
func pullIdentity(a registry.Auth, ok bool) string {
	if !ok {
		return ""
	}
	return a.Registry + "/" + a.Name + "/" + a.Username
}
