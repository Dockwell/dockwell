// SPDX-License-Identifier: AGPL-3.0-only

// Package folder checks a catalog's folder of entries as the control plane reads a
// catalog source: each <id>.yaml as catalog.Parse reads it, in a file named by its
// ID, with its Compose file read as an install would read it, and each image an
// entry names in the folder media beside it, by what its bytes say it is. The
// control plane and dockwell-catalog check run the same checks, so a catalog's CI
// refuses what a server would leave out (docs/adr/0055-catalogs-from-a-git-url.md,
// docs/adr/0077-dockwells-catalog-in-its-own-repository.md).
package folder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/gitfetch"
	"github.com/dockwell/dockwell/internal/resource"
)

// Media is the folder of a catalog's images, in its folder of entries. An entry
// names a file in it as media/<name>.
const Media = "media"

// Limits are what a source's commit is read within: its entries, as ADR 0055 has
// them, and its images, a folder of their own beside them. An image is a few
// dozen KiB as Dockwell's own are, so a thousand of them come to a few MiB.
var Limits = func() gitfetch.Limits {
	lim := gitfetch.Default
	lim.Sub = &gitfetch.Sub{Name: Media, Limits: MediaLimits, Want: ImageName}
	return lim
}()

// MediaLimits bound a commit's images: how many, how large each, and all of them.
var MediaLimits = gitfetch.Limits{Files: 1000, File: 1 << 20, Total: 32 << 20}

// imageName is what an image's file may be called: lowercase words, then the
// extension of a raster format the panel shows. No SVG, which can hold a script.
var imageName = regexp.MustCompile(`^[a-z0-9]+(?:[-_][a-z0-9]+)*\.(png|jpg|jpeg|webp)$`)

// ImageName says a name is one an image in the folder media may have.
func ImageName(name string) bool { return imageName.MatchString(name) }

// MediaFile is the image an entry's icon or screenshot names in its folder, as
// media/ghost-icon.webp names ghost-icon.webp, and whether it names one so.
func MediaFile(path string) (string, bool) {
	name, ok := strings.CutPrefix(path, Media+"/")
	return name, ok && ImageName(name)
}

// Sniffed is how many of an image's first bytes Image needs.
const Sniffed = 12

// Image says what an image is by its first bytes, as its content type: WebP, PNG or
// JPEG, and that its name's extension says the same. Anything else is refused, as
// a stranger's file could be a page a browser runs, whatever it's called.
func Image(name string, head []byte) (string, error) {
	var kind, ext string
	switch {
	case len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		kind, ext = "image/webp", "webp"
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		kind, ext = "image/png", "png"
	case bytes.HasPrefix(head, []byte{0xff, 0xd8, 0xff}):
		kind, ext = "image/jpeg", "jpeg"
	default:
		return "", errors.New("by its first bytes it isn't a WebP, PNG or JPEG image, the images a catalog may hold. Save it as one")
	}
	got := name[strings.LastIndexByte(name, '.')+1:]
	if got == "jpg" {
		got = "jpeg"
	}
	if got != ext {
		return "", fmt.Errorf("by its first bytes it's a %s image, named as a %s one. Name it .%s", strings.ToUpper(ext), strings.ToUpper(got), ext)
	}
	return kind, nil
}

// Refusal is a file the catalog leaves out, or an image an entry loses, and why.
type Refusal struct {
	// File is its name in the folder, such as "notes.yaml" or "media/logo.png".
	File string
	// Reason says why, in a sentence that ends with what to do.
	Reason string
	// Kind is what kind of refusal it is: KindCheck, KindTaken or KindImage.
	Kind string
}

// What kind of refusal a Refusal is.
const (
	// KindCheck is an entry that fails the catalog's checks.
	KindCheck = "check"
	// KindTaken is an entry whose ID a catalog before it has.
	KindTaken = "taken"
	// KindImage is an image an entry names that it can't show.
	KindImage = "image"
)

// Result is what came of checking a folder.
type Result struct {
	// Entries are those it keeps, in their files' order, with the images they name
	// as written: media/<name>, each one the folder has.
	Entries []*catalogv1.Entry
	// Also are entries of apps a catalog before this one has, which Options.Same
	// says are the same app: another choice of each, not a clash (ADR 0078).
	Also []*catalogv1.Entry
	// Refused is each file it leaves out, and each image an entry names that it
	// can't show, with why.
	Refused []Refusal
}

// Taken is what a catalog before this one has of an ID: whose it is, as a reason
// says it, such as "the catalog community", and its entry.
type Taken struct {
	By    string
	Entry *catalogv1.Entry
}

// Options are how Check reads a folder.
type Options struct {
	// Tagged takes an image named by a tag its file writes plainly, as an entry
	// converted on the control plane from another store's names one until an
	// install pins it (ADR 0078).
	Tagged bool
	// Same says an entry of an ID a catalog before has is the same app, which Check
	// keeps in Also, once it reads as an install would, rather than leaving it out.
	// Nil leaves every one out.
	Same func(before, e *catalogv1.Entry) bool
	// Proved reads no entry's Compose file again, for files this whole check passed
	// as they were built: the catalog a build holds, whose tests check it so. Each
	// entry is still parsed, and each image it names still has to be in media.
	Proved bool
}

// Check checks each <id>.yaml in files, by name, as Dockwell's own entries are
// checked, against the images in media that passed Image, by name. An entry whose
// ID taken has, by whose it is, is left out, saying so, unless opts.Same says it's
// the same app, which goes in Also. One it keeps goes in taken as the catalog
// name's. A file that isn't an entry, such as a README, is no
// entry to leave out. An entry keeps its place without an image it names that media
// hasn't, or one of the artwork Dockwell ships, which a source's entry can't wear.
func Check(ctx context.Context, name string, files map[string][]byte, media map[string]bool, taken map[string]Taken, opts Options) Result {
	var out Result
	refuse := func(file, why, kind string) {
		out.Refused = append(out.Refused, Refusal{File: file, Reason: why, Kind: kind})
	}
	parse := catalog.Parse
	if opts.Tagged {
		parse = catalog.ParseTagged
	}
	for _, file := range slices.Sorted(maps.Keys(files)) {
		id, ok := strings.CutSuffix(file, ".yaml")
		if !ok || resource.CheckLabel("catalog entry", id, "postgres") != nil {
			continue
		}
		e, err := parse(files[file])
		switch {
		case err != nil:
			refuse(file, err.Error(), KindCheck)
			continue
		case e.GetId() != id:
			refuse(file, fmt.Sprintf("it's the entry %s, in a file named for %s. Name the file %s.yaml", e.GetId(), id, e.GetId()), KindCheck)
			continue
		case taken[e.GetId()].By != "" && (opts.Same == nil || !opts.Same(taken[e.GetId()].Entry, e)):
			refuse(file, fmt.Sprintf("%s has an entry of the ID %s, which comes first. Give it another ID", taken[e.GetId()].By, e.GetId()), KindTaken)
			continue
		}
		if !opts.Proved {
			if err := ReadsAsInstall(ctx, e, opts.Tagged); err != nil {
				refuse(file, err.Error(), KindCheck)
				continue
			}
		}
		// The same app a catalog before this one lists: another choice of it.
		also := taken[e.GetId()].By != ""
		// An image it names that the folder hasn't, or one of Dockwell's own, goes.
		shows := func(path string) bool {
			if image, ok := MediaFile(path); ok {
				if !media[image] {
					refuse(path, fmt.Sprintf("%s names it, and the folder %s has no such image, or none that passed its checks. Add it, or take it out of %s", file, Media, file), KindImage)
				}
				return media[image]
			}
			refuse(path, fmt.Sprintf("%s names artwork Dockwell ships for its own apps, which another catalog's entry can't show. Put the app's own image in the folder %s, and name it as %s/<name>", file, Media, Media), KindImage)
			return false
		}
		if e.GetIcon() != "" && !shows(e.GetIcon()) {
			e.Icon = ""
		}
		e.Screenshots = slices.DeleteFunc(e.Screenshots, func(s *catalogv1.Screenshot) bool { return !shows(s.GetPath()) })
		if also {
			out.Also = append(out.Also, e)
			continue
		}
		out.Entries = append(out.Entries, e)
		taken[e.GetId()] = Taken{By: "the catalog " + name, Entry: e}
	}
	return out
}

// ReadsAsInstall reads an entry's Compose file as an install would, with stand-ins
// for what a person types, and refuses an image that isn't pinned by its digest
// however the file writes it: a flow mapping or a quoted key gets past a check of
// lines (the first review of #332, its M3). tagged takes an image named by a tag its
// file writes plainly, as Check's does.
func ReadsAsInstall(ctx context.Context, e *catalogv1.Entry, tagged bool) error {
	stack, err := ReadStack(ctx, e)
	if err != nil {
		return err
	}
	plain := catalog.TaggedImages(e.GetCompose())
	for _, s := range stack.Services {
		image := s.Spec.GetApp().GetImage()
		if tagged && slices.Contains(plain, image) {
			continue
		}
		if !pinnedImage.MatchString(image) {
			return fmt.Errorf("its service %s runs %s, which isn't pinned by its digest, as image@sha256:<digest>, so what it runs could change under it. Pin it", s.Name, image)
		}
	}
	return nil
}

// ReadStack reads an entry's Compose file as an install would, with stand-ins for
// what a person types.
func ReadStack(ctx context.Context, e *catalogv1.Entry) (*compose.Stack, error) {
	given := map[string]string{}
	for _, in := range e.GetInputs() {
		if catalog.Asks(in) && in.GetDefaultValue() == "" && !in.GetOptional() {
			given[in.GetName()] = StandIn(in)
		}
	}
	filled, err := catalog.Fill(e, given, catalog.Suggest(e, nil))
	if err != nil {
		return nil, fmt.Errorf("its form can't be filled in: %w", err)
	}
	stack, err := compose.Read(ctx, compose.Input{File: []byte(e.GetCompose()), Variables: filled.Variables})
	if err != nil {
		return nil, fmt.Errorf("its Compose file, as an install reads it: %w", err)
	}
	return stack, nil
}

// pinnedImage is an image named by its digest.
var pinnedImage = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// StandIn is a value for an input that has none yet, of the kind it takes. Text reads
// as a path, as a mount's source may be one, which Compose would read as a volume.
func StandIn(in *catalogv1.Input) string {
	switch in.GetType() {
	case catalogv1.InputType_INPUT_TYPE_EMAIL:
		return "you@example.com"
	case catalogv1.InputType_INPUT_TYPE_PORT:
		return "1024"
	case catalogv1.InputType_INPUT_TYPE_SECRET:
		return "stand-in"
	case catalogv1.InputType_INPUT_TYPE_UNSPECIFIED, catalogv1.InputType_INPUT_TYPE_TEXT:
	}
	return "/srv/stand-in"
}
