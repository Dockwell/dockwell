// SPDX-License-Identifier: AGPL-3.0-only

package revision

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
)

// Upgrading a server's agent (ADR 0094): the control plane's own install script
// runs on the server, for the control plane's own version. The plan names the
// script by its SHA-256, the step's call carries it, and the agent checks it
// before it runs it. The step passes once the agent connects at that version, as
// a reboot's passes once it connects from another boot, and fails past
// RebootLimit, as one does.

// ErrNoRelease is a control plane that isn't a release, which upgrades no agent.
var ErrNoRelease = errors.New("the control plane isn't a release")

// ErrUpgradeLost is a server whose agent hasn't come back at the version asked.
var ErrUpgradeLost = errors.New("the agent hasn't come back after its upgrade")

// AgentRelease is what the control plane upgrades agents to: its own version, and
// the install script it was released with.
type AgentRelease struct {
	// Version is the release, without a "v", such as 0.9.4.
	Version string
	// Script is the install script, scripts/install.sh as the release ships it.
	Script []byte
	// Mirror is where the script downloads the release from, when it isn't GitHub.
	Mirror string
}

// SetAgentRelease says what the control plane upgrades agents to. Until it's said,
// or for a build that isn't a release, it upgrades none.
func (e *Engine) SetAgentRelease(r AgentRelease) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	r.Version = strings.TrimPrefix(r.Version, "v")
	e.release = &r
}

// AgentReleaseOf is the version the control plane upgrades agents to, and the
// SHA-256 of the script that does it, in hex, or ErrNoRelease.
func (e *Engine) AgentReleaseOf() (version, scriptSHA256 string, err error) {
	e.pmu.Lock()
	r := e.release
	e.pmu.Unlock()
	if r == nil || r.Version == "" || len(r.Script) == 0 {
		return "", "", fault.New(ErrNoRelease, "this control plane is a development build, so it has no release to upgrade agents to. Upgrade agents by running the install script on each server")
	}
	sum := sha256.Sum256(r.Script)
	return r.Version, hex.EncodeToString(sum[:]), nil
}

// upgradeAsk is the upgrade asked, with the script and the mirror its step's call
// carries: the control plane's, which must still be the one the plan named.
func (e *Engine) upgradeAsk(ask resource.AgentUpgradeAsked) (resource.AgentUpgradeAsked, error) {
	version, sum, err := e.AgentReleaseOf()
	if err != nil {
		return ask, err
	}
	if version != ask.Version || sum != ask.ScriptSHA256 {
		return ask, fault.New(ErrPlanChanged, "the control plane runs %s now, with another install script than the plan named, so no agent was upgraded to %s. Plan the upgrade again", version, ask.Version)
	}
	e.pmu.Lock()
	ask.Script, ask.Mirror = e.release.Script, e.release.Mirror
	e.pmu.Unlock()
	return ask, nil
}

// upgradeIn is the upgrade of its agent change c asks of ref, as its plan fixed it,
// or nil.
func upgradeIn(c store.Change, ref resource.Ref) *resource.AgentUpgradeAsked {
	p, err := DecodePlan(c.Plan)
	if err != nil {
		return nil
	}
	for i := range p.Resources {
		if r := &p.Resources[i]; r.Ref == ref && r.AgentUpgrade != nil {
			return &resource.AgentUpgradeAsked{Change: c.ID, Version: r.AgentUpgrade.To, ScriptSHA256: r.AgentUpgrade.ScriptSHA256}
		}
	}
	return nil
}

// UpgradesItself reports whether server's agent is connected and upgrades itself by
// the script a change carries (ADR 0094): an agent from before can't, and is
// upgraded by the install script on its server.
func (e *Engine) UpgradesItself(server string) bool {
	a, online := e.agents.Agent(server)
	u, ok := a.(interface{ UpgradesItself() bool })
	return online && ok && u.UpgradesItself()
}

// waitsForServer reports an item whose step waits, once it's applied, for its
// server's agent to connect again: from another boot, or at another version.
func (it *item) waitsForServer() bool { return it.reboot != "" || it.upgrade != nil }

// waitsOn reports a planned resource whose step waits for its server's agent to
// connect again, as a reboot's and an agent upgrade's do.
func (r *ResourcePlan) waitsOn() bool { return r.Reboot != nil || r.AgentUpgrade != nil }
