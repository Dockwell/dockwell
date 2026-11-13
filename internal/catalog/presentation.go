// SPDX-License-Identifier: AGPL-3.0-only

package catalog

import (
	"cmp"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// Media is local to the panel. An entry cannot turn opening the catalog into a
// request to another server, an API procedure or a path outside the media folder.
var mediaPath = regexp.MustCompile(`^/catalog-media/[a-z0-9]+(?:[-_/][a-z0-9]+)*\.(svg|png|jpg|jpeg|webp|avif)$`)

// ownMedia is an image in the folder media beside an entry, in its own catalog,
// which the control plane serves from what it read of it (ADR 0077): a raster
// format alone, never SVG, which can hold a script. package folder checks the file.
var ownMedia = regexp.MustCompile(`^media/[a-z0-9]+(?:[-_][a-z0-9]+)*\.(png|jpg|jpeg|webp)$`)

// image says a path is one an entry's image may have: its own catalog's, or the
// panel's for an entry built into Dockwell. raster leaves out SVG and AVIF.
func image(path string, raster bool) bool {
	if ownMedia.MatchString(path) {
		return true
	}
	return mediaPath.MatchString(path) && (!raster || !strings.HasSuffix(path, ".svg"))
}

func checkPresentation(e *catalogv1.Entry) error {
	if utf8.RuneCountInString(e.GetDescription()) > 6000 {
		return errors.New("its description is longer than 6000 characters. Keep the app's overview within that length")
	}
	if icon := e.GetIcon(); icon != "" && !image(icon, false) {
		return errors.New("its icon needs an image in the folder media beside it, as media/<name>.webp, in WebP, PNG or JPEG")
	}
	for name, address := range map[string]string{"homepage": e.GetHomepage(), "documentation": e.GetDocumentation(), "app source": e.GetAppSource()} {
		if address != "" && !presentationLink(address) {
			return fmt.Errorf("its %s needs an HTTPS address without sign-in details", name)
		}
	}
	if len(e.GetFeatures()) > 12 {
		return errors.New("it lists more than 12 features. Keep the list to the app's main features")
	}
	for _, feature := range e.GetFeatures() {
		if strings.TrimSpace(feature) == "" || utf8.RuneCountInString(feature) > 240 {
			return errors.New("each feature needs between 1 and 240 characters")
		}
	}
	if len(e.GetScreenshots()) > 8 {
		return errors.New("it has more than 8 screenshots. Keep the previews to the app's main screens")
	}
	seen := map[string]bool{}
	for _, shot := range e.GetScreenshots() {
		p := shot.GetPath()
		if !image(p, true) {
			return errors.New("each screenshot needs an image in the folder media beside it, as media/<name>.webp, in WebP, PNG or JPEG")
		}
		if seen[p] {
			return fmt.Errorf("the screenshot %s is listed twice. Keep one copy", p)
		}
		seen[p] = true
		if strings.TrimSpace(shot.GetCaption()) == "" || utf8.RuneCountInString(shot.GetCaption()) > 240 {
			return errors.New("each screenshot needs a caption of 1 to 240 characters saying what it shows")
		}
		if !presentationLink(shot.GetSource()) {
			return errors.New("each screenshot needs an HTTPS source address without sign-in details")
		}
	}
	return nil
}

// Link reports whether an address is one an entry may link to: HTTPS, with a
// host and no sign-in details. A converter leaves out one that isn't.
func Link(address string) bool { return presentationLink(address) }

func presentationLink(address string) bool {
	u, err := url.Parse(address)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && !strings.ContainsAny(address, "\r\n\t ")
}

// FirstWords is what an install's plan says a person needs, in words, as the CLI
// shows a plan as the panel does: how to sign in, that its first account owns it, its
// notes and its first steps, a short sentence each, and the notes of each companion
// that runs. How to sign in to a companion left out, it doesn't say. A nil choice of
// companions runs every one. The panel shows the same as state on the app's page once
// it runs.
func FirstWords(e *catalogv1.Entry, choice map[string]bool) string {
	runs := func(service string) bool {
		_, companion := Companion(e, service)
		return choice == nil || !companion || choice[service]
	}
	var out []string
	if s := e.GetSignIn(); s != nil && runs(e.GetWeb().GetService()) {
		web := cmp.Or(e.GetWeb().GetService(), e.GetName())
		user := s.GetUser()
		for _, in := range e.GetInputs() {
			if user == "" && in.GetName() == s.GetUserInput() {
				label := strings.ToLower(in.GetLabel())
				if user = label; !strings.HasPrefix(label, "your ") {
					user = "the " + label + " you give"
				}
			}
		}
		server := ""
		if s.GetServer() != "" {
			server = " to server " + s.GetServer()
		}
		switch {
		case user != "" && s.GetPasswordInput() != "":
			out = append(out, fmt.Sprintf("Sign in to %s%s as %s, with the password in the %s secret.", web, server, user, s.GetPasswordInput()))
		case s.GetPasswordInput() != "":
			out = append(out, fmt.Sprintf("Sign in to %s with the key in the %s secret.", web, s.GetPasswordInput()))
		case user != "":
			out = append(out, fmt.Sprintf("Sign in to %s%s as %s.", web, server, user))
		}
	}
	if e.GetFirstVisitorOwns() {
		out = append(out, "Its first account is its owner, so make yours before it has a domain.")
	}
	notes := slices.Clone(e.GetNotes())
	for _, c := range e.GetCompanions() {
		if runs(c.GetService()) {
			notes = append(notes, c.GetNotes()...)
		}
	}
	for _, note := range notes {
		if note = strings.TrimSpace(note); note != "" {
			if !strings.HasSuffix(note, ".") {
				note += "."
			}
			out = append(out, note)
		}
	}
	if steps := strings.TrimSpace(e.GetFirstSteps()); steps != "" {
		out = append(out, steps)
	}
	return strings.Join(out, " ")
}
