// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

// signInWait is how long checking a sign-in takes at most: making a network of its
// own, a throwaway on it, and the server's answer.
const signInWait = 60 * time.Second

// CheckSignIn says whether a user signs in to a PostgreSQL, MySQL or MariaDB server
// on this server with a password, from another host, as taking the user on checks,
// and whether it's the server's administrator, changing nothing (ADR 0084, Connect a
// database). Nothing it says holds the password.
func (r Reader) CheckSignIn(ctx context.Context, call *agentv1.CheckDatabaseSignInCall, password string) (*agentv1.CheckDatabaseSignInResult, error) {
	if r.Client == nil {
		return nil, errors.New("this agent can't check a sign-in, as it has no Docker driver")
	}
	if err := CheckSQLName("user", call.GetUser()); err != nil {
		return nil, err
	}
	if err := CheckSQLName("database", call.GetDatabase()); err != nil {
		return nil, err
	}
	if ReservedUser(call.GetUser()) {
		return &agentv1.CheckDatabaseSignInResult{Administrator: true}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, signInWait)
	defer cancel()
	c, err := r.Client.ContainerOf(ctx, docker.Project(call.GetEnvironment()), call.GetService(), 1)
	switch {
	case docker.IsNotFound(err):
		return nil, fmt.Errorf("%s doesn't run on this server now. Deploy it, then check again", call.GetService())
	case err != nil:
		return nil, err
	}
	e := Recognise(c.Config.Image)
	if e == nil || signsInAs[e.Kind] == nil {
		return nil, fmt.Errorf("%s runs no PostgreSQL, MySQL or MariaDB that Dockwell knows", call.GetService())
	}
	// One check at a time on a server: each joins its running container to a network
	// of its own for a moment, and Docker has only so many to give.
	if !signingIn.TryLock(c.ID) {
		return nil, fmt.Errorf("a sign-in to %s is being checked already. Try again in a moment", call.GetService())
	}
	defer signingIn.Unlock(c.ID)
	out := &agentv1.CheckDatabaseSignInResult{}
	ok, said, err := Loader(r).SignsIn(ctx, Load{To: c, Engine: e, User: call.GetUser(), Password: password, Database: call.GetDatabase()})
	if err != nil {
		return nil, err
	}
	out.SignsIn = ok
	if !ok {
		out.Said = said
	}
	// What it reaches, as the plan asks it: one the server won't say of reads as
	// neither here, and the plan asks again before anything changes, refusing it there.
	if ok {
		if got, err := (&UserDriver{Client: r.Client}).reach(ctx, c, e, call.GetUser(), call.GetDatabase()); err == nil {
			out.Administrator, out.NoRights = got.beyond, !got.beyond && !got.own
		}
	}
	return out, nil
}

// keyedLock lets one holder at a time have each key.
type keyedLock struct {
	mu   sync.Mutex
	held map[string]bool
}

// signingIn is the database servers a sign-in is being checked on, by container.
var signingIn = &keyedLock{held: map[string]bool{}}

// TryLock takes key, or says it's taken.
func (k *keyedLock) TryLock(key string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.held[key] {
		return false
	}
	k.held[key] = true
	return true
}

// Unlock lets key go.
func (k *keyedLock) Unlock(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.held, key)
}
