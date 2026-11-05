// SPDX-License-Identifier: AGPL-3.0-only

// Package volume is the "volume" kind: a Docker volume in an environment,
// which its services mount by name, as a Compose file's volumes. Docker
// names it as Compose names a project's volume, <project>_<name>, with
// Compose's labels, so docker compose finds it.
//
// Deleting one destroys its data, so the plan says so. The volume stays on
// the server for KeepFor first, so undoing the deletion brings the data
// back, and Prune removes it after.
package volume

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// Kind is the volume kind.
const Kind resource.Kind = "volume"

// KeepFor is how long a deleted volume stays on its server, with its data,
// as the PRD keeps what destructive actions remove for 7 days.
const KeepFor = 7 * 24 * time.Hour

// Target is a volume in Docker.
type Target struct {
	// Name is the volume's name in Docker.
	Name string
	// Project is its Compose project, and Key its name there.
	Project, Key string
}

// TargetOf is where a volume goes: <project>_<name>, in its environment's
// Compose project.
func TargetOf(ref resource.Ref) Target {
	p := docker.Project(ref.Scope)
	return Target{Name: docker.ScopedName(p, ref.Name), Project: p, Key: ref.Name}
}

// Runtime is the part of Docker the driver needs.
type Runtime interface {
	// Inspect returns the volume as Docker has it, or nil if there is none,
	// or if it was deleted and is only kept.
	Inspect(ctx context.Context, t Target) (*resourcev1.Volume, error)
	// Create makes the volume, or brings back one deleted and kept.
	// restore says the plan expected to bring one back: if it is gone, as
	// a prune removed it, Create fails rather than make it empty.
	Create(ctx context.Context, t Target, spec *resourcev1.Volume, restore bool) error
	// Delete deletes the volume, which stays on the server for KeepFor.
	Delete(ctx context.Context, t Target) error
}

// Driver manages volumes.
type Driver struct {
	Runtime Runtime
}

var _ resource.Driver = (*Driver)(nil)

// Kind returns "volume".
func (*Driver) Kind() resource.Kind { return Kind }

// Level is the environment: a volume belongs to its services.
func (*Driver) Level() resource.Level { return resource.LevelEnvironment }

// IDPrefix is "vol".
func (*Driver) IDPrefix() string { return "vol" }

// New returns an empty Volume.
func (*Driver) New() proto.Message { return &resourcev1.Volume{} }

// CheckName requires a name Docker takes, up to 63 characters, as Compose
// files name volumes, such as db-data.
func (*Driver) CheckName(name string) error {
	if !container.NameRE.MatchString(name) || len(name) > 63 {
		return fmt.Errorf("%q isn't a valid volume name: use up to 63 letters, digits, dots, dashes and underscores, starting with a letter or digit, such as db-data", name)
	}
	return nil
}

// Validate fills in the local driver, and refuses Dockwell's own labels,
// a mount the local driver isn't allowed to make, and a bind of a server
// path no container may mount.
func (*Driver) Validate(spec proto.Message) error {
	v := spec.(*resourcev1.Volume)
	if v.Driver == "" {
		v.Driver = "local"
	}
	if err := container.CheckDriver(v.GetDriver()); err != nil {
		return err
	}
	for k := range v.GetLabels() {
		if docker.IsOwnLabel(k) {
			return fmt.Errorf("the label %s is Dockwell's own, and Docker Compose's; choose a key that doesn't start with %s or %s", k, docker.ComposePrefix, docker.OwnPrefix)
		}
	}
	for k := range v.GetOptions() {
		if strings.TrimSpace(k) == "" {
			return errors.New("a volume's driver options need names, such as type")
		}
	}
	return validateMount(v)
}

// passwordNames are what an option gives a password by: CIFS takes it in o as
// password=, pass= or password2=, the one it moves to as a password changes, and
// a plugin may take one of its own, as vieux/sshfs takes
// password.
var passwordNames = []string{"password", "pass", "password2"}

// Keep keeps a volume's password, and its options when an edit gives none, as an
// export leaves them out. A password in the edit never compares with the one there:
// where the edit gives the volume as it is but for that, it gets the options the
// volume has, so a guess plans the same right or wrong, on any server, and a plan
// says the volume keeps its password.
//
// An edit that gives a password and differs in anything else is refused, in words
// that hold nothing of it, wherever it places the volume. Docker can't change a
// volume, but a move makes it anew on another server, so keeping the password there
// would take it to the address or driver the edit names. Planning it with the
// edit's own password wouldn't do either, as the paths a plan says changed would
// say whether that one was right.
func (*Driver) Keep(current, desired proto.Message) (resource.Kept, error) {
	c, d := current.(*resourcev1.Volume), desired.(*resourcev1.Volume)
	switch {
	case len(d.GetOptions()) == 0 && len(c.GetOptions()) > 0 && d.GetDriver() == c.GetDriver():
		d.Options = maps.Clone(c.GetOptions())
		return resource.Kept{Reason: "kept_options", Words: "keeps the options it was made with, as this gives it none, which is how an export writes it. " +
			"Docker can't change a volume, so to make it without them, make a volume of another name, move its data across, then delete this one"}, nil
	case !HoldsPassword(d.GetOptions()):
		return resource.Kept{}, nil
	case d.GetDriver() != c.GetDriver() || !maps.Equal(withoutPasswords(d.GetOptions()), withoutPasswords(c.GetOptions())):
		return resource.Kept{}, errors.New("docker can't change a volume once it is made, and a plan never compares a volume's password, so its options can't change here. " +
			"Make a new volume as you want it, move the data across, then delete this one")
	}
	d.Options = maps.Clone(c.GetOptions())
	return resource.Kept{Reason: "kept_password", Words: "keeps the password it was made with. A plan never compares a volume's password with the one there, so it doesn't say whether this one is the same. " +
		"Docker can't change a volume, so to give it another, make a volume of another name, move its data across, then delete this one"}, nil
}

// withoutPasswords is a volume's options with what gives a password blanked, in
// place, so what's left can be compared.
func withoutPasswords(options map[string]string) map[string]string {
	out := make(map[string]string, len(options))
	for k, v := range options {
		if passwordName(k) {
			out[k] = ""
			continue
		}
		items := strings.Split(v, ",")
		for i, item := range items {
			if name, _, ok := strings.Cut(item, "="); ok && passwordName(name) {
				items[i] = name + "="
			}
		}
		out[k] = strings.Join(items, ",")
	}
	return out
}

// HoldsPassword reports whether a volume's options give a password: an option of
// that name, or an item of one in a list of them, such as o's
// username=svc,password=…. A plan never shows a volume's options, but a command
// line stays in shell history and the process list, so the CLI takes options that
// give one from a file or the environment.
func HoldsPassword(options map[string]string) bool {
	for k, v := range options {
		if passwordName(k) {
			return true
		}
		for item := range strings.SplitSeq(v, ",") {
			if name, _, ok := strings.Cut(item, "="); ok && passwordName(name) {
				return true
			}
		}
	}
	return false
}

// passwordName reports an option's name that gives a password.
func passwordName(name string) bool {
	return slices.ContainsFunc(passwordNames, func(p string) bool { return strings.EqualFold(strings.TrimSpace(name), p) })
}

// mountTypes are what the local driver may mount besides a bind. Anything
// else reads paths the plan can't name: overlay takes them in o, and a
// disk's filesystem takes a device under /dev.
var mountTypes = []string{"nfs", "nfs4", "cifs", "tmpfs"}

// validateMount checks what the local driver's options mount, by an
// allowlist. A bind's device is checked as a container's bind is.
func validateMount(v *resourcev1.Volume) error {
	options := v.GetOptions()
	if v.GetDriver() != "local" || len(options) == 0 {
		return nil
	}
	kind := options["type"]
	dev, bind := container.BindDevice(v.GetDriver(), options)
	switch {
	case bind && kind != "none" && kind != "":
		return fmt.Errorf("the volume binds %s with type %s, and a bind takes type none. Use type: none, or a mount without bind", dev, kind)
	case bind:
		if !path.IsAbs(dev) {
			return fmt.Errorf("the volume binds the server path %q, which isn't absolute; start its device with /, such as /srv/data", dev)
		}
		if err := container.CheckSpelling(dev, "the server path "+dev+" that the volume binds"); err != nil {
			return err
		}
		dev = path.Clean(dev)
		options["device"] = dev
		return container.DefaultHost.CheckPath(dev, "the server path "+dev+" that the volume binds")
	case kind == "none":
		return errors.New("a volume of type none binds a server path, and this one has no bind. Add bind to o, such as o: bind, with the path in device")
	case kind == "":
		return errors.New("a volume's driver options need a type. Use type none with bind, nfs, nfs4, cifs or tmpfs")
	case !slices.Contains(mountTypes, kind):
		return fmt.Errorf("the volume mounts type %q, which Dockwell can't run: a plan couldn't show what it reaches. Use type none with bind, nfs, nfs4, cifs or tmpfs", kind)
	case kind == "cifs" || kind == "nfs" || kind == "nfs4":
		return validateRemote(kind, options)
	}
	return nil
}

// maxMountText is the most a mount's device or options may hold: the kernel
// reads one page of options, and a path of up to this long.
const maxMountText = 4096

// What a remote mount takes in o, by name. A plan shows the server and share
// a person approves, and masks the rest of o, so an option must not be able
// to change them. These lists name what is safe, rather than what isn't: the
// kernel's parsers take more than it looks, such as CIFS's source=, which
// replaces the device, sep= and ,, which change how the rest reads, and
// sloppy, which makes NFS skip what it doesn't know. They are plain lists so
// an agent doesn't build them as it starts.
var (
	remoteFlags = []string{
		"ro", "rw", "defaults", "nosuid", "suid", "nodev", "dev", "noexec", "exec",
		"noatime", "atime", "nodiratime", "diratime", "relatime", "norelatime", "strictatime", "nostrictatime", "sync", "async",
	}
	cifsValues = []string{
		"addr", "ip", "port", "vers", "sec", "username", "user", "password", "pass", "password2", "domain", "dom", "workgroup",
		"uid", "gid", "cruid", "file_mode", "dir_mode", "iocharset", "cache", "rsize", "wsize", "bsize", "actimeo", "acregmax", "acdirmax",
		"closetimeo", "echo_interval", "handletimeout", "max_credits", "esize", "max_channels", "dirmode",
	}
	cifsFlags = []string{
		"guest", "forceuid", "noforceuid", "forcegid", "noforcegid", "hard", "soft", "seal", "sign", "nodfs",
		"unix", "nounix", "linux", "nolinux", "posix", "noposix", "serverino", "noserverino", "perm", "noperm", "brl", "nobrl", "nolease",
		"mfsymlinks", "nomfsymlinks", "sfu", "nosfu", "cifsacl", "nocifsacl", "modefromsid", "idsfromsid", "acl", "noacl",
		"mapchars", "nomapchars", "mapposix", "nomapposix", "nocase", "ignorecase", "nosharesock", "persistenthandles", "nopersistenthandles",
		"resilienthandles", "noresilienthandles", "nohandlecache", "handlecache", "multichannel", "nomultichannel", "rdma", "fsc",
		"_netdev", "nostrictsync", "nosetuids", "nouser_xattr", "dynperm", "intr",
	}
	nfsValues = []string{
		"addr", "port", "vers", "nfsvers", "minorversion", "proto", "sec", "clientaddr", "nconnect", "timeo", "retrans", "rsize", "wsize",
		"acregmin", "acregmax", "acdirmin", "acdirmax", "actimeo", "local_lock", "lookupcache", "namlen",
		"retry", "mountproto", "mountvers", "mountport",
	}
	nfsFlags = []string{
		"tcp", "udp", "rdma", "soft", "softerr", "hard", "ac", "noac", "cto", "nocto", "lock", "nolock",
		"sharecache", "nosharecache", "resvport", "noresvport", "rdirplus", "nordirplus", "fsc", "nofsc",
		"intr", "nointr", "bg", "fg",
	}
)

// remoteOption says whether a remote mount of this kind takes an option, and
// whether the option takes a value.
func remoteOption(kind, name string) (known, takesValue bool) {
	values, flags := nfsValues, nfsFlags
	if kind == "cifs" {
		values, flags = cifsValues, cifsFlags
	}
	if slices.Contains(values, name) {
		return true, true
	}
	return slices.Contains(flags, name) || slices.Contains(remoteFlags, name), false
}

// validName is a server's name, as a device or addr= gives it.
func validName(h string) bool {
	if h == "" || strings.HasPrefix(h, ".") || strings.HasPrefix(h, "-") || strings.HasSuffix(h, ".") || strings.HasSuffix(h, "-") {
		return false
	}
	for _, r := range h {
		switch {
		case r == '.', r == '-', r == '_', '0' <= r && r <= '9', 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z':
		default:
			return false
		}
	}
	// A name that looks like a number can resolve as one, in another form
	// than a plan shows: glibc reads 010.0.0.2 as 8.0.0.2.
	last := strings.ToLower(h[strings.LastIndex(h, ".")+1:])
	return strings.Trim(last, "0123456789") != "" && !strings.HasPrefix(last, "0x")
}

// validHost is a server in a device: a name, or an IPv6 address in brackets.
func validHost(h string) bool {
	if inner, ok := strings.CutPrefix(h, "["); ok {
		addr, ok := strings.CutSuffix(inner, "]")
		_, err := netip.ParseAddr(addr)
		return ok && err == nil
	}
	if addr, err := netip.ParseAddr(h); err == nil && addr.Is4() {
		return true
	}
	return validName(h)
}

// validateRemote checks that a plan can say what a CIFS or NFS volume
// reaches, and that what it says is so. Its errors never repeat device or an
// option's value, or part of one: a password with a comma in it reads as an
// option of its own.
func validateRemote(kind string, options map[string]string) error {
	if err := validateDevice(kind, options["device"]); err != nil {
		return err
	}
	o, servers := options["o"], 0
	if len(o) > maxMountText {
		return fmt.Errorf("the volume's o holds %d bytes, and the kernel reads up to %d. Leave out what you can", len(o), maxMountText)
	}
	if o != "" {
		for i, opt := range strings.Split(o, ",") {
			name, value, hasValue := strings.Cut(opt, "=")
			known, takesValue := remoteOption(kind, name)
			switch {
			case !known:
				return fmt.Errorf("option %d of the volume's o isn't one Dockwell takes for a %s mount, as a plan couldn't show what it reaches. "+
					"See the list in docs/adr/0021-the-docker-driver.md. If it's part of a password, choose a password without a comma", i+1, kind)
			case takesValue && !hasValue:
				return fmt.Errorf("the option %s in the volume's o takes a value. Write it as %s=value", name, name)
			case !takesValue && hasValue:
				return fmt.Errorf("the option %s in the volume's o takes no value. Write it as %s alone", name, name)
			case name != "addr" && name != "ip":
				continue
			}
			servers++
			if _, err := netip.ParseAddr(value); err != nil && (name == "ip" || !validName(value)) {
				return fmt.Errorf("the option %s in the volume's o isn't a server's address. Give one such as %s=10.0.0.2", name, name)
			}
		}
	}
	switch {
	case servers > 1:
		return errors.New("the volume's o names its server more than once, so a plan couldn't say which it reaches. Keep one addr or ip")
	case servers == 0 && kind != "cifs":
		return errors.New("the volume's o doesn't name the NFS server, and the kernel needs it there. Add it as addr=10.0.0.2")
	}
	return nil
}

// unseen is a character a person can't see, or that reads as a space: Unicode
// counts blank braille and the Hangul fillers as printable, so a device could
// spell out the rest of a plan's line with them.
func unseen(r rune) bool {
	return !unicode.IsPrint(r) || r == 0x2800 || unicode.In(r, unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector)
}

// validateDevice checks a remote mount's device has the one form a plan can
// show: no user or password, which belong in o, nothing Docker reads its own
// way, as it unescapes a CIFS device from version 25, and nothing a person
// couldn't see, or that could read as part of a plan.
func validateDevice(kind, dev string) error {
	const cifsForm, nfsForm = "Write it as //server/share", "Write it as :/path, with the server in o as addr=10.0.0.2, or as server:/path"
	form := nfsForm
	if kind == "cifs" {
		form = cifsForm
	}
	if len(dev) > maxMountText {
		return fmt.Errorf("the volume's device holds %d bytes, and the kernel reads up to %d. %s", len(dev), maxMountText, form)
	}
	refused := "@,()\\"
	if kind == "cifs" {
		refused += "%+#" // Docker from version 25 reads a CIFS device as a URL
	}
	if strings.ContainsAny(dev, refused) || strings.TrimSpace(dev) != dev || strings.ContainsFunc(dev, unseen) {
		return fmt.Errorf("the volume's device has characters a %s device can't, such as @. A user and password go in o, as username=name,password=secret. %s", kind, form)
	}
	if kind == "cifs" {
		rest, ok := strings.CutPrefix(dev, "//")
		host, share, _ := strings.Cut(rest, "/")
		if !ok || !validHost(host) || share == "" {
			return errors.New("the volume's device isn't a CIFS share. " + form)
		}
		return nil
	}
	host, _, ok := strings.Cut(dev, ":/")
	if !ok || host != "" && !validHost(host) {
		return errors.New("the volume's device isn't an NFS export. " + form)
	}
	return nil
}

// HostAccess is what a volume reaches on its server, beyond what Docker
// keeps for it: the server path it binds, the mount its options make, such
// as an NFS share, or what a plugin driver does. It names the type, where
// it reaches and how, and never the credentials in its options.
func (*Driver) HostAccess(spec proto.Message) []resource.Access {
	v := spec.(*resourcev1.Volume)
	options := v.GetOptions()
	o := container.ReadMountOptions(options["o"])
	if v.GetDriver() != "local" && v.GetDriver() != "" {
		a := resource.Access{Kind: resource.AccessMount, Driver: v.GetDriver(), ReadOnly: o.ReadOnly}
		// A plugin's options are its own, and what one mounts is often among them,
		// such as local-persist's mountpoint. A system path is no secret, so it can
		// be said. Any other value stays out, as it may be one.
		for _, key := range slices.Sorted(maps.Keys(options)) {
			if p := path.Clean(options[key]); path.IsAbs(p) && (resource.SystemPath(p, o.ReadOnly) || resource.ControlsDocker(p)) {
				a.SameAsRoot, a.Path = true, p
				break
			}
		}
		return []resource.Access{a}
	}
	if dev, ok := container.BindDevice(v.GetDriver(), options); ok {
		return []resource.Access{container.PathAccess(path.Clean(dev), o.ReadOnly)}
	}
	if len(options) == 0 {
		return nil
	}
	a := resource.Access{Kind: resource.AccessMount, Type: options["type"], Address: o.Address, ReadOnly: o.ReadOnly}
	if a.Type != "tmpfs" {
		a.Path = options["device"]
	}
	return []resource.Access{a}
}

// Leads says where the server path a volume binds leads on this server.
func (d *Driver) Leads(_ context.Context, _ resource.Ref, desired proto.Message) (map[string]string, error) {
	if l, ok := d.Runtime.(pathLeader); ok {
		return l.Leads(desired.(*resourcev1.Volume))
	}
	return nil, nil
}

// pathLeader is a Runtime that can say where a volume's server path leads, as
// Docker can.
type pathLeader interface {
	Leads(spec *resourcev1.Volume) (map[string]string, error)
}

// checker is a Runtime that checks a spec on its server, as Docker does.
type checker interface {
	CheckHost(ctx context.Context, spec *resourcev1.Volume) error
}

// Check refuses a volume that binds a server path no container may mount,
// as the agent's server has it.
func (d *Driver) Check(ctx context.Context, _ resource.Ref, desired proto.Message) error {
	if c, ok := d.Runtime.(checker); ok {
		return c.CheckHost(ctx, desired.(*resourcev1.Volume))
	}
	return nil
}

// restorer is a Runtime that keeps deleted volumes, as Docker does.
type restorer interface {
	// Restores reports whether creating the volume at t brings back one
	// deleted and kept, with its data.
	Restores(ctx context.Context, t Target) (bool, error)
}

// Judge plans a step as Plan does, and says whether a create brings back
// a volume the server kept, as undoing its deletion does: the plan says
// so, and its fingerprint differs from one that makes the volume empty.
func (d *Driver) Judge(ctx context.Context, ref resource.Ref, actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	dec, err := d.Plan(actual, desired, changes)
	if err != nil || dec.Action != resource.ActionCreate {
		return dec, err
	}
	if r, ok := d.Runtime.(restorer); ok {
		if dec.Restores, err = r.Restores(ctx, TargetOf(ref)); err != nil {
			return resource.Decision{}, err
		}
	}
	return dec, nil
}

// Observe asks Docker.
func (d *Driver) Observe(ctx context.Context, ref resource.Ref) (proto.Message, error) {
	v, err := d.Runtime.Inspect(ctx, TargetOf(ref))
	if err != nil || v == nil {
		return nil, err
	}
	return v, nil
}

// Plan creates and deletes volumes. Deleting one destroys its data, though
// it is kept for KeepFor first. Docker can't change a volume, and making
// it again would empty it, so a change to one is refused.
func (d *Driver) Plan(actual, desired proto.Message, changes []resource.Change) (resource.Decision, error) {
	switch {
	case actual == nil:
		return resource.Decision{Action: resource.ActionCreate}, nil
	case desired == nil:
		return resource.Decision{Action: resource.ActionDelete, Destroys: true}, nil
	}
	what := "its spec"
	if len(changes) > 0 {
		what = changes[0].Path
	}
	return resource.Decision{}, fmt.Errorf("docker can't change a volume once it is made, and making it again would empty it, so its %s can't change. "+
		"Make a new volume as you want it, move the data across, then delete this one", what)
}

// Apply creates or deletes the volume.
func (d *Driver) Apply(ctx context.Context, step resource.Step, desired proto.Message) error {
	t := TargetOf(step.Ref)
	switch step.Action {
	case resource.ActionCreate:
		return d.Runtime.Create(ctx, t, desired.(*resourcev1.Volume), step.Restores)
	case resource.ActionDelete:
		return d.Runtime.Delete(ctx, t)
	case resource.ActionUpdate, resource.ActionReplace:
		return fmt.Errorf("docker can't change the volume %s once it is made", t.Name)
	case resource.ActionNone:
	}
	return nil
}

// Verify has nothing to check: the volume exists as asked for.
func (*Driver) Verify(context.Context, resource.Ref, proto.Message) error { return nil }

// Ready says why volumes can't be made now, such as Docker not running.
func (d *Driver) Ready(ctx context.Context) error {
	if r, ok := d.Runtime.(resource.Readier); ok {
		return r.Ready(ctx)
	}
	return nil
}

// ReadyKey is the runtime's, such as its Docker client.
func (d *Driver) ReadyKey() any {
	if r, ok := d.Runtime.(resource.SharedReadier); ok {
		return r.ReadyKey()
	}
	return d
}
