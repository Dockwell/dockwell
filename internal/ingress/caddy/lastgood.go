// SPDX-License-Identifier: AGPL-3.0-only

package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/dockwell/dockwell/internal/ingress"
)

// Caddy saves its config with a truncating write, so a full disk or a power cut
// can leave it empty or cut short. An empty one starts Caddy with no routes; one
// cut short makes it exit, and Docker start it again, for good. Nothing else on
// the server knows what it served, so the agent keeps a copy of its own, where
// Caddy can't reach it, and puts that back when the last config is damaged.

// goodFile is the agent's copy of Caddy's last good config. It is in the
// ingress's own directory, not Caddy's, so it is out of Caddy's reach. It may
// hold the panel's private key while that serves (PanelSite.Cert), as the config
// does.
const goodFile = "last-good.json"

// autosaveRel is Caddy's last config, in its config directory. Caddy rewrites it
// on every change, and autosaveSettle is longer than that takes, so that a read
// caught in the middle of a rewrite isn't taken for damage.
const (
	autosaveRel    = "caddy/autosave.json"
	autosaveSettle = time.Second
)

// serverConfig holds what a config needs for the agent to call it good.
type serverConfig struct {
	Admin struct {
		Listen string `json:"listen"`
	} `json:"admin"`
	Apps map[string]json.RawMessage `json:"apps"`
}

// usable reports a config Caddy could run that the agent could go on managing:
// a whole JSON object with apps, listening on its socket. So a copy can't be
// what puts the admin API back on TCP, whatever was loaded by hand.
func (c *Caddy) usable(b []byte) bool {
	var config serverConfig
	if json.Unmarshal(b, &config) != nil {
		return false
	}
	return config.Admin.Listen == adminListen(c.Runtime.Paths()) && len(config.Apps) > 0
}

// damaged reports a last config Caddy can't have written whole, which a full
// disk or a power cut leaves: nothing, an end cut off, or a block of zeros.
// Caddy writes no config for "null", so that isn't one either.
func damaged(b []byte) bool {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	var top map[string]json.RawMessage
	return json.Unmarshal(b, &top) != nil
}

// lastConfig reads Caddy's last config. Anything that isn't a regular file reads
// as damaged, with no error: Caddy writes no such thing, and it is replaced, not
// followed. missing says there's none at all.
func (c *Caddy) lastConfig() (b []byte, missing bool, err error) {
	b, err = readIn(c.configDir(), autosaveRel, maxConfigFile)
	switch {
	case err == nil:
		return b, false, nil
	case errors.Is(err, os.ErrNotExist):
		return nil, true, nil
	case errors.Is(err, errNotRegular):
		return nil, false, nil
	}
	return nil, false, err
}

// writeLastConfig replaces Caddy's last config, as its user, whole or not at all.
func (c *Caddy) writeLastConfig(b []byte) error {
	return writeIn(c.configDir(), autosaveRel, b, 0o600, proxyOwns())
}

// keepGood records what Caddy runs now as the last good config, if it is one and
// it has changed. It is best effort beside a change that has already worked:
// Recover tries again within a minute.
func (c *Caddy) keepGood(b []byte) error {
	if !c.usable(b) {
		return nil
	}
	c.goodMu.Lock()
	defer c.goodMu.Unlock()
	if bytes.Equal(c.good, b) {
		// It may have gone from under this, and nothing else would say so until
		// the day it's needed.
		if _, err := os.Stat(filepath.Join(c.Dir, goodFile)); err == nil {
			return nil
		}
	}
	if err := writeIn(c.Dir, goodFile, b, 0o600, agentOwns); err != nil {
		return fmt.Errorf("can't keep a copy of Caddy's config: %w", err)
	}
	c.good = bytes.Clone(b)
	return nil
}

// keepRunning reads what Caddy runs, and keeps it, as after a change. It doesn't
// fail what it follows.
func (c *Caddy) keepRunning(ctx context.Context) {
	if b, _, err := c.call(ctx, http.MethodGet, "/config/", nil, nil); err == nil {
		_ = c.keepGood(b)
	}
}

// readGood reads the agent's copy, or nil if it has none or can't use it.
func (c *Caddy) readGood() []byte {
	b, err := readIn(c.Dir, goodFile, maxConfigFile)
	if err != nil || !c.usable(b) {
		return nil
	}
	return b
}

// forgetGood drops the copy, once Caddy has stopped for its last route. A
// server's next first route starts from the bootstrap.
func (c *Caddy) forgetGood() error {
	c.goodMu.Lock()
	defer c.goodMu.Unlock()
	c.good = nil
	if err := os.Remove(filepath.Join(c.Dir, goodFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("can't remove the copy of Caddy's last config: %w", err)
	}
	root, err := os.OpenRoot(c.Dir)
	if err != nil {
		return fmt.Errorf("can't remove what a crash left of the copy of Caddy's last config: %w", err)
	}
	defer root.Close()
	if err := removeTemps(root, goodFile); err != nil {
		return fmt.Errorf("can't remove what a crash left of the copy of Caddy's last config: %w", err)
	}
	return nil
}

// forgetLastConfig removes Caddy's last config, with what else in its directory
// holds a copy, once it has stopped for its last route. The panel's key is in a
// config while that serves, and these would otherwise stay there for any later
// Caddy to read.
func (c *Caddy) forgetLastConfig() error {
	root, err := os.OpenRoot(c.configDir())
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("can't remove Caddy's last config: %w", err)
	}
	defer root.Close()
	for _, name := range []string{autosaveRel, autosaveRel + ".last", autosaveRel + ".damaged"} {
		if err := root.RemoveAll(name); err != nil {
			return fmt.Errorf("can't remove Caddy's last config: %w", err)
		}
	}
	if err := removeTemps(root, autosaveRel); err != nil {
		return fmt.Errorf("can't remove what a crash left of Caddy's last config: %w", err)
	}
	return nil
}

// repairFile puts the last config right while Caddy isn't answering: from the
// agent's copy if it has one, or else by setting the file aside, so Caddy starts
// from its bootstrap rather than exiting for ever. It reports what it did.
func (c *Caddy) repairFile() (repaired bool, err error) {
	b, missing, err := c.lastConfig()
	if err != nil {
		return false, err
	}
	good := c.readGood()
	switch {
	case missing && good == nil:
		return false, nil
	case !missing && !damaged(b):
		return false, nil
	}
	if good != nil {
		if err := c.writeLastConfig(good); err != nil {
			return false, fmt.Errorf("can't put back Caddy's last config: %w", err)
		}
		return true, nil
	}
	root, err := os.OpenRoot(c.configDir())
	if err != nil {
		return false, err
	}
	defer root.Close()
	if err := root.Rename(autosaveRel, autosaveRel+".damaged"); err != nil {
		return false, fmt.Errorf("can't set aside Caddy's damaged config: %w", err)
	}
	return true, nil
}

// Recover puts right a last config that a full disk or a power cut damaged, and
// keeps a copy of what Caddy runs now.
//
//   - With Caddy up and running a config, the file matters only at its next
//     start, and what it runs is the truth, so that's what goes back. A healthy
//     Caddy rewrites this file on every change, so it is read again before
//     anything is written over it.
//   - With Caddy up and running nothing, it started from an empty file, and the
//     agent's copy is loaded into it.
//   - With Caddy not answering, as when a file cut short has it restarting for
//     ever, the file is put right, for its next start.
func (c *Caddy) Recover(ctx context.Context) (recovered bool, err error) {
	running, _, err := c.call(ctx, http.MethodGet, "/config/", nil, nil)
	switch {
	case answeredByAnother(err):
		// Not down at all, and the log is the one place this reaches a person.
		return false, err
	case errors.Is(err, errDown):
		return c.repairFile()
	case err != nil:
		return false, err
	}
	// A Caddy that came up from an empty file runs nothing, and one that came up
	// from its bootstrap, as when its last config went missing, runs none of what
	// the agent last gave it. Either reads as a server with no routes, which the
	// agent would stop, so the copy goes back in.
	if damaged(running) || startedBare(running) {
		good := c.readGood()
		if good == nil || startedBare(good) {
			return false, nil
		}
		if _, _, err := c.call(ctx, http.MethodPost, "/load", good, nil); err != nil {
			return false, fmt.Errorf("can't load Caddy's last good config back into it: %w", err)
		}
		return true, nil
	}
	if err := c.keepGood(running); err != nil {
		return false, err
	}
	b, missing, err := c.lastConfig()
	if err != nil {
		return false, err
	}
	// Caddy writes its file on every change, so one that has gone is someone's
	// doing, and its next start would come up from the bootstrap.
	if missing {
		if err := c.writeLastConfig(running); err != nil {
			return false, fmt.Errorf("can't put back Caddy's last config: %w", err)
		}
		return true, nil
	}
	if !damaged(b) {
		return false, nil
	}
	// It may have been caught between Caddy's truncating and its writing.
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(autosaveSettle):
	}
	again, _, err := c.call(ctx, http.MethodGet, "/config/", nil, nil)
	if err != nil {
		return false, err
	}
	if b, missing, err = c.lastConfig(); err != nil || missing || !damaged(b) {
		return false, err
	}
	if err := c.writeLastConfig(again); err != nil {
		return false, fmt.Errorf("can't put back Caddy's last config: %w", err)
	}
	return true, nil
}

// startedBare reports a whole config with nothing of the agent's on either of
// Dockwell's servers. Every change writes a generation route, and the bootstrap has
// none, so this is a Caddy that started from that rather than from what it last ran,
// or a config loaded by hand.
func startedBare(config []byte) bool {
	var top struct {
		Apps struct {
			HTTP struct {
				Servers map[string]json.RawMessage `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if json.Unmarshal(config, &top) != nil {
		return false
	}
	return bareServers(top.Apps.HTTP.Servers)
}

// loadCopyFirst is for a change about to go in. On a Caddy running none of what the
// agent last gave it, as when its last config went missing, a change would write over
// the agent's copy, and removing a route would read as the last one going, so Caddy
// would stop and take the copy with it. So the copy goes back in first, and if Caddy
// won't take it, the change waits, with the copy as it was for the next try.
func (c *Caddy) loadCopyFirst(ctx context.Context, servers map[string]json.RawMessage) (loaded bool, err error) {
	if !bareServers(servers) {
		return false, nil
	}
	good := c.readGood()
	if good == nil || startedBare(good) {
		return false, nil
	}
	if _, _, err := c.call(ctx, http.MethodPost, "/load", good, nil); err != nil {
		return false, fmt.Errorf("caddy is running without the routes the agent last gave it, and won't load the agent's copy of them, so this change wasn't made, as it would replace them. The copy stays, and the agent tries to load it again within a minute. Read why with docker logs %s, then apply this again: %w", ingress.ContainerName, err)
	}
	return true, nil
}

// bareServers reports servers holding nothing the agent gave them: no generation
// route, which every change writes, and no route of Dockwell's. A route left where a
// person replaced the list by hand, which takes the generation route with it, is
// still what the agent gave it, and reads as an edit, not as a Caddy that lost it all.
func bareServers(servers map[string]json.RawMessage) bool {
	for _, name := range []string{serverName, httpServerName} {
		var server struct {
			Routes json.RawMessage `json:"routes"`
		}
		if len(servers[name]) == 0 || json.Unmarshal(servers[name], &server) != nil {
			continue
		}
		routes, err := decodeRoutes(server.Routes)
		if err != nil {
			continue
		}
		if slices.ContainsFunc(routes, func(r json.RawMessage) bool { return isGeneration(routeID(r)) || servesARoute(routeID(r)) }) {
			return false
		}
	}
	return true
}

// editLastConfig changes the routes in Caddy's last config, for when it is down:
// it is what Caddy serves when it starts again. It changes the agent's copy too,
// as that is what would be put back. No config, or a damaged one with no copy to
// put back, has no route to remove.
func (c *Caddy) editLastConfig(edit func([]json.RawMessage) []json.RawMessage) error {
	if _, err := c.repairFile(); err != nil {
		return err
	}
	b, missing, err := c.lastConfig()
	if err != nil {
		return fmt.Errorf("can't read Caddy's last config: %w", err)
	}
	if missing || damaged(b) {
		return nil
	}
	next, err := c.withRoutes(b, edit)
	if err != nil {
		return err
	}
	if err := c.writeLastConfig(next); err != nil {
		return fmt.Errorf("can't change Caddy's last config: %w", err)
	}
	return c.keepGood(next)
}

// withRoutes applies an edit to the routes in a whole config, and leaves the rest
// of it as it is, down to what Dockwell doesn't model.
func (c *Caddy) withRoutes(config []byte, edit func([]json.RawMessage) []json.RawMessage) ([]byte, error) {
	var top, apps, app, servers map[string]json.RawMessage
	if err := json.Unmarshal(config, &top); err != nil {
		return nil, fmt.Errorf("can't read Caddy's last config: %w", err)
	}
	if err := unmarshalIf(top["apps"], &apps); err != nil {
		return nil, err
	}
	if err := unmarshalIf(apps["http"], &app); err != nil {
		return nil, err
	}
	if err := unmarshalIf(app["servers"], &servers); err != nil {
		return nil, err
	}
	if len(servers[serverName]) == 0 {
		return config, nil
	}
	// Caddy starts from this, so it needs a generation of its own to be told by.
	generation, err := c.newGeneration()
	if err != nil {
		return nil, err
	}
	if err := c.editServers(servers, func(routes []json.RawMessage) ([]json.RawMessage, error) { return edit(routes), nil }, generation); err != nil {
		return nil, err
	}
	if app["servers"], err = json.Marshal(servers); err != nil {
		return nil, err
	}
	if apps["http"], err = json.Marshal(app); err != nil {
		return nil, err
	}
	if top["apps"], err = json.Marshal(apps); err != nil {
		return nil, err
	}
	return json.Marshal(top)
}

// unmarshalIf reads raw, if there's any. A missing part of a config stays nil.
func unmarshalIf[T any](raw json.RawMessage, into *T) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("can't read Caddy's last config: %w", err)
	}
	return nil
}
