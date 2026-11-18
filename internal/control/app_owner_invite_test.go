// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/owners"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// inviteEntry is an app whose owner registers through an invite, as Vaultwarden's:
// its admin token is generated and sealed, its service runs the token's Argon2id
// hash, and once the owner has registered, the release takes the hash out.
func inviteEntry() string {
	return fmt.Sprintf(`id: thing
name: Thing
summary: Something whose owner registers
category: storage
version: "1.0"
release: 1
license: AGPL-3.0-only
appLicense: MIT
source:
  repository: https://github.com/Dockwell/dockwell
web:
  service: web
owner:
  service: web
  emailInput: OWNER_EMAIL
  madeWhenDone: true
  unsetWhenMade: [ADMIN_TOKEN]
  steps:
    - request:
        method: POST
        path: /admin/invite
        contentType: application/json
        body: '{"email":"${OWNER_EMAIL}","token":"${ADMIN_TOKEN_PLAIN}"}'
        port: 80
  done:
    request:
      path: /admin/users
      port: 80
    where: "[email=${OWNER_EMAIL}]._status=0"
signups:
  service: web
  variable: SIGNUPS_ALLOWED
  closed: "false"
inputs:
  - name: PORT
    label: Port
    type: INPUT_TYPE_PORT
    defaultValue: "18100"
  - name: OWNER_EMAIL
    label: Owner's email
    type: INPUT_TYPE_EMAIL
  - name: ADMIN_TOKEN_PLAIN
    label: Admin token
    type: INPUT_TYPE_SECRET
    generate: 48
  - name: ADMIN_TOKEN
    label: Admin token's hash
    type: INPUT_TYPE_SECRET
    hashOf: ADMIN_TOKEN_PLAIN
compose: |
  services:
    web:
      image: %s
      environment:
        ADMIN_TOKEN: ${ADMIN_TOKEN}
        SIGNUPS_ALLOWED: "true"
      ports:
        - "${PORT}:80"
      volumes:
        - data:/data
  volumes:
    data: {}
`, firstRelease)
}

// inviteApp is an agent for an app whose owner registers once invited: its steps
// invite, and its done reads registered once the test says the owner has.
type inviteApp struct {
	mu         sync.Mutex
	registered bool
	sent       []map[string]string
}

func (a *inviteApp) MakeAppOwner(_ context.Context, _, _ string, run *resourcev1.OwnerRun, values map[string]string, _ bool) (*agentv1.MakeAppOwnerResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = append(a.sent, maps.Clone(values))
	if run.GetMadeWhenDone() && !a.registered {
		return &agentv1.MakeAppOwnerResult{Waiting: true}, nil
	}
	return &agentv1.MakeAppOwnerResult{Made: true}, nil
}

func (a *inviteApp) ReadAppOwner(context.Context, string, string, *resourcev1.OwnerRun, map[string]string, bool) (*agentv1.ReadAppOwnerResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return &agentv1.ReadAppOwnerResult{Yes: a.registered}, nil
}

// verifies reads whether an Argon2id PHC string is of secret, with what it says.
func verifies(phc, secret string) bool {
	parts := strings.Split(phc, "$")
	var m, tm uint32
	var p uint8
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &tm, &p); err != nil {
		return false
	}
	salt, err1 := b64raw(parts[4])
	key, err2 := b64raw(parts[5])
	return err1 == nil && err2 == nil && bytes.Equal(argon2.IDKey([]byte(secret), salt, tm, m, p, uint32(len(key))), key)
}

// An invite's owner: the install seals a generated admin token and runs its Argon2id
// hash, $ and all; the steps get the token, never the hash; the install stays held
// while the owner hasn't registered, read every five minutes; and once done reads
// them registered, the release opens it and takes the hash out of the service, so
// its admin page is off (ADR 0105, Vaultwarden).
func TestAnInvitesOwnerIsMadeOnceTheyveRegistered(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o := startOwning(t, backend, inviteEntry(), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
		app := &inviteApp{}
		o.f.server.owners.SetAgent(func(string) (owners.Agent, bool) { return app, true })
		var mu sync.Mutex
		now := time.Now()
		o.f.server.owners.SetNow(func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		})
		plain, _ := o.spec(variable.SecretKind, o.env, "ADMIN_TOKEN_PLAIN").(*resourcev1.Secret)
		hash, _ := o.spec(variable.SecretKind, o.env, "ADMIN_TOKEN").(*resourcev1.Secret)
		if len(plain.GetValue()) != 48 || !verifies(hash.GetValue(), plain.GetValue()) {
			t.Fatalf("the hash %q isn't the token's", hash.GetValue())
		}
		if env := o.web().Config.Env; !slices.Contains(env, "ADMIN_TOKEN="+hash.GetValue()) || slices.ContainsFunc(env, func(e string) bool { return strings.Contains(e, plain.GetValue()) }) {
			t.Errorf("web runs with %v, want the hash as it is and never the token", env)
		}
		o.look()
		if len(app.sent) != 1 || app.sent[0]["ADMIN_TOKEN_PLAIN"] != plain.GetValue() || app.sent[0]["ADMIN_TOKEN"] != "" {
			t.Fatalf("the steps were sent %d times, the token %v, the hash %v", len(app.sent), app.sent[0]["ADMIN_TOKEN_PLAIN"] == plain.GetValue(), app.sent[0]["ADMIN_TOKEN"] != "")
		}
		if in := o.install(); in.GetOwner().GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_WAITING || !in.GetHeld() {
			t.Fatalf("invited, the install reads %v, held %v, want waiting and held", in.GetOwner(), in.GetHeld())
		}
		app.mu.Lock()
		app.registered = true
		app.mu.Unlock()
		o.look()
		if in := o.install(); in.GetOwner().GetState() == dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_MADE {
			t.Fatal("done was read again sooner than five minutes after the invite")
		}
		mu.Lock()
		now = now.Add(owners.Poll + time.Second)
		mu.Unlock()
		o.look()
		if len(app.sent) != 1 {
			t.Errorf("the invite was sent %d times, want once, then done read alone", len(app.sent))
		}
		in := o.install()
		if in.GetOwner().GetState() != dockwellv1.CatalogOwnerState_CATALOG_OWNER_STATE_MADE || in.GetHeld() {
			t.Fatalf("registered, the install reads %v, held %v", in.GetOwner(), in.GetHeld())
		}
		web, _ := o.spec(service.Kind, o.env, "web").(*resourcev1.Service)
		if _, ok := web.GetSecrets()["ADMIN_TOKEN"]; ok || web.GetApp().GetEnv()["ADMIN_TOKEN"] != "" {
			t.Errorf("once its owner registered, web still takes ADMIN_TOKEN: %v", web.GetSecrets())
		}
		if env := o.web().Config.Env; slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, "ADMIN_TOKEN=") }) {
			t.Errorf("web runs with %v, want no ADMIN_TOKEN", env)
		}
	})
}

func b64raw(s string) ([]byte, error) { return base64.RawStdEncoding.DecodeString(s) }

// An install from before its entry made owners, whose owner a person made on their own,
// upgraded to a release whose owner registers through an invite: the release's
// ADMIN_TOKEN, there only to make the owner, stays out, as the owner is made already.
// Before, the upgrade turned Vaultwarden's admin page on with a token nobody was shown,
// which nothing then took away (catalog#37's proof).
func TestAnUpgradeGainingAnInviteOwnerTurnsNoAdminPageOn(t *testing.T) {
	t.Parallel()
	storetest.Each(t, func(t *testing.T, backend store.Backend) {
		o := startOwning(t, backend, inviteEntryBefore(), &dockwellv1.InstallCatalogEntryRequest{Id: "thing"})
		o.release(strings.Replace(inviteEntry(), "release: 1", "release: 2", 1))
		o.applied(o.upgrade())
		web, _ := o.spec(service.Kind, o.env, "web").(*resourcev1.Service)
		if _, ok := web.GetSecrets()["ADMIN_TOKEN"]; ok || web.GetApp().GetEnv()["ADMIN_TOKEN"] != "" {
			t.Errorf("an install from before, upgraded, takes ADMIN_TOKEN: %v", web.GetSecrets())
		}
		if env := o.web().Config.Env; slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, "ADMIN_TOKEN=") }) {
			t.Errorf("web runs with %v, want no ADMIN_TOKEN", env)
		}
		if in := o.install(); in.GetOwner() != nil {
			t.Errorf("an install from before reads %v, want no owner to make", in.GetOwner())
		}
	})
}

// inviteEntryBefore is inviteEntry as a release from before it made owners: its first
// visitor makes the owner, and it has no admin token.
func inviteEntryBefore() string {
	return fmt.Sprintf(`id: thing
name: Thing
summary: Something whose owner registers
category: storage
version: "1.0"
release: 1
license: AGPL-3.0-only
appLicense: MIT
source:
  repository: https://github.com/Dockwell/dockwell
web:
  service: web
firstVisitorOwns: true
signups:
  service: web
  variable: SIGNUPS_ALLOWED
  closed: "false"
inputs:
  - name: PORT
    label: Port
    type: INPUT_TYPE_PORT
    defaultValue: "18100"
compose: |
  services:
    web:
      image: %s
      environment:
        SIGNUPS_ALLOWED: "true"
      ports:
        - "${PORT}:80"
      volumes:
        - data:/data
  volumes:
    data: {}
`, firstRelease)
}
