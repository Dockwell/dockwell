// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dockwell/dockwell/internal/store"
)

// How often a person is asked for their passkey again. Each person chooses, and
// the install's owner sets the least anyone may: what applies is the stricter.
// Where it isn't each time, a session's cover stands in for a step-up's proof:
// a passkey within the hour, or, for someone who chose so, the sign-in itself.
// It never stands in for how someone signs in, for this choice or the floor, or
// for approving someone else's change, and never for an API token. See
// docs/adr/0075-how-often-a-passkey-is-asked-for-again.md.

// StepUpPolicy is how often someone is asked for their passkey again.
type StepUpPolicy string

// The choices, strictest first.
const (
	// AskEachTime asks for a step-up for each action, as ADR 0022 has it.
	AskEachTime StepUpPolicy = store.StepUpEachTime
	// AskHourly lets a passkey in the session within StepUpHour stand in.
	AskHourly StepUpPolicy = store.StepUpHourly
	// AskAtSignIn lets a sign-in with a passkey or an authenticator code
	// stand in for the session's life, and a passkey within the hour
	// otherwise.
	AskAtSignIn StepUpPolicy = store.StepUpAtSignIn
)

// StepUpHour is how long a passkey covers its session, from when it confirmed
// it, for someone who asked to be asked once an hour. Use doesn't extend it.
const StepUpHour = time.Hour

// ErrBelowFloor refuses a choice looser than the install allows.
var ErrBelowFloor = errors.New("this install asks everyone for their passkey at least as often as its owner set, so that choice isn't open to you. " +
	"Choose one at or above it, or ask the owner")

// rank orders the choices by how little they ask: anything unknown is each time.
func (p StepUpPolicy) rank() int {
	switch p {
	case AskEachTime:
		return 0
	case AskHourly:
		return 1
	case AskAtSignIn:
		return 2
	}
	return 0
}

// Valid reports one of the three choices.
func (p StepUpPolicy) Valid() bool {
	return p == AskEachTime || p == AskHourly || p == AskAtSignIn
}

// Stricter is whichever of a and b asks more often.
func Stricter(a, b StepUpPolicy) StepUpPolicy {
	if !a.Valid() || !b.Valid() {
		return AskEachTime
	}
	if b.rank() < a.rank() {
		return b
	}
	return a
}

// policyOf is what a person chose, each time when it's nothing Dockwell knows.
func policyOf(u store.User) StepUpPolicy {
	if p := StepUpPolicy(u.StepUpPolicy); p.Valid() {
		return p
	}
	return AskEachTime
}

// alwaysAsks are what a passkey confirms each time, whatever anyone chose: how
// someone signs in, as a stolen session would make itself a way back in; this
// choice and the floor, as it would loosen its own checks first; and approving
// someone else's change, which is the point of a protected environment.
var alwaysAsks = map[StepUpAction]bool{
	StepUpAddPasskey:    true,
	StepUpRemovePasskey: true,
	StepUpSetUpCodes:    true,
	StepUpDisableCodes:  true,
	StepUpRecoveryCodes: true,
	StepUpSetPolicy:     true,
	StepUpSetFloor:      true,
	StepUpApproveChange: true,
}

// AlwaysAsks reports an action a passkey confirms each time, whatever anyone chose.
func AlwaysAsks(action StepUpAction) bool { return alwaysAsks[action] }

// signInConfirms reports a sign-in that confirms a session for someone who
// chose AskAtSignIn: one with a passkey, which claiming and accepting an
// invite make, or with an authenticator code. A recovery code and a link from
// dockwell recover are for getting back in, and confirm nothing more.
func signInConfirms(method string) bool {
	switch method {
	case MethodPasskey, MethodClaim, MethodInvite, MethodAuthenticator:
		return true
	}
	return false
}

// signedInWith says how a session began, for the audit log.
func signedInWith(method string) string {
	switch method {
	case MethodPasskey:
		return "with a passkey"
	case MethodClaim:
		return "by claiming the install"
	case MethodInvite:
		return "by accepting an invite"
	case MethodAuthenticator:
		return "with an authenticator code"
	}
	return method
}

// Cover is how a session confirms what a step-up would, without one.
type Cover struct {
	// Since is when it began: the passkey, or signing in.
	Since time.Time
	// Until is when it ends: an hour after the passkey, or the session's end.
	Until time.Time
	// AtSignIn is true where signing in is what covers it, and Method says how
	// the session signed in.
	AtSignIn bool
	Method   string
}

// coverOf is p's session's cover under policy at now, if it has one.
func coverOf(p Principal, policy StepUpPolicy, now time.Time) (Cover, bool) {
	if p.Token != nil || p.Session.ID == "" || policy.rank() == 0 {
		return Cover{}, false
	}
	end := SessionEnd(p.Session)
	if !now.Before(end) {
		return Cover{}, false
	}
	if policy == AskAtSignIn && signInConfirms(p.Session.Method) {
		return Cover{Since: p.Session.CreatedAt, Until: end, AtSignIn: true, Method: p.Session.Method}, true
	}
	at := p.Session.PasskeyAt
	if at == nil || at.After(now) || !now.Before(at.Add(StepUpHour)) {
		return Cover{}, false
	}
	return Cover{Since: *at, Until: earlier(at.Add(StepUpHour), end)}, true
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// mayCover reports whether anything could stand in for a proof for p: a session,
// an action that doesn't always ask, and a choice other than each time. It reads
// nothing, so someone who asks each time pays nothing for this.
func mayCover(p Principal, action StepUpAction) bool {
	_, known := stepUpActions[action]
	return known && p.Token == nil && p.Session.ID != "" && !alwaysAsks[action] && policyOf(p.User).rank() > 0
}

// ReadStepUpFloor reads the least anyone may choose: AskAtSignIn, which allows
// all three, until the owner sets it, and each time for anything Dockwell
// doesn't know.
func ReadStepUpFloor(ctx context.Context, tx *store.Tx) (StepUpPolicy, error) {
	v, err := tx.GetSetting(ctx, store.SettingStepUpFloor)
	if errors.Is(err, store.ErrNotFound) {
		return AskAtSignIn, nil
	}
	if err != nil {
		return "", err
	}
	if p := StepUpPolicy(v); p.Valid() {
		return p, nil
	}
	return AskEachTime, nil
}

// coverIn checks, in tx, whether p's session confirms action on target without
// a proof, and if it does, puts how on the audit log, in tx. It reports
// ErrNeedsStepUp where it doesn't.
func (s *Service) coverIn(ctx context.Context, tx *store.Tx, client Client, p Principal, action StepUpAction, target string) error {
	if !mayCover(p, action) {
		return ErrNeedsStepUp
	}
	if !stepUpActions[action].targeted {
		target = p.User.ID
	}
	floor, err := ReadStepUpFloor(ctx, tx)
	if err != nil {
		return err
	}
	c, ok := coverOf(p, Stricter(policyOf(p.User), floor), s.now())
	if !ok {
		return ErrNeedsStepUp
	}
	from := ""
	if ip := client.remoteIP(); ip != "" {
		from = ", from " + ip
	}
	what := doing(action, target)
	if c.AtSignIn {
		return s.auditOn(ctx, tx, p.Actor(), "auth.step_up_sign_in", p.User.ID, target,
			fmt.Sprintf("confirmed %s by signing in %s at %s, session %s%s", what, signedInWith(c.Method), c.Since.UTC().Format(time.RFC3339), p.Session.ID, from))
	}
	return s.auditOn(ctx, tx, p.Actor(), "auth.step_up_hour", p.User.ID, target,
		fmt.Sprintf("confirmed %s within an hour of the passkey at %s, session %s%s", what, c.Since.UTC().Format(time.RFC3339), p.Session.ID, from))
}

// StepUpChoice is how often someone is asked for their passkey again, and what
// it comes to in the session asking.
type StepUpChoice struct {
	// Policy is what they chose, Floor the least the install allows, and
	// Effective the stricter of the two, which applies.
	Policy, Floor, Effective StepUpPolicy
	// Cover is the session's cover now, if it has one.
	Cover *Cover
}

func (s *Service) choiceOf(p Principal, policy, floor StepUpPolicy) StepUpChoice {
	out := StepUpChoice{Policy: policy, Floor: floor, Effective: Stricter(policy, floor)}
	if c, ok := coverOf(p, out.Effective, s.now()); ok {
		out.Cover = &c
	}
	return out
}

// GetStepUpPolicy says how often p is asked for their passkey again, and
// whether their session needs none now.
func (s *Service) GetStepUpPolicy(ctx context.Context, p Principal) (StepUpChoice, error) {
	if p.Token != nil || p.Session.ID == "" {
		return StepUpChoice{}, ErrNeedsSession
	}
	var floor StepUpPolicy
	err := s.db.View(ctx, func(tx *store.Tx) (err error) {
		floor, err = ReadStepUpFloor(ctx, tx)
		return err
	})
	if err != nil {
		return StepUpChoice{}, err
	}
	return s.choiceOf(p, policyOf(p.User), floor), nil
}

// SetStepUpPolicy chooses how often p is asked for their passkey again. It
// refuses a choice below the install's floor, and then needs a proof for
// StepUpSetPolicy, whichever way it goes. Choosing what's chosen already
// changes nothing and asks nothing.
func (s *Service) SetStepUpPolicy(ctx context.Context, client Client, p Principal, policy StepUpPolicy, proof string) (StepUpChoice, error) {
	if p.Token != nil || p.Session.ID == "" {
		return StepUpChoice{}, ErrNeedsSession
	}
	if !policy.Valid() {
		return StepUpChoice{}, fmt.Errorf("%w: choose how often you're asked for your passkey: each time, once an hour, or only when you sign in", ErrInvalid)
	}
	was := policyOf(p.User)
	var floor StepUpPolicy
	err := s.db.Update(ctx, func(tx *store.Tx) error {
		var err error
		if floor, err = ReadStepUpFloor(ctx, tx); err != nil {
			return err
		}
		if policy == was {
			return nil
		}
		if policy.rank() > floor.rank() {
			return ErrBelowFloor
		}
		if err := s.UseStepUpIn(ctx, tx, client, p, proof, StepUpSetPolicy, p.User.ID); err != nil {
			return err
		}
		if err := tx.SetStepUpPolicy(ctx, p.User.ID, string(policy)); err != nil {
			return err
		}
		return s.audit(ctx, tx, p.Actor(), "auth.step_up_policy", p.User.ID,
			fmt.Sprintf("from %s to %s", policyWords(was), policyWords(policy)))
	})
	if err != nil {
		return StepUpChoice{}, err
	}
	return s.choiceOf(p, policy, floor), nil
}

// policyWords says a choice for the audit log.
func policyWords(p StepUpPolicy) string {
	switch p {
	case AskEachTime:
		return "each time"
	case AskHourly:
		return "once an hour"
	case AskAtSignIn:
		return "only when signing in"
	}
	return "each time"
}
