// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/dockwell/dockwell/internal/auth/authtest"
)

// storedPasskey reads the owner's only passkey as stored.
func (f *fixture) storedPasskey(t *testing.T, owner SignedIn) Passkey {
	t.Helper()
	keys, err := f.svc.ListPasskeys(t.Context(), ownerPrincipal(owner))
	if err != nil || len(keys) != 1 {
		t.Fatalf("passkeys = %+v, %v", keys, err)
	}
	return keys[0]
}

// finishAtOnce finishes passkey sign-ins concurrently, one per count, each
// signed as if the passkey had reached that count, and returns the error of
// each.
func (f *fixture) finishAtOnce(t *testing.T, a *authtest.Authenticator, counts []uint32) []error {
	t.Helper()
	cred := a.Credentials()[0]
	ids, resps := make([]string, len(counts)), make([]string, len(counts))
	for i, count := range counts {
		cer, err := f.svc.BeginSignIn(t.Context(), fromAddress(i))
		if err != nil {
			t.Fatal(err)
		}
		cred.Count = count - 1 // Get adds one
		if resps[i], err = a.Get(string(cer.Options), localhost.Site.Origin, cred); err != nil {
			t.Fatal(err)
		}
		ids[i] = cer.ID
	}
	errs := make([]error, len(counts))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range counts {
		wg.Go(func() {
			<-start
			_, errs[i] = f.svc.FinishSignIn(context.Background(), fromAddress(i), ids[i], []byte(resps[i]))
		})
	}
	close(start)
	wg.Wait()
	return errs
}

// A copy and the original signing in at once, with the same count: one
// gets in, and the others are refused as copies.
func TestRacingSignInsWithOneCountFlagTheCopy(t *testing.T) {
	f := newFixture(t, "")
	a := &authtest.Authenticator{Counter: true}
	owner := f.claim(t, localhost, a)
	const racers = 16
	counts := make([]uint32, racers)
	for i := range counts {
		counts[i] = 5
	}
	var won, copied int
	for _, err := range f.finishAtOnce(t, a, counts) {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrPasskeyCopied):
			copied++
		default:
			t.Errorf("FinishSignIn: %v", err)
		}
	}
	if won != 1 || copied != racers-1 {
		t.Fatalf("%d sign-ins with one count: %d got in and %d were refused as copies, want 1 and %d", racers, won, copied, racers-1)
	}
	if pk := f.storedPasskey(t, owner); pk.CloneWarningAt == nil || pk.Credential.Authenticator.SignCount != 5 {
		t.Errorf("after the race: flagged %v, count %d, want flagged at count 5", pk.CloneWarningAt, pk.Credential.Authenticator.SignCount)
	}
}

// Sign-ins racing with different counts can't lower the stored count: the
// one with the lower count, if it comes second, is refused as a copy.
func TestTheStoredCountNeverGoesBackwards(t *testing.T) {
	f := newFixture(t, "")
	a := &authtest.Authenticator{Counter: true}
	owner := f.claim(t, localhost, a)
	stored := uint32(1)
	for round := range 20 {
		counts := []uint32{stored + 9, stored + 4}
		errs := f.finishAtOnce(t, a, counts)
		pk := f.storedPasskey(t, owner)
		for i, err := range errs {
			if err == nil && pk.Credential.Authenticator.SignCount < counts[i] {
				t.Fatalf("round %d: stored count %d after a sign-in with %d", round, pk.Credential.Authenticator.SignCount, counts[i])
			}
		}
		if pk.Credential.Authenticator.SignCount < stored {
			t.Fatalf("round %d: the stored count went from %d to %d", round, stored, pk.Credential.Authenticator.SignCount)
		}
		if pk.CloneWarningAt != nil {
			return // the lower count came second and was caught
		}
		stored = pk.Credential.Authenticator.SignCount
	}
}

// When flagging a copied passkey fails, the sign-in is refused with the
// error, and the passkey is flagged on its next sign-in, even the
// original's.
func TestAFailedCloneFlagIsTriedAgain(t *testing.T) {
	f := newFixture(t, "")
	a := &authtest.Authenticator{Counter: true}
	owner := f.claim(t, localhost, a)
	cred := a.Credentials()[0]
	if _, err := f.passkeySignIn(t, localhost, a, nil); err != nil {
		t.Fatal(err)
	}

	f.execSQL(t, `CREATE TRIGGER no_flags BEFORE UPDATE OF clone_warning_at ON passkeys BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END`)
	original := cred.Count
	cred.Count = 0 // a copy that missed the last uses
	_, err := f.passkeySignIn(t, localhost, a, nil)
	if err == nil || errors.Is(err, ErrPasskeyCopied) || !strings.Contains(err.Error(), "disk I/O error") {
		t.Fatalf("a copy whose flag can't be stored = %v, want the error that stopped it", err)
	}
	if f.storedPasskey(t, owner).CloneWarningAt != nil {
		t.Fatal("flagged in spite of the failed write")
	}

	f.execSQL(t, `DROP TRIGGER no_flags`)
	cred.Count = original + 5 // the original, moving on
	if _, err := f.passkeySignIn(t, localhost, a, nil); !errors.Is(err, ErrPasskeyCopied) {
		t.Errorf("the next sign-in after a failed flag = %v, want ErrPasskeyCopied", err)
	}
	if f.storedPasskey(t, owner).CloneWarningAt == nil {
		t.Error("the passkey still isn't flagged")
	}
	if !contains(f.events(t), "passkey.clone_suspected") {
		t.Error("the suspected copy isn't in the audit log")
	}
}
