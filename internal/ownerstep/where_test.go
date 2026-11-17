// SPDX-License-Identifier: AGPL-3.0-only

package ownerstep

import (
	"net/http"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// A where reads a path of keys, list indexes and picks of a list's object by one of
// its fields, in any case, then compares the scalar it reads, as it reads; ${NAME}
// fills from the values (ADR 0105).
func TestWhereReadsAPathAndAPick(t *testing.T) {
	t.Parallel()
	users := []byte(`[{"email":"other@example.com","_status":0},{"email":"you@example.com","_status":1,"name":"You","org":{"ok":true}},{"email":"z@example.com","_status":null}]`)
	vals := map[string]string{"OWNER_EMAIL": "You@Example.com"}
	for where, want := range map[string]bool{
		"[email=${OWNER_EMAIL}]._status=1":     true,
		"[email=${OWNER_EMAIL}]._status=0":     false,
		"[email=you@example.com].name=You":     true,
		"[email=you@example.com].name=you":     false,
		"[email=you@example.com].org.ok=true":  true,
		"1.email=you@example.com":              true,
		"5.email=you@example.com":              false,
		"[email=nobody@example.com]._status=0": false,
		"[email=z@example.com]._status=null":   true,
		"[email=you@example.com].org=true":     false,
		"email=you@example.com":                false,
	} {
		got, err := Where(where, users, vals)
		if err != nil || got != want {
			t.Errorf("Where(%q) = %v, %v, want %v", where, got, err, want)
		}
	}
	if got, err := Where("a.b=c", []byte(`{"a":{"b":"c"}}`), nil); !got || err != nil {
		t.Errorf("an object's path: %v, %v", got, err)
	}
	if got, err := Where("a=b", []byte(`<html>`), nil); got || err == nil {
		t.Errorf("an answer that isn't JSON: %v, %v", got, err)
	}
}

// A where is a small grammar, never an expression: anything else is refused as the
// catalog checks an entry.
func TestAWhereThatIsntOneIsRefused(t *testing.T) {
	t.Parallel()
	for _, where := range []string{
		"", "status", "[email=x]", "[email=x", "[email]=1", "[em ail=x].s=1", "a..b=1", "a b=1", "a.$x=1", "a=[1]",
		"a=1 || 1=1", "[email=x]]=1", "a.=1", ".a=1", "a[=1",
	} {
		if _, _, err := parseWhere(where); err == nil {
			t.Errorf("parseWhere(%q) took it", where)
		}
		if err := CheckCheck(&resourcev1.OwnerCheck{Request: &resourcev1.OwnerRequest{Path: "/u", Port: 80}, Where: where}); where != "" && err == nil {
			t.Errorf("CheckCheck took the where %q", where)
		}
	}
	for _, where := range []string{"a=", "a=1 or 2", "[email=${OWNER_EMAIL}]._status=0", "0.a-b_c=x.y"} {
		if _, _, err := parseWhere(where); err != nil {
			t.Errorf("parseWhere(%q): %v", where, err)
		}
	}
	if err := CheckCheck(&resourcev1.OwnerCheck{Command: &resourcev1.OwnerCommand{Words: []string{"true"}}, Where: "a=1"}); err == nil {
		t.Error("a command's check took a where")
	}
	if err := CheckRequest(&resourcev1.OwnerRequest{Port: 80, Capture: map[string]string{"S": "cookie:bad name"}}); err == nil {
		t.Error("a capture took a cookie that isn't a cookie's name")
	}
}

// A check's request sent first, as a sign-in, captures its cookie for the check
// alone: the read sends it, a where reads the answer, and what it captured is hidden
// in what the check says and kept nowhere (ADR 0105, Vaultwarden).
func TestACheckSignsInFirstWithACookieItKeepsForItselfAlone(t *testing.T) {
	t.Parallel()
	const session = "eyJhbGciOiJIUzI1NiJ9.a-session-the-app-made"
	registered := false
	tg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin":
			_ = r.ParseForm()
			if r.PostForm.Get("token") != password {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "VW_ADMIN", Value: session, Path: "/admin", HttpOnly: true})
		case "/admin/users":
			if c, err := r.Cookie("VW_ADMIN"); err != nil || c.Value != session {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			status := 1
			if registered {
				status = 0
			}
			_, _ = w.Write([]byte(`[{"email":"you@example.com","_status":` + map[int]string{0: "0", 1: "1"}[status] + `,"echo":"` + session + `"}]`))
		}
	})
	done := &resourcev1.OwnerCheck{
		Before: &resourcev1.OwnerRequest{
			Method: http.MethodPost, Path: "/admin", ContentType: "application/x-www-form-urlencoded", Body: "token=${OWNER_PASSWORD}", Port: 80,
			Capture: map[string]string{"VW": "cookie:VW_ADMIN"},
		},
		Request: &resourcev1.OwnerRequest{Path: "/admin/users", Port: 80, Headers: map[string]string{"Cookie": "VW_ADMIN=${VW}"}},
		Where:   "[email=${OWNER_EMAIL}]._status=0",
	}
	if err := CheckCheck(done); err != nil {
		t.Fatal(err)
	}
	o := owner()
	yes, said := Read(t.Context(), tg, o, done, values())
	if yes {
		t.Fatal("done read an invited user registered")
	}
	if strings.Contains(said, session) || strings.Contains(said, "a-session") {
		t.Errorf("what done says holds the session it captured: %q", said)
	}
	registered = true
	if yes, said := Read(t.Context(), tg, o, done, values()); !yes {
		t.Errorf("done doesn't read the registered user: %s", said)
	}
	// A wrong token: its sign-in is refused, and the read says no.
	wrong := values()
	wrong["OWNER_PASSWORD"] = "not-the-token"
	if yes, _ := Read(t.Context(), tg, o, done, wrong); yes {
		t.Error("done read it with a token the app refused")
	}
}

// A step captures a cookie its answer sets, for the steps after it, as a secret:
// what a later step's failure echoes of it is hidden, and nothing of it is kept.
func TestAStepCapturesACookieAsASecret(t *testing.T) {
	t.Parallel()
	const session = "a-cookie-the-app-set-0123456789"
	tg := serve(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: session})
		case "/invite":
			c, _ := r.Cookie("SID")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte("no invite for the session " + c.Value))
		}
	})
	login := post("/login", `{}`, 80)
	login.GetRequest().Capture = map[string]string{"SID": "cookie:SID"}
	invite := post("/invite", `{"email":"${OWNER_EMAIL}"}`, 80)
	invite.GetRequest().Headers = map[string]string{"Cookie": "SID=${SID}"}
	o := owner(login, invite)
	if err := Check(o.GetSteps()); err != nil {
		t.Fatal(err)
	}
	res := Run(t.Context(), tg, o, values(), false)
	if res.Made || res.Step != 2 || !strings.Contains(res.Said, "no invite for the session [hidden]") || strings.Contains(res.Said, session) {
		t.Errorf("Run = %+v, want step 2's failure with the cookie hidden", res)
	}
	missing := post("/plain", `{}`, 80)
	missing.GetRequest().Capture = map[string]string{"X": "cookie:NONE"}
	if res := Run(t.Context(), tg, owner(missing), values(), false); res.Made || !strings.Contains(res.Said, "sets no cookie NONE") {
		t.Errorf("a cookie its answer doesn't set: %+v", res)
	}
}
