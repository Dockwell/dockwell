// SPDX-License-Identifier: AGPL-3.0-only

package authapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/dockwell/dockwell/internal/agent/signin"
	"github.com/dockwell/dockwell/internal/auth"
)

// ProviderCallback takes a provider's way back to the panel, at
// auth.ProviderCallbackPath, behind Guard: it signs someone in, or links an account
// to them, and goes on to the panel (ADR 0095). It's a navigation from another site,
// which carries the flow's SameSite=Lax cookie and no session, so what it does is
// the flow's: the flow is used once, and its cookie cleared, whatever happens.
//
// It answers with a page that goes on to the panel, not a redirect: the session
// cookie is SameSite=Strict, which a browser doesn't send on a redirect chain that
// began at another site.
func ProviderCallback(svc *auth.Service, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, ok := ClientFrom(r.Context())
		if !ok {
			signin.Page(w, http.StatusInternalServerError, "Sign-in didn't finish", "", "Sign in", "/", false)
			return
		}
		http.SetCookie(w, flowCookie("", -1))
		c, err := r.Cookie(auth.ProviderFlowCookie)
		if err != nil {
			signin.Page(w, http.StatusBadRequest, "Sign-in didn't finish",
				"It took longer than 10 minutes, or began in another browser.", "Sign in", "/", false)
			return
		}
		done, err := svc.FinishProvider(r.Context(), client, c.Value, r.URL.Query())
		switch {
		case err == nil && done.SignedIn != nil:
			cookie, err := done.SignedIn.Cookie(client.Site)
			if err != nil {
				logger.ErrorContext(r.Context(), "can't set the session cookie of a sign-in with a provider", slog.Any("error", err))
				signin.Page(w, http.StatusInternalServerError, "Sign-in didn't finish", "", "Sign in", "/", false)
				return
			}
			w.Header().Add("Set-Cookie", cookie)
			signin.Page(w, http.StatusOK, "Signed in with "+done.Provider, "", "Go to the panel", "/", true)
		case err == nil && done.Linked != nil:
			signin.Page(w, http.StatusOK, "Linked your account at "+done.Provider, "", "Back to Profile and security", "/profile", true)
		case errors.Is(err, auth.ErrCeremonyExpired):
			signin.Page(w, http.StatusBadRequest, "Sign-in didn't finish",
				"It took longer than 10 minutes, or began in another browser.", "Sign in", "/", false)
		case errors.Is(err, auth.ErrProviderNotLinked):
			signin.Page(w, http.StatusForbidden, "This account isn't linked to anyone here",
				"Sign in with your passkey, then link it in Profile and security.", "Sign in", "/", false)
		case errors.Is(err, auth.ErrProviderLinkedElse), errors.Is(err, auth.ErrProviderLinkedTwice):
			signin.Page(w, http.StatusConflict, "That account wasn't linked", sentence(err.Error()), "Back to Profile and security", "/profile", false)
		case errors.Is(err, auth.ErrUnauthenticated):
			signin.Page(w, http.StatusUnauthorized, "That account wasn't linked",
				"The session you began linking it in has ended. Sign in, then link it again.", "Sign in", "/", false)
		case errors.Is(err, auth.ErrProviderBusy), errors.Is(err, auth.ErrProviderNotOffered):
			signin.Page(w, http.StatusServiceUnavailable, "Sign-in didn't finish", sentence(err.Error()), "Sign in", "/", false)
		default:
			// What the provider or the exchange said, which names the provider and
			// how to go on, and holds nothing of the flow's.
			logger.InfoContext(r.Context(), "a sign-in with a provider didn't finish", slog.String("from", client.Addr), slog.Any("error", err))
			signin.Page(w, http.StatusBadGateway, "Sign-in didn't finish", sentence(err.Error()), "Sign in", "/", false)
		}
	})
}
