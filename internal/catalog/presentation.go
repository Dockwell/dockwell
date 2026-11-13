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
// format, or for an icon an SVG image, which the control plane keeps only as
// svgclean writes it anew (ADR 0083). package folder checks the file.
var ownMedia = regexp.MustCompile(`^media/[a-z0-9]+(?:[-_][a-z0-9]+)*\.(png|jpg|jpeg|webp|svg)$`)

// image says a path is one an entry's image may have: its own catalog's, or the
// panel's for an entry built into Dockwell. raster leaves out SVG and AVIF, as a
// screenshot is a picture of the app, never a drawing.
func image(path string, raster bool) bool {
	if raster && (strings.HasSuffix(path, ".svg") || strings.HasSuffix(path, ".avif")) {
		return false
	}
	return ownMedia.MatchString(path) || mediaPath.MatchString(path)
}

// What an entry may say at length, and in other languages (ADR 0083).
const (
	// MaxReadme is the most bytes an entry's README may be: the longest of 1Panel's
	// 468 was 6.8 KiB on 12 Nov 2026.
	MaxReadme = 32 << 10
	// MaxSummary is the most characters an entry's summary may be, in any language.
	MaxSummary = 240
	// MaxDescription is the most characters an entry's description may be.
	MaxDescription = 6000
	// maxTranslations is how many languages an entry may speak beside its own.
	maxTranslations = 8
)

// languageTag is a language an entry may speak, as a tag such as zh-CN or pt-BR.
var languageTag = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z][a-z]{3})?(-[A-Z]{2})?$`)

func checkPresentation(e *catalogv1.Entry) error {
	if utf8.RuneCountInString(e.GetDescription()) > MaxDescription {
		return fmt.Errorf("its description is longer than %d characters. Keep the app's overview within that length", MaxDescription)
	}
	if err := checkWords("", e.GetReadme(), e.GetSummary()); err != nil {
		return err
	}
	if len(e.GetTranslations()) > maxTranslations {
		return fmt.Errorf("it speaks over %d languages beside its own. Keep those the panel speaks", maxTranslations)
	}
	for tag, words := range e.GetTranslations() {
		switch {
		case !languageTag.MatchString(tag):
			return fmt.Errorf("it has words in %q, which isn't a language's tag such as zh-CN", tag)
		case words == nil:
			return fmt.Errorf("its words in %s are empty. Take them out", tag)
		case utf8.RuneCountInString(words.GetDescription()) > MaxDescription:
			return fmt.Errorf("its description in %s is longer than %d characters. Keep the app's overview within that length", tag, MaxDescription)
		}
		if err := checkWords(tag, words.GetReadme(), words.GetSummary()); err != nil {
			return err
		}
	}
	if icon := e.GetIcon(); icon != "" && !image(icon, false) {
		return errors.New("its icon needs an image in the folder media beside it, as media/<name>.webp, in WebP, PNG, JPEG or SVG")
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

// checkWords checks an entry's README and summary, in its own language or another:
// UTF-8, within their lengths, and the summary on one line.
func checkWords(tag, readme, summary string) error {
	in := ""
	if tag != "" {
		in = " in " + tag
	}
	switch {
	case len(readme) > MaxReadme:
		return fmt.Errorf("its README%s is over %d KiB. Keep it to the app's overview", in, MaxReadme>>10)
	case !utf8.ValidString(readme) || strings.ContainsRune(readme, 0):
		return fmt.Errorf("its README%s isn't UTF-8 text", in)
	case utf8.RuneCountInString(summary) > MaxSummary:
		return fmt.Errorf("its summary%s is longer than %d characters. Say what the app is for in one line", in, MaxSummary)
	case strings.ContainsAny(summary, "\r\n"):
		return fmt.Errorf("its summary%s is more than one line. Say what the app is for in one", in)
	}
	return nil
}
