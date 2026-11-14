// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// What signs in as a server's administrator is its password's secret, wherever the
// server takes it: the image's variable, redis-cli's REDISCLI_AUTH, or --requirepass in
// its command. A server that asks no one for a password is unguarded, and never
// shared (ADR 0084).
func TestAServersPasswordIsFoundWhereverItIsTaken(t *testing.T) {
	for name, tc := range map[string]struct {
		app       *resourcev1.Container
		secrets   map[string]string
		secret    string
		unguarded bool
	}{
		"redis variable":     {app: &resourcev1.Container{Image: "redis:7"}, secrets: map[string]string{"REDIS_PASSWORD": "CACHE_PASSWORD"}, secret: "CACHE_PASSWORD", unguarded: true},
		"redis cli auth":     {app: &resourcev1.Container{Image: "redis:7"}, secrets: map[string]string{"REDISCLI_AUTH": ""}, secret: "REDISCLI_AUTH", unguarded: true},
		"redis requirepass":  {app: &resourcev1.Container{Image: "redis:7", Command: []string{"redis-server", "--requirepass", "${CACHE_PW}"}}, secrets: map[string]string{"CACHE_PW": "SHARED_PW"}, secret: "SHARED_PW"},
		"redis none":         {app: &resourcev1.Container{Image: "redis:7"}, unguarded: true},
		"redis literal":      {app: &resourcev1.Container{Image: "redis:7", Command: []string{"redis-server", "--requirepass", "typed"}}},
		"valkey conf":        {app: &resourcev1.Container{Image: "valkey/valkey:8", Command: []string{"valkey-server", "/etc/valkey/valkey.conf"}}},
		"mongo root":         {app: &resourcev1.Container{Image: "mongo:8", Env: map[string]string{"MONGO_INITDB_ROOT_USERNAME": "root"}}, secrets: map[string]string{"MONGO_INITDB_ROOT_PASSWORD": "ROOT"}, secret: "ROOT"},
		"mongo without root": {app: &resourcev1.Container{Image: "mongo:8"}, unguarded: true},
		"mongo with auth":    {app: &resourcev1.Container{Image: "mongo:8", Command: []string{"mongod", "--auth"}}},
	} {
		t.Run(name, func(t *testing.T) {
			f, ok := FromSpec(&resourcev1.Service{Runs: &resourcev1.Service_App{App: tc.app}, Secrets: tc.secrets})
			if !ok {
				t.Fatal("not read as a database")
			}
			if f.PasswordSecret != tc.secret || f.Unguarded != tc.unguarded {
				t.Errorf("found %q, unguarded %t; want %q, %t", f.PasswordSecret, f.Unguarded, tc.secret, tc.unguarded)
			}
		})
	}
}

// A Redis's reading lists its ACL users by name, with whether each can sign in and
// reaches past a prefix of its own, and nothing of their password hashes.
func TestARedisReadingListsItsUsers(t *testing.T) {
	said := "# Server\nredis_version:7.4.1\n" +
		"user default on #0123 ~* &* +@all\nuser shop on #4567 ~shop:* &shop:* -@all +@read\nuser ops on #89ab ~* &* +@all -@admin\n"
	r, err := parseRedis(Of(dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS), []byte(said))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, u := range r.GetUsers() {
		got[u.GetName()] = u.GetReachesPast()
		if strings.Contains(u.String(), "#") {
			t.Errorf("the reading keeps a hash: %v", u)
		}
	}
	if len(got) != 3 || got["shop"] || !got["ops"] || r.GetVersion() != "7.4.1" {
		t.Errorf("users %v, version %q", got, r.GetVersion())
	}
}
