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
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
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
	// Path is where the app's version is in the source, such as apps/memos/1.0.
	Path string
	// Compose is its Compose file, as the source wrote it.
	Compose []byte
	// Fields are what the source's form asks for, by the variable each fills.
	Fields []Field
	// Shipped says whether a path relative to the Compose file is a file or folder
	// the source ships beside it, as opposed to one the app makes as it runs.
	Shipped func(rel string) bool
	// LeftOut is why the adapter couldn't read the app, when it couldn't.
	LeftOut string
	// Older is the version before, where the source keeps one, which converts in
	// this one's place while this one is too young.
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
	// Web says this port is the one a person opens.
	Web bool
}

// Result is what came of one app.
type Result struct {
	ID   string
	Path string
	// Entry is the converted entry, nil when the app was left out.
	Entry *catalogv1.Entry
	// Date is when this release of the entry was made.
	Date string
	// LeftOut says why it wasn't converted.
	LeftOut string
	// Again says a later run may convert it with nothing changed, as when its
	// version is still young or a registry wasn't asked.
	Again bool
	// Young says it's left out as its version is under HoldBack old.
	Young bool
	// Notes are what the converter changed beyond the rules every entry follows.
	Notes []string
}

// Pinned is what the converter keeps of an image's tag between runs, so a version's
// age counts from the first build it saw, and a rebuild of that version, such as for
// its base image's updates, doesn't start it again.
type Pinned struct {
	Digest string    `json:"digest"`
	Built  time.Time `json:"built"`
}

// Converter converts apps from one source.
type Converter struct {
	Source   Source
	Registry Registry
	// Overlays are what a person wrote for each app, by ID: the summary and the
	// category in our own words, as ADR 0029 asks, and anything they've looked at,
	// such as the host access an app needs.
	Overlays map[string]*catalogv1.Entry
	// Previous are the entries the last run wrote, by ID, so an entry's release goes
	// up only when it changes.
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

// Convert converts one app, or says why it's left out. A version too young to take
// falls back to the one before, where the source keeps one.
func (c *Converter) Convert(ctx context.Context, app App) Result {
	r := c.convert(ctx, app)
	for at := app.Older; r.Young && at != nil; at = at.Older {
		older := c.convert(ctx, *at)
		if older.Entry != nil {
			older.Notes = append(older.Notes, "took "+at.Version+", as "+app.Version+" is under 14 days old")
			return older
		}
	}
	return r
}

func (c *Converter) convert(ctx context.Context, app App) Result {
	r := Result{ID: app.ID, Path: app.Path}
	if app.LeftOut != "" {
		r.LeftOut = app.LeftOut
		return r
	}
	if err := resource.CheckLabel("catalog entry", app.ID, "postgres"); err != nil {
		r.LeftOut = "its ID " + err.Error()
		return r
	}
	for _, own := range c.Own {
		if own.GetId() == app.ID {
			r.LeftOut = "Dockwell has its own entry of this ID"
			return r
		}
	}
	file, inputs, notes, err := rewrite(app)
	if err != nil {
		r.LeftOut = err.Error()
		return r
	}
	r.Notes = notes
	file, again, err := c.pin(ctx, file)
	if err != nil {
		var young youngError
		r.LeftOut, r.Again, r.Young = err.Error(), again, errors.As(err, &young)
		return r
	}
	if own := c.ownImage(file, inputs); own != "" {
		r.LeftOut = "it runs " + own + ", as Dockwell's own entry does. The reviewed list says whether they're one app"
		return r
	}
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
		Inputs:   inputs,
	}
	web := webService(file, inputs)
	others := slices.ContainsFunc(inputs, func(in *catalogv1.Input) bool {
		return in.GetType() == catalogv1.InputType_INPUT_TYPE_PORT && in.GetName() != "PORT"
	})
	if web.service != "" {
		e.Web = &catalogv1.Web{Service: web.service}
	}
	e.FirstSteps = firstSteps(app.Name, web.port, others)
	overlay := c.Overlays[app.ID]
	applyOverlay(e, overlay)
	// What only a person writes is checked last, so an app waiting for it is one
	// that converts once they have.
	var needs []string
	if e.GetSummary() == "" {
		needs, e.Summary = append(needs, "a summary in our own words, as a source's may be copied from elsewhere under other terms"), "stand-in"
	}
	if e.GetCategory() == "" {
		needs, e.Category = append(needs, "a category"), "stand-in"
	}
	if err := check(ctx, e, overlay); err != nil {
		r.LeftOut = err.Error()
		return r
	}
	if len(needs) > 0 {
		r.LeftOut = "it needs " + strings.Join(needs, ", and ") + ", in its overlay"
		return r
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

// pin pins every image by digest, and holds back a version younger than HoldBack.
func (c *Converter) pin(ctx context.Context, file []byte) ([]byte, bool, error) {
	doc, err := parseFile(file)
	if err != nil {
		return nil, false, err
	}
	for _, name := range serviceNames(doc) {
		svc := service(doc, name)
		img := mapGet(svc, "image")
		if img == nil {
			return nil, false, fmt.Errorf("its service %s names no image, and the converter doesn't build one", name)
		}
		image := img.Value
		if strings.Contains(image, "$") {
			return nil, false, fmt.Errorf("its service %s's image comes from a variable, %s, so no digest can pin it", name, image)
		}
		if strings.Contains(image, "@") {
			continue
		}
		r, err := parseRef(image)
		if err != nil {
			return nil, false, err
		}
		if unversioned[r.tag] {
			return nil, false, fmt.Errorf("its service %s's image is %s, which names no version", name, image)
		}
		digest, err := c.Registry.Pin(ctx, image)
		if err != nil {
			return nil, true, fmt.Errorf("pinning %s: %w", image, err)
		}
		kept, ok := c.Pins[image]
		if !ok || kept.Built.IsZero() {
			built, err := c.Registry.Built(ctx, image, digest)
			if err != nil {
				return nil, true, fmt.Errorf("%s's age isn't known: %w. It converts on a later run", image, err)
			}
			kept = Pinned{Built: built}
		}
		kept.Digest = digest
		if c.Pins == nil {
			c.Pins = map[string]Pinned{}
		}
		c.Pins[image] = kept
		if age := c.Now.Sub(kept.Built); age < HoldBack {
			return nil, true, youngError(fmt.Sprintf("%s is %d days old, and converts once it's %d", image, int(age.Hours()/24), int(HoldBack.Hours()/24)))
		}
		// Plain, as the catalog reads a pin by its line.
		img.Value, img.Style = image+"@"+digest, 0
	}
	out, err := encodeFile(doc)
	return out, false, err
}

// ownImage names the image of Dockwell's own entry that the app's web service runs,
// if one does, as two entries of one app would show twice.
func (c *Converter) ownImage(file []byte, inputs []*catalogv1.Input) string {
	web := webService(file, inputs)
	doc, err := parseFile(file)
	if err != nil || web.service == "" {
		return ""
	}
	image := repoOf(mapGet(service(doc, web.service), "image").Value)
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

// applyOverlay takes what a person wrote over what the converter made: each field
// they set, and each input's words by its name.
func applyOverlay(e, o *catalogv1.Entry) {
	for to, from := range map[*string]string{
		&e.Name: o.GetName(), &e.Summary: o.GetSummary(), &e.Category: o.GetCategory(), &e.Homepage: o.GetHomepage(),
		&e.AppLicense: o.GetAppLicense(), &e.FirstSteps: o.GetFirstSteps(), &e.HostAccess: o.GetHostAccess(),
	} {
		if from != "" {
			*to = from
		}
	}
	if o.GetWeb() != nil {
		e.Web = proto.CloneOf(o.GetWeb())
	}
	for _, in := range o.GetInputs() {
		i := slices.IndexFunc(e.GetInputs(), func(have *catalogv1.Input) bool { return have.GetName() == in.GetName() })
		if i < 0 {
			continue
		}
		have := e.GetInputs()[i]
		have.Label = cmp.Or(in.GetLabel(), have.GetLabel())
		have.Hint = cmp.Or(in.GetHint(), have.GetHint())
		have.DefaultValue = cmp.Or(in.GetDefaultValue(), have.GetDefaultValue())
		have.Optional = have.GetOptional() || in.GetOptional()
	}
}

// check reads an entry as the catalog and an install would, and refuses one that
// reaches its server unless a person has looked: its overlay says what it reaches.
func check(ctx context.Context, e, overlay *catalogv1.Entry) error {
	if err := catalog.Check(e); err != nil {
		return err
	}
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
	if err != nil {
		return fmt.Errorf("its Compose file, as an install reads it: %w", err)
	}
	var reaches []string
	for i := range stack.Services {
		s := &stack.Services[i]
		access := container.ServiceAccess(s.Spec.GetApp())
		for j := range access {
			reaches = append(reaches, s.Name+": "+words(&access[j]))
		}
		for _, p := range s.Spec.GetApp().GetPorts() {
			if addr := p.GetHostIp(); addr != "127.0.0.1" {
				return fmt.Errorf("its service %s publishes port %d on %q, where every entry publishes on loopback until it has a domain", s.Name, p.GetContainerPort(), addr)
			}
		}
	}
	if len(reaches) > 0 && overlay.GetHostAccess() == "" {
		return fmt.Errorf("it reaches its server (%s), so a person looks first. Once they have, say what it reaches in host_access in its overlay", strings.Join(reaches, "; "))
	}
	return nil
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

// words says what an access reaches, for the report.
func words(a *resource.Access) string {
	what := map[resource.AccessKind]string{
		resource.AccessPath:            "the server path",
		resource.AccessDocker:          "Docker's socket",
		resource.AccessNetwork:         "the server's own network",
		resource.AccessExternalNetwork: "a network Dockwell didn't make",
		resource.AccessCapability:      "a capability",
		resource.AccessPrivileged:      "every privilege",
		resource.AccessExternalVolume:  "a volume Dockwell didn't make",
	}[a.Kind]
	if what == "" {
		what = "something beyond its containers"
	}
	return strings.TrimSpace(what + " " + a.Path)
}

// firstSteps says how to reach an app, as every entry does: on loopback until it
// has a domain, and through an SSH tunnel for whoever makes its first account.
func firstSteps(name, port string, others bool) string {
	const more = " Its other ports answer only on this server too. Add a port on its page for one that something elsewhere must reach."
	if port == "" {
		return name + " answers only on this server. Add a port on its page for anything elsewhere that must reach it."
	}
	steps := fmt.Sprintf("%[1]s answers only on this server until you add a domain. If it makes an account for "+
		"whoever opens it first, make yours first, through an SSH tunnel from your computer: "+
		"ssh -N -L %[2]s:127.0.0.1:%[2]s you@your-server, using the port you gave it, then open "+
		"http://localhost:%[2]s. Add a domain on its page only after that, over HTTPS or behind your tunnel, "+
		"as a new domain's certificate shows in public logs within minutes.", name, port)
	if others {
		steps += more
	}
	return steps
}

// webPort is the service a person opens and its port's default.
type webPort struct{ service, port string }

// webService finds the service that publishes the port a person opens: PORT, or the
// first port the form asks for.
func webService(file []byte, inputs []*catalogv1.Input) webPort {
	doc, err := parseFile(file)
	if err != nil {
		return webPort{}
	}
	var web *catalogv1.Input
	for _, in := range inputs {
		if in.GetType() == catalogv1.InputType_INPUT_TYPE_PORT && (web == nil || in.GetName() == "PORT") {
			web = in
		}
	}
	if web == nil {
		return webPort{}
	}
	for _, name := range serviceNames(doc) {
		ports := mapGet(service(doc, name), "ports")
		if ports == nil {
			continue
		}
		for _, p := range ports.Content {
			if strings.Contains(nodeText(p), "${"+web.GetName()+"}") {
				return webPort{service: name, port: web.GetDefaultValue()}
			}
		}
	}
	return webPort{}
}

// Kept says which of the earlier run's entries stay, and which go: an entry left out
// for a reason a later run may not have stays as it was, and one left out for good
// goes, with its reason in the report.
func Kept(results []Result, previous map[string]*catalogv1.Entry) (stay, gone []string) {
	seen := map[string]bool{}
	for _, r := range results {
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
	for _, id := range slices.Sorted(maps.Keys(previous)) {
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	return stay, gone
}

// unversioned are tags that name a branch rather than a version, which moves.
var unversioned = map[string]bool{"latest": true, "stable": true, "main": true, "master": true, "nightly": true, "dev": true, "develop": true, "edge": true, "beta": true, "server": true}

// youngError says a version is too young to take yet.
type youngError string

func (e youngError) Error() string { return string(e) }

// ErrNoApps says a source had nothing to convert, as a wrong folder would.
var ErrNoApps = errors.New("found no apps")
