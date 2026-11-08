// SPDX-License-Identifier: AGPL-3.0-only

// Package convert turns another catalog's apps into entries in Dockwell's own
// format, as ADR 0029 sets out: once, when the catalog is built, never in the
// control plane. An adapter reads a source's apps; Convert rewrites each one's
// Compose file to what an entry may say, pins its images, fills in what a person
// wrote for it, and checks the result as an install would read it. An app it
// can't convert is left out, with the reason, so a person can see what's missing.
package convert

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	networkdriver "github.com/dockwell/dockwell/internal/resource/network"
	volumedriver "github.com/dockwell/dockwell/internal/resource/volume"
)

// HoldBack is how long a version is out before an entry takes it, as the repository
// holds back every other dependency.
const HoldBack = 14 * 24 * time.Hour

// Source is the catalog apps come from.
type Source struct {
	// Name is how its entries' header says it, such as "1Panel's app store".
	Name string
	// Repository is where it's published, such as https://github.com/1Panel-dev/appstore.
	Repository string
	// Commit is the commit it's read at.
	Commit string
	// License is the SPDX licence of what it publishes, which a converted entry keeps.
	License string
	// LicenseFile is where its licence's text is in a checkout, such as LICENSE.
	LicenseFile string
	// LicenseText is that text, which goes beside what's converted from it, as each
	// of the licences the converter reads asks.
	LicenseText []byte
}

// App is one app as an adapter reads it, before it's an entry.
type App struct {
	ID       string
	Name     string
	Version  string
	Homepage string
	// Category is the adapter's guess from the source's tags, which a person's
	// overlay can change. Empty when it has none.
	Category string
	// Web says a person opens it in a browser, as the source says, so its first
	// steps tell them how, and a domain fits it.
	Web bool
	// Path is where the app's version is in the source, such as apps/memos/1.0.
	Path string
	// Compose is its Compose file, as the source wrote it.
	Compose []byte
	// Fields are what the source's form asks for, by the variable each fills.
	Fields []Field
	// Shipped says whether a path relative to the Compose file is a file or folder
	// the source ships beside it, as opposed to one the app makes as it runs.
	Shipped func(rel string) bool
	// LeftOut is why the adapter couldn't read the app, when it couldn't, and Class
	// what the report counts it as.
	LeftOut string
	Class   Class
	// Older is the version before, where the source keeps one, which converts in
	// this one's place while this one is too young, within its major version.
	Older *App
}

// Kind is what a field asks for.
type Kind int

const (
	// Text is a value a person may type, with its default.
	Text Kind = iota
	// Port is a port on the server.
	Port
	// Secret is a password a person types.
	Secret
	// Generated is a password the source makes up, which an entry generates and seals.
	Generated
	// Email is an address a person types.
	Email
)

// Field is one value an app's form asks for.
type Field struct {
	// Name is the variable it fills in the Compose file.
	Name     string
	Label    string
	Kind     Kind
	Default  string
	Optional bool
	// Choices are the values a source's select offers, in its order.
	Choices []string
	// Web says this port is the one a person opens in a browser.
	Web bool
	// Length is how much a Generated field makes, 32 unless set, and As how it's
	// written, for an app that decodes it as a key.
	Length int
	As     catalogv1.GenerateAs
}

// Result is what came of one app.
type Result struct {
	ID   string
	Path string
	// Entry is the converted entry, nil when the app was left out.
	Entry *catalogv1.Entry
	// Date is when this release of the entry was made.
	Date string
	// LeftOut says why it wasn't converted, and Class what the report counts it as.
	LeftOut string
	Class   Class
	// Held says why an earlier release stays in place of this run's, when it does:
	// Entry is that release.
	Held string
	// Again says a later run may convert it with nothing changed, as when its
	// version is still young or a registry wasn't asked.
	Again bool
	// Young says it's left out as its version is under HoldBack old.
	Young bool
	// Notes are what the converter changed beyond the rules every entry follows.
	Notes []string
}

// Pinned is what the converter keeps of an image's tag between runs. Entries pin
// Digest, built at Built. A tag pushed again names a new build, Next, which entries
// move to once it's HoldBack old too, as a tag that moves under a version can carry
// anything.
type Pinned struct {
	Digest    string    `json:"digest"`
	Built     time.Time `json:"built"`
	Next      string    `json:"next,omitempty"`
	NextBuilt time.Time `json:"nextBuilt,omitzero"`
}

// Converter converts apps from one source.
type Converter struct {
	Source   Source
	Registry Registry
	// Overlays are what a person wrote for each app, by ID: the summary and the
	// category in our own words, as ADR 0029 asks, and what they've looked at, such
	// as what the app reaches on its server.
	Overlays map[string]Overlay
	// Previous are the entries the last run wrote, by ID, so an entry's release goes
	// up only when it changes, and never back or past a major version.
	Previous map[string]*catalogv1.Entry
	// PreviousDates are the dates of those entries' releases.
	PreviousDates map[string]string
	// Pins are what the converter kept of each image's tag, which Convert updates.
	Pins map[string]Pinned
	// Own are Dockwell's own entries, which a converted app never takes over from:
	// by ID, and by the image a person opens.
	Own []*catalogv1.Entry
	// Now is when the run is.
	Now time.Time
}

// Convert converts one app, or says why it's left out. While its newest version is
// under HoldBack old, an entry an earlier run wrote stays as it is, and with none, the
// version before converts, within the newest's major. An entry never moves back, or
// across a major version, until the format can mark a release that old data won't
// survive (#248).
func (c *Converter) Convert(ctx context.Context, app App) Result {
	was := c.Previous[app.ID]
	r := c.convert(ctx, app)
	switch {
	case r.Entry != nil && was != nil:
		if held, class := hold(was, r.Entry); held != "" {
			return c.keep(app.ID, was, held, class)
		}
		return r
	case r.Entry != nil || !r.Young:
		return r
	case was != nil:
		return c.keep(app.ID, was, r.LeftOut, ClassYoung)
	}
	major := majorOf(app.Version)
	for at := app.Older; at != nil; at = at.Older {
		if major == "" || majorOf(at.Version) != major {
			continue
		}
		older := c.convert(ctx, *at)
		if older.Entry != nil {
			older.Notes = append(older.Notes, "took "+at.Version+", as "+app.Version+" is under 14 days old")
			return older
		}
	}
	return r
}

// hold says why an earlier entry stays in place of a new one: it would go back, or
// move to a new major version, which old data may not survive.
func hold(was, now *catalogv1.Entry) (string, Class) {
	switch {
	case compareVersions(now.GetVersion(), was.GetVersion()) < 0:
		return fmt.Sprintf("it would go back from %s to %s, which no upgrade may", was.GetVersion(), now.GetVersion()), ClassBack
	case majorOf(now.GetVersion()) != majorOf(was.GetVersion()):
		return fmt.Sprintf("it moves from %s to %s, a new major version, which old data may not survive. It waits for the format to mark such a release (#248)", was.GetVersion(), now.GetVersion()), ClassMajor
	}
	return "", ""
}

// keep keeps an earlier run's entry, as it was.
func (c *Converter) keep(id string, was *catalogv1.Entry, why string, class Class) Result {
	return Result{ID: id, Path: was.GetSource().GetPath(), Entry: was, Date: c.PreviousDates[id], Held: why, Class: class}
}

func (c *Converter) convert(ctx context.Context, app App) Result {
	r := Result{ID: app.ID, Path: app.Path}
	fail := func(err error, fallback Class) Result {
		r.LeftOut, r.Class = err.Error(), classOf(err, fallback)
		return r
	}
	if app.LeftOut != "" {
		r.LeftOut, r.Class = app.LeftOut, cmp.Or(app.Class, ClassCantSay)
		return r
	}
	if err := resource.CheckLabel("catalog entry", app.ID, "postgres"); err != nil {
		return fail(leave(ClassCantSay, "its ID %s", err), "")
	}
	for _, own := range c.Own {
		if own.GetId() == app.ID {
			return fail(leave(ClassOwn, "Dockwell has its own entry of this ID, so this one stays out, as one would take over from the other. "+
				"If they're different apps, the reviewed list gives this one an ID of its own"), "")
		}
	}
	rw, err := rewrite(app)
	if err != nil {
		return fail(err, ClassCantSay)
	}
	r.Notes = rw.notes
	file, notes, err := c.pin(ctx, rw.file)
	if err != nil {
		var young youngError
		r.Young = errors.As(err, &young)
		r.Again = r.Young || forNow(err)
		if r.Young {
			return fail(err, ClassYoung)
		}
		return fail(err, ClassPin)
	}
	r.Notes = append(r.Notes, notes...)
	e := &catalogv1.Entry{
		Id:       app.ID,
		Name:     app.Name,
		Category: app.Category,
		Version:  app.Version,
		Release:  1,
		Homepage: app.Homepage,
		License:  c.Source.License,
		Source:   &catalogv1.Source{Repository: c.Source.Repository, Path: app.Path, Commit: c.Source.Commit},
		Compose:  string(file),
		Inputs:   rw.inputs,
	}
	if svc := serviceOf(file, rw.web); svc != "" {
		e.Web = &catalogv1.Web{Service: svc}
	}
	if own := c.ownImage(e); own != "" {
		return fail(leave(ClassOwn, "it runs %s, as Dockwell's own entry does, so it stays out until the reviewed list says whether they're one app", own), "")
	}
	overlay := c.Overlays[app.ID]
	if overlay.Entry == nil {
		overlay.Entry = &catalogv1.Entry{}
	}
	if err := applyOverlay(e, overlay); err != nil {
		return fail(err, ClassOverlay)
	}
	if overlay.Entry.GetFirstSteps() == "" {
		e.FirstSteps = firstSteps(e, app.Name)
	}
	// What only a person writes is checked last, so an app waiting for it is one
	// that converts once they have.
	var needs []string
	if e.GetSummary() == "" {
		needs, e.Summary = append(needs, "a summary in our own words, as a source's may be copied from elsewhere under other terms"), "stand-in"
	}
	if e.GetCategory() == "" {
		needs, e.Category = append(needs, "a category"), "stand-in"
	}
	if err := check(ctx, e, overlay, rw.reaches, rw.typed); err != nil {
		return fail(err, ClassCheck)
	}
	if len(needs) > 0 {
		return fail(leave(ClassWords, "it needs %s, in its overlay", strings.Join(needs, ", and ")), "")
	}
	r.Entry, r.Date = e, c.Now.UTC().Format(time.DateOnly)
	if was := c.Previous[app.ID]; was != nil {
		if same(was, e) {
			r.Entry, r.Date = was, cmp.Or(c.PreviousDates[app.ID], r.Date)
		} else {
			e.Release = was.GetRelease() + 1
		}
	}
	return r
}

// same says two entries differ in nothing but their release and the commit they were
// read at, so a run over a newer commit that didn't touch an app writes it again as
// it was.
func same(a, b *catalogv1.Entry) bool {
	a, b = proto.CloneOf(a), proto.CloneOf(b)
	for _, e := range []*catalogv1.Entry{a, b} {
		e.Release = 0
		if e.GetSource() != nil {
			e.Source.Commit = ""
		}
	}
	return proto.Equal(a, b)
}

// The platforms every entry runs on: the owner's servers are amd64 and arm64.
var platforms = []string{"linux/amd64", "linux/arm64"}

// pin pins every image by digest, and holds back a build younger than HoldBack: a new
// version, or a tag pushed again, whose new build waits its own 14 days while entries
// keep the one before. A build that says no believable date counts from when the
// converter first saw it.
func (c *Converter) pin(ctx context.Context, file []byte) ([]byte, []string, error) {
	doc, err := parseFile(file)
	if err != nil {
		return nil, nil, err
	}
	var notes []string
	for _, name := range serviceNames(doc) {
		img := mapGet(service(doc, name), "image")
		if img == nil {
			return nil, nil, leave(ClassCantSay, "its service %s names no image, and the converter doesn't build one", name)
		}
		image := img.Value
		if strings.Contains(image, "$") {
			return nil, nil, leave(ClassCantSay, "its service %s's image comes from a variable, %s, so no digest can pin it", name, image)
		}
		r, err := parseRef(image)
		if err != nil {
			return nil, nil, leave(ClassNoVersion, "%s", err)
		}
		if r.digest == "" && unversioned[r.tag] {
			return nil, nil, leave(ClassNoVersion, "its service %s's image is %s, which names no version", name, image)
		}
		current, err := c.Registry.Pin(ctx, image)
		if err != nil {
			return nil, nil, leaveForNow(ClassPin, "pinning %s: %v. It converts on a later run", image, err)
		}
		kept, had := c.Pins[image]
		switch {
		case had && kept.Digest == current:
		case had && kept.Next == current:
		default:
			built, note, err := c.built(ctx, image, current)
			if err != nil {
				return nil, nil, err
			}
			if note != "" {
				notes = append(notes, note)
			}
			if had {
				kept.Next, kept.NextBuilt = current, built
			} else {
				kept = Pinned{Digest: current, Built: built}
			}
		}
		if kept.Next != "" && c.Now.Sub(kept.NextBuilt) >= HoldBack {
			kept = Pinned{Digest: kept.Next, Built: kept.NextBuilt}
		}
		if c.Pins == nil {
			c.Pins = map[string]Pinned{}
		}
		c.Pins[image] = kept
		if age := c.Now.Sub(kept.Built); age < HoldBack {
			return nil, nil, youngError(fmt.Sprintf("%s is %s old, and converts once it's %d days old", image, days(age), int(HoldBack.Hours()/24)))
		}
		if kept.Next != "" {
			notes = append(notes, fmt.Sprintf("pins %s's build of %s, as the tag's build of %s waits until %s",
				image, kept.Built.Format(time.DateOnly), kept.NextBuilt.Format(time.DateOnly), kept.NextBuilt.Add(HoldBack).Format(time.DateOnly)))
		}
		has, err := c.Registry.Platforms(ctx, image, kept.Digest)
		if err != nil {
			return nil, nil, leaveForNow(ClassAgeUnknown, "the platforms %s runs on aren't known: %v. It converts on a later run", image, err)
		}
		if missing := slices.DeleteFunc(slices.Clone(platforms), func(p string) bool { return slices.Contains(has, p) }); len(missing) > 0 {
			return nil, nil, leave(ClassArch, "%s runs on %s, and not %s, where an entry runs on amd64 and arm64 both", image, strings.Join(has, " and "), strings.Join(missing, " or "))
		}
		// Plain, and with no comment after it, as the catalog reads a pin by its line.
		if r.digest == "" {
			img.Value = image + "@" + kept.Digest
		}
		img.Style, img.LineComment = 0, ""
	}
	out, err := encodeFile(doc)
	return out, notes, err
}

// built says when a build was made, as its registry says. A date before 2013, when
// Docker began, or after now, is no date at all, as reproducible builds write the
// epoch, so such a build counts from when the converter first saw it: now.
func (c *Converter) built(ctx context.Context, image, digest string) (time.Time, string, error) {
	t, err := c.Registry.Built(ctx, image, digest)
	if err != nil {
		return time.Time{}, "", leaveForNow(ClassAgeUnknown, "%s's age isn't known: %v. It converts on a later run", image, err)
	}
	if t.Year() < 2013 || t.After(c.Now) {
		return c.Now, fmt.Sprintf("counts %s's age from %s, when the converter first saw it, as its build says %s", image, c.Now.Format(time.DateOnly), t.Format(time.DateOnly)), nil
	}
	return t, "", nil
}

// serviceOf finds the service that publishes a port, by the input it's named by.
func serviceOf(file []byte, port string) string {
	doc, err := parseFile(file)
	if err != nil || port == "" {
		return ""
	}
	for _, name := range serviceNames(doc) {
		if ports := mapGet(service(doc, name), "ports"); ports != nil && strings.Contains(nodeText(ports), "${"+port+"}") {
			return name
		}
	}
	return ""
}

// webPort is the default of the port a person opens a service on: the first port
// input the service publishes.
func webPort(e *catalogv1.Entry) string {
	doc, err := parseFile([]byte(e.GetCompose()))
	if err != nil || e.GetWeb().GetService() == "" {
		return ""
	}
	ports := mapGet(service(doc, e.GetWeb().GetService()), "ports")
	if ports == nil {
		return ""
	}
	text := nodeText(ports)
	for _, in := range e.GetInputs() {
		if in.GetType() == catalogv1.InputType_INPUT_TYPE_PORT && strings.Contains(text, "${"+in.GetName()+"}") {
			return in.GetDefaultValue()
		}
	}
	return ""
}

// ownImage names the image of Dockwell's own entry that the app's web service runs,
// if one does, as two entries of one app would show twice.
func (c *Converter) ownImage(e *catalogv1.Entry) string {
	doc, err := parseFile([]byte(e.GetCompose()))
	if err != nil || e.GetWeb().GetService() == "" {
		return ""
	}
	image := repoOf(mapGet(service(doc, e.GetWeb().GetService()), "image").Value)
	for _, own := range c.Own {
		ownDoc, err := parseFile([]byte(own.GetCompose()))
		if err != nil {
			continue
		}
		if svc := service(ownDoc, own.GetWeb().GetService()); svc != nil {
			if img := mapGet(svc, "image"); img != nil && repoOf(img.Value) == image {
				return image
			}
		}
	}
	return ""
}

// repoOf is an image's repository, as Docker would pull it, without tag or digest.
func repoOf(image string) string {
	r, err := parseRef(image)
	if err != nil {
		return image
	}
	return r.host + "/" + r.name
}

// check reads an entry as the catalog and an install would, and refuses one that
// reaches its server unless a person has looked: its overlay says what it reaches in
// words, and lists exactly what the converter finds it reaches now. That's what its
// services reach, what its volumes and networks reach, as the plan counts them, and
// what a value a person types can reach.
func check(ctx context.Context, e *catalogv1.Entry, overlay Overlay, typed []string, typedResources map[string]bool) error {
	if err := catalog.Check(e); err != nil {
		return err
	}
	unlooked := overlay.Entry.GetHostAccess() == "" || len(overlay.Reaches) == 0
	given := map[string]string{}
	for _, in := range e.GetInputs() {
		if catalog.Asks(in) && in.GetDefaultValue() == "" && !in.GetOptional() {
			given[in.GetName()] = standIn(in)
		}
	}
	filled, err := catalog.Fill(e, given, catalog.Suggest(e, nil))
	if err != nil {
		return fmt.Errorf("filling in its form: %w", err)
	}
	stack, err := compose.Read(ctx, compose.Input{File: []byte(e.GetCompose()), Variables: filled.Variables})
	switch {
	case err != nil && len(typed) > 0 && unlooked:
		// What a value a person types reaches needs no reading of the file to say, and
		// a person looks at it first, whatever the file's stand-ins read as.
		return leave(ClassReaches, "it reaches its server, so a person looks first. Once they have, write what it reaches in host_access in its overlay, and list under reaches what they looked at: %s", quoted(typed))
	case err != nil:
		return leave(ClassCantRun, "Dockwell can't run what its Compose file asks yet: %s", forCatalog(err.Error()))
	}
	reaches := slices.Clone(typed)
	for i := range stack.Services {
		s := &stack.Services[i]
		access := container.ServiceAccess(s.Spec.GetApp())
		for j := range access {
			reaches = append(reaches, s.Name+": "+words(&access[j]))
		}
		for _, p := range s.Spec.GetApp().GetPorts() {
			if addr := p.GetHostIp(); addr != "127.0.0.1" {
				return leave(ClassCantSay, "its service %s publishes port %d on %q, where every entry publishes on loopback until it has a domain", s.Name, p.GetContainerPort(), addr)
			}
		}
	}
	for i := range stack.Volumes {
		v := &stack.Volumes[i]
		if typedResources["volume "+v.Name] {
			// What a person types, which typed says, and not what a stand-in reads as.
			continue
		}
		access := (&volumedriver.Driver{}).HostAccess(v.Spec)
		for j := range access {
			reaches = append(reaches, "volume "+v.Name+": "+words(&access[j]))
		}
	}
	for i := range stack.Networks {
		n := &stack.Networks[i]
		if typedResources["network "+n.Name] {
			continue
		}
		access := (&networkdriver.Driver{}).HostAccess(n.Spec)
		for j := range access {
			reaches = append(reaches, "network "+n.Name+": "+words(&access[j]))
		}
	}
	slices.Sort(reaches)
	reaches = slices.Compact(reaches)
	looked := slices.Sorted(slices.Values(overlay.Reaches))
	switch {
	case len(reaches) == 0 && len(looked) == 0:
		return nil
	case len(reaches) > 0 && unlooked:
		return leave(ClassReaches, "it reaches its server, so a person looks first. Once they have, write what it reaches in host_access in its overlay, and list under reaches what they looked at: %s", quoted(reaches))
	case !slices.Equal(reaches, looked):
		added := slices.DeleteFunc(slices.Clone(reaches), func(r string) bool { return slices.Contains(looked, r) })
		gone := slices.DeleteFunc(slices.Clone(looked), func(r string) bool { return slices.Contains(reaches, r) })
		var diff []string
		if len(added) > 0 {
			diff = append(diff, "it now reaches "+quoted(added))
		}
		if len(gone) > 0 {
			diff = append(diff, "it no longer reaches "+quoted(gone))
		}
		return leave(ClassReaches, "%s, where its overlay lists what a person looked at. Once they've looked again, write host_access and reaches as it is now", strings.Join(diff, ", and "))
	}
	return nil
}

func quoted(list []string) string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(out, ", ")
}

// standIn is a value for a required input with no default, for reading the file.
func standIn(in *catalogv1.Input) string {
	switch in.GetType() {
	case catalogv1.InputType_INPUT_TYPE_EMAIL:
		return "someone@example.com"
	case catalogv1.InputType_INPUT_TYPE_PORT:
		return "8000"
	case catalogv1.InputType_INPUT_TYPE_SECRET:
		return "stand-in-for-a-secret-of-thirty-two"
	default:
	}
	return "stand-in"
}

// words says what an access reaches, for the report and an overlay's reaches.
func words(a *resource.Access) string {
	var what string
	switch a.Kind {
	case resource.AccessPath:
		what = "the server path " + a.Path
	case resource.AccessDocker:
		// Docker's socket, or a folder that holds it, such as /.
		what = "control of Docker, through " + a.Path
	case resource.AccessNetwork:
		what = "the server's own network"
	case resource.AccessExternalNetwork:
		what = "a network Dockwell didn't make, " + a.Path
	case resource.AccessCapability:
		what = "the capability " + a.Path
	case resource.AccessPrivileged:
		what = "every privilege"
	case resource.AccessExternalVolume:
		what = "a volume Dockwell didn't make, " + a.Path
	case resource.AccessMount:
		what = strings.TrimSpace("a mount " + strings.Join(slices.DeleteFunc([]string{a.Type, a.Driver, a.Address, a.Path}, func(s string) bool { return s == "" }), " "))
	case resource.AccessNetworkDriver:
		what = strings.TrimSpace("the " + a.Driver + " network driver " + strings.TrimSpace(a.Path+" "+a.Detail))
	default:
		what = strings.TrimSpace(fmt.Sprintf("what Dockwell counts as %v, %s", a.Kind, a.Path))
	}
	if a.ReadOnly {
		what += ", read-only"
	}
	return what
}

// firstSteps says how to reach an app, as every entry does: on loopback until it has
// a domain, and, for one a person opens in a browser, through an SSH tunnel for
// whoever makes its first account.
func firstSteps(e *catalogv1.Entry, name string) string {
	others := 0
	for _, in := range e.GetInputs() {
		if in.GetType() == catalogv1.InputType_INPUT_TYPE_PORT {
			others++
		}
	}
	port := webPort(e)
	if port == "" {
		return name + " answers only on this server. Add a port on its page for anything elsewhere that must reach it."
	}
	steps := fmt.Sprintf("%[1]s answers only on this server until you add a domain. If it makes an account for "+
		"whoever opens it first, make yours first, through an SSH tunnel from your computer: "+
		"ssh -N -L %[2]s:127.0.0.1:%[2]s you@your-server, using the port you gave it, then open "+
		"http://localhost:%[2]s. Add a domain on its page only after that, over HTTPS or behind your tunnel, "+
		"as a new domain's certificate shows in public logs within minutes.", name, port)
	if others > 1 {
		steps += " Its other ports answer only on this server too. Add a port on its page for one that something elsewhere must reach."
	}
	return steps
}

// Kept says which of the earlier run's entries stay, and which go: an entry left out
// for a reason a later run may not have stays as it was, and one left out for good
// goes, with its reason in the report. Only the IDs read count: an entry of one this
// run didn't read stays.
func Kept(results []Result, previous map[string]*catalogv1.Entry, all bool) (stay, gone []string) {
	seen := map[string]bool{}
	for i := range results {
		r := &results[i]
		seen[r.ID] = true
		if r.Entry != nil || previous[r.ID] == nil {
			continue
		}
		if r.Again {
			stay = append(stay, r.ID)
		} else {
			gone = append(gone, r.ID)
		}
	}
	if all {
		for _, id := range slices.Sorted(maps.Keys(previous)) {
			if !seen[id] {
				gone = append(gone, id)
			}
		}
	}
	return stay, gone
}

// unversioned are tags that name a branch rather than a version, which moves.
var unversioned = map[string]bool{"latest": true, "stable": true, "main": true, "master": true, "nightly": true, "dev": true, "develop": true, "edge": true, "beta": true, "server": true}

// days says an age in whole days, as people say one.
func days(age time.Duration) string {
	if n := int(age.Hours() / 24); n != 1 {
		return strconv.Itoa(n) + " days"
	}
	return "1 day"
}

// youngError says a version is too young to take yet.
type youngError string

func (e youngError) Error() string { return string(e) }

// ErrNoApps says a source had nothing to convert, as a wrong folder would.
var ErrNoApps = errors.New("found no apps")
