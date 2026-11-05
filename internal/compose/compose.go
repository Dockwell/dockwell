// SPDX-License-Identifier: AGPL-3.0-only

// Package compose turns a Docker Compose file into an environment's
// services, volumes and networks, and back. It parses with compose-go, as
// docker compose does, in the control plane alone. An environment is one
// Compose project: its containers carry Compose's labels, so docker compose
// sees them, and an exported file runs the same under docker compose. See
// docs/adr/0021-the-docker-driver.md.
package compose

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/compose-spec/compose-go/v2/transform"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/go-viper/mapstructure/v2"
	"github.com/sirupsen/logrus"
	"go.yaml.in/yaml/v4"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/container"
)

// Item is one resource a Compose file makes: its name and spec.
type Item[T any] struct {
	Name string
	Spec T
}

// Stack is what a Compose file makes in an environment, in the order to
// make it: networks and volumes first, then services, each after those it
// depends on.
type Stack struct {
	// Name is what the file's name: calls it, as written once its variables
	// are read: the project it belongs to, unless a request names another.
	// Empty for a file with none.
	Name     string
	Networks []Item[*resourcev1.Network]
	Volumes  []Item[*resourcev1.Volume]
	Services []Item[*resourcev1.Service]
	// Variables are the values of the environment's variables, by name, for
	// Export to write for the services that use them. Never a secret's.
	Variables map[string]string
	// Notices are what the plan should say of the file that no step does.
	Notices []Notice
	// Exposed says where the file writes a variable's value that anyone who can
	// read what it makes sees, for the plan to warn of.
	Exposed []Exposed
	// ExposedMore says the file writes more than Exposed lists.
	ExposedMore bool
}

// Notice is what a plan should say of a file that no step does, such as an
// image the file builds and Dockwell doesn't.
type Notice struct {
	// Reason says what it is about, for a surface with words of its own.
	Reason string
	// Service is the service it is about.
	Service string
	// Message says it, for people. It holds only what has been checked.
	Message string
	// Names are what it is about, such as the image.
	Names []string
}

// ErrInvalid is a Compose file Dockwell can't take, with why.
var ErrInvalid = errors.New("invalid Compose file")

// ErrCostly is a file the guard stopped, as reading it took more time or memory
// than Dockwell allows. Nothing should read it again.
var ErrCostly = invalid("reading the Compose file took more time or memory than Dockwell allows, so it stopped. " +
	"A real file doesn't: look for one very long word in a command, thousands of names in one place, or aliases repeated many times")

// ErrBusy is too many files waiting to be read to wait with them.
var ErrBusy = errors.New("too many Compose files are waiting to be read, so this one wasn't. Try again in a moment")

// maxMessage is the most of a refusal that goes to a caller or a log. A library's
// error can repeat what a file holds once for each mistake in it: 87 KiB came to
// 128 MiB of text.
const maxMessage = 4 << 10

func invalid(format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	if len(message) > maxMessage {
		message = strings.ToValidUTF8(message[:maxMessage], "") + "…"
	}
	return fmt.Errorf("%w: %s", ErrInvalid, message)
}

// Message is an error's text for people, without ErrInvalid's prefix.
func Message(err error) string {
	return strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ")
}

// What Dockwell runs of a Compose file. Anything else is refused by name,
// rather than dropped, so nothing in a file is quietly ignored, apart from
// x- extensions and what changes nothing on one server, such as expose.
var (
	topKeys     = keys("name", "version", "services", "volumes", "networks")
	serviceKeys = keys("image", "command", "entrypoint", "environment", "ports", "restart", "labels", "volumes",
		"networks", "network_mode", "user", "working_dir", "healthcheck", "depends_on", "expose",
		"container_name", "build", "deploy", "cpus", "mem_limit", "mem_reservation",
		"cap_add", "cap_drop", "group_add", "privileged")
	volumeKeys  = keys("driver", "driver_opts", "labels")
	networkKeys = keys("driver", "driver_opts", "internal", "labels", "external", "name")
	healthKeys  = keys("test", "interval", "timeout", "start_period", "retries", "disable")
	portKeys    = keys("target", "published", "host_ip", "protocol", "mode")
	mountKeys   = keys("type", "source", "target", "read_only", "bind", "consistency")
	bindKeys    = keys("selinux")
	dependsKeys = keys("condition", "required", "restart")
	deployKeys  = keys("resources")
	limitsKeys  = keys("limits", "reservations")
	amountKeys  = keys("cpus", "memory")
	joinKeys    = keys("aliases")

	// why says why a key Dockwell can't run is refused, and what to do,
	// where that is more than removing it.
	why = map[string]string{ //nolint:gosec // reasons for refusing keys such as secrets, not credentials
		"build":      "builds an image, which Dockwell can't do yet. Build and push the image, then name it with `image`",
		"env_file":   "reads `env_file`, a file beside the Compose file, which Dockwell doesn't have. Put the variables in `environment`",
		"label_file": "reads `label_file`, a file beside the Compose file, which Dockwell doesn't have. Put the labels in `labels`",
		"extends":    "extends another service, which Dockwell can't follow yet. Write the service out in full",
		"profiles":   "is in `profiles`, which Dockwell doesn't select. Remove `profiles` to run it, or the service to leave it out",
		"secrets":    "uses Compose `secrets`, which Dockwell can't run yet. Pass the values as environment variables",
		"configs":    "uses Compose `configs`, which Dockwell can't run yet. Mount the files from the server instead",
		"include":    "includes other files, which Dockwell doesn't have. Put everything in one file",
		"external":   "is `external`, made outside the file, and a service can only join a network that is, for now. Let the file make the volume, or mount its path on the server",
		"pid":        "shares a process namespace, which Dockwell can't run yet: with the server's, it sees and signals every process there. Remove `pid`",
		"ipc":        "shares an IPC namespace, which Dockwell can't run yet. Remove `ipc`",
		"devices":    "uses the server's devices, which Dockwell can't run yet. Remove `devices`, or run it with docker run for now",
		"name":       "names itself in Docker, which Dockwell names as Compose does, `<project>_<name>`. Remove `name`",
	}
)

func keys(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// check refuses what a section of the file has beyond allowed, naming it
// as what, such as "the service web".
func check(what string, m map[string]any, allowed map[string]bool) error {
	for _, k := range slices.Sorted(maps.Keys(m)) {
		if allowed[k] || strings.HasPrefix(k, "x-") {
			continue
		}
		if reason, ok := why[k]; ok {
			return invalidAt(place{what, k}, "%s %s", what, reason)
		}
		return invalidAt(place{what, k}, "%s uses `%s`, which Dockwell can't run yet. Remove it from the Compose file to import it", what, k)
	}
	return nil
}

// checkRaw reads the file's YAML, and refuses anything Dockwell doesn't
// run, and anything that would read files on the control plane, such as
// env_file and include.
func checkRaw(file []byte) (map[string]any, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(file, &raw); err != nil {
		return nil, notYAML(err, file)
	}
	if len(raw) == 0 {
		return nil, invalid("the Compose file is empty. Give it services, such as web with image: nginx:1.27")
	}
	if err := check("the Compose file", raw, topKeys); err != nil {
		return nil, err
	}
	dropExtensions(raw)
	if err := measure(raw); err != nil {
		return nil, err
	}
	if err := conform(place{owner: fileOwner}, raw, fileShape()); err != nil {
		return nil, err
	}
	services, _ := raw["services"].(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(services)) {
		what := "the service " + name
		svc, ok := services[name].(map[string]any)
		if !ok {
			return nil, invalid("%s has no image. Name one, such as image: nginx:1.27", what)
		}
		if err := check(what, svc, serviceKeys); err != nil {
			return nil, err
		}
		if hc, ok := svc["healthcheck"].(map[string]any); ok {
			if err := check(what+"'s healthcheck", hc, healthKeys); err != nil {
				return nil, err
			}
		}
		for _, p := range list(svc["ports"]) {
			if m, ok := p.(map[string]any); ok {
				if err := check(what+"'s port", m, portKeys); err != nil {
					return nil, err
				}
			}
		}
		for _, v := range list(svc["volumes"]) {
			if m, ok := v.(map[string]any); ok {
				if err := check(what+"'s volume", m, mountKeys); err != nil {
					return nil, err
				}
				if b, ok := m["bind"].(map[string]any); ok {
					if err := check(what+"'s volume's bind", b, bindKeys); err != nil {
						return nil, err
					}
				}
			}
		}
		if nets, ok := svc["networks"].(map[string]any); ok {
			for _, n := range slices.Sorted(maps.Keys(nets)) {
				if m, ok := nets[n].(map[string]any); ok {
					if err := check(what+"'s network "+n, m, joinKeys); err != nil {
						return nil, err
					}
				}
			}
		}
		if err := checkDeploy(what, svc); err != nil {
			return nil, err
		}
		if deps, ok := svc["depends_on"].(map[string]any); ok {
			for _, d := range slices.Sorted(maps.Keys(deps)) {
				if m, ok := deps[d].(map[string]any); ok {
					if err := check(what+"'s dependency on "+d, m, dependsKeys); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	for section, allowed := range map[string]map[string]bool{"volumes": volumeKeys, "networks": networkKeys} {
		entries, _ := raw[section].(map[string]any)
		for _, name := range slices.Sorted(maps.Keys(entries)) {
			m, _ := entries[name].(map[string]any)
			if err := check(fmt.Sprintf("the %s %s", strings.TrimSuffix(section, "s"), name), m, allowed); err != nil {
				return nil, err
			}
		}
	}
	return raw, nil
}

// mountOptions are what Dockwell runs of the options a volume takes in
// Compose's short syntax, /srv/data:/data:ro,z. compose-go drops those it
// doesn't know, so they are checked here, on the file.
var mountOptions = map[string]string{
	"ro": "", "rw": "", "z": "", "Z": "",
	// Docker takes these only on macOS; on Linux they change nothing.
	"cached": "", "delegated": "", "consistent": "",
	"nocopy":   "with nocopy, which Dockwell can't run yet. Remove :nocopy",
	"shared":   "with shared propagation, which Dockwell can't run yet. Remove :shared",
	"rshared":  "with rshared propagation, which Dockwell can't run yet. Remove :rshared",
	"slave":    "with slave propagation, which Dockwell can't run yet. Remove :slave",
	"rslave":   "with rslave propagation, which Dockwell can't run yet. Remove :rslave",
	"private":  "with private propagation, which Dockwell can't run yet. Remove :private",
	"rprivate": "with rprivate propagation, which Dockwell can't run yet. Remove :rprivate",
}

// checkMountOptions refuses an option in a volume's short syntax that
// Dockwell can't run, by name.
func checkMountOptions(dict map[string]any) error {
	services, _ := dict["services"].(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(services)) {
		svc, _ := services[name].(map[string]any)
		for _, v := range list(svc["volumes"]) {
			spec, ok := v.(string)
			parts := strings.Split(spec, ":")
			if !ok || len(parts) != 3 {
				continue
			}
			for _, o := range strings.Split(parts[2], ",") {
				why, known := mountOptions[o]
				switch {
				case !known:
					return invalid("the service %s mounts %s at %s with the option %q, which Docker doesn't take. Use ro, rw, z or Z, or leave it out",
						name, parts[0], parts[1], o)
				case why != "":
					return invalid("the service %s mounts %s at %s %s", name, parts[0], parts[1], why)
				}
			}
		}
	}
	return nil
}

// maxExpandedPorts is the most that a file's short-form port ranges may come to,
// as many as a container takes.
const maxExpandedPorts = 4096

// checkPortRanges counts what a file's port ranges come to, before compose-go
// reads them: it makes one entry for each port, so 400 ranges of every port in a
// file of 4 KiB took all the memory there was. Ranges such as a mail server's or
// FTP's are fine. A variable can hold a range too, so this comes after they are
// read. The long form doesn't take a range.
func checkPortRanges(dict map[string]any) error {
	services, _ := dict["services"].(map[string]any)
	total := 0
	for _, name := range slices.Sorted(maps.Keys(services)) {
		svc, _ := services[name].(map[string]any)
		for _, p := range list(svc["ports"]) {
			switch p := p.(type) {
			case string:
				total += portsIn(p)
				if total > maxExpandedPorts {
					return invalid("the service %s publishes the port range %s, which takes the file past %d ports. List fewer", name, p, maxExpandedPorts)
				}
			case map[string]any:
				if published, ok := p["published"].(string); ok && strings.Contains(published, "-") {
					return invalid("the service %s publishes the port range %s in the long form, which Dockwell can't run yet. Write it in the short form, as %s:%v", name, published, published, p["target"])
				}
			}
		}
	}
	return nil
}

// portsIn is how many ports a short-form mapping's largest range holds.
func portsIn(mapping string) int {
	mapping, _, _ = strings.Cut(mapping, "/")
	most := 1
	for _, part := range strings.Split(mapping, ":") {
		from, to, ranged := strings.Cut(part, "-")
		if !ranged {
			continue
		}
		a, errA := strconv.Atoi(from)
		b, errB := strconv.Atoi(to)
		if errA != nil || errB != nil || b < a {
			continue // not a range of ports: what follows says what's wrong
		}
		most = max(most, b-a+1)
	}
	return most
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

var quiet sync.Once

// model is the part of a Compose file Dockwell runs, in compose-go's
// types.
type model struct {
	Services types.Services `yaml:"services"`
	Volumes  types.Volumes  `yaml:"volumes"`
	Networks types.Networks `yaml:"networks"`
	// limits are each service's limits, which takeResources read.
	limits map[string]*resourcev1.Resources
	// notices are what the plan should say of the file.
	notices []Notice
}

// MaxFile is the largest Compose file Parse reads: 256 KiB, far more than
// any real file. Reading one takes time and memory that grow faster than a
// file does, so a real file at this size takes up to 2 s and 44 MiB of heap, as
// against 13 s at 1 MiB. One made to cost more is stopped by guard.
const MaxFile = 256 << 10

// parsing limits how many files are read at once, as a write token can send
// as many as it likes, and waiting how many may wait for a turn, as each holds
// its file and its variables.
var (
	parsing = make(chan struct{}, 2)
	waiting atomic.Int32
)

const maxWaiting = 16

// Parse reads a Compose file into what it makes in an environment.
// Variables such as ${TAG} take their values from variables, or the
// defaults the file gives; one with neither is refused, rather than left
// blank, at every level (interpolate).
//
// It reads the file as docker compose does, with compose-go's canonical
// forms of the short syntaxes and its types, but not its loader. The loader validates against the Compose
// schema with a JSON Schema library that allocates 1.7 MB as every
// process starts, agents included (ADR 0021); checkRaw allows far less
// than the schema does, and the steps here do the rest of what the loader
// would: resolving variables, the default network, and references between
// services, volumes and networks.
func Parse(ctx context.Context, file []byte, variables map[string]string) (*Stack, error) {
	return Read(ctx, Input{File: file, Variables: variables})
}

// Input is a Compose file, and what it reads beside it.
type Input struct {
	File []byte
	// Variables are values for what it uses, which win over Env, as a shell's
	// win over an .env file.
	Variables map[string]string
	// Env is the text of the .env file beside it.
	Env string
	// Directory is where it is on the servers, which a path relative to it
	// starts from.
	Directory string
}

// Read reads a file as Parse does, with what it reads beside it.
func Read(ctx context.Context, in Input) (*Stack, error) {
	_, stack, err := read(ctx, in, false)
	return stack, err
}

// read takes one turn at reading a file: the child first, which says what the
// file says of itself when asked, as anything that reads a file does so under
// its limits; then Parse's reading here, of a file the child read to the end of
// what this will. So what it costs here, the child has already paid for.
func read(ctx context.Context, in Input, asked bool) (Inspection, *Stack, error) {
	if len(in.File) > MaxFile {
		return Inspection{}, nil, invalid("the Compose file is %d KiB, and Dockwell reads files of up to %d KiB. "+
			"Leave out what Dockwell doesn't run, or import the services in more than one file", len(in.File)>>10, MaxFile>>10)
	}
	if len(in.Env) > MaxEnv {
		return Inspection{}, nil, invalid("the .env file is %d KiB, and Dockwell reads files of up to %d KiB. Leave out what the Compose file doesn't use", len(in.Env)>>10, MaxEnv>>10)
	}
	if err := checkVariables(in.Variables); err != nil {
		return Inspection{}, nil, err
	}
	if err := checkDirectory(in.Directory); err != nil {
		return Inspection{}, nil, err
	}
	if waiting.Add(1) > maxWaiting {
		waiting.Add(-1)
		return Inspection{}, nil, ErrBusy
	}
	select {
	case parsing <- struct{}{}:
		waiting.Add(-1)
		defer func() { <-parsing }()
	case <-ctx.Done():
		waiting.Add(-1)
		return Inspection{}, nil, fmt.Errorf("stopped waiting to read the Compose file: %w", ctx.Err())
	}
	seen, err := guard(ctx, in, asked)
	if err != nil {
		return Inspection{}, nil, err
	}
	if asked && guardPath == "" {
		seen = inspect(ctx, in)
	}
	stack, err := parse(ctx, in)
	return seen, stack, err
}

// parse reads a file in this process.
func parse(ctx context.Context, in Input) (*Stack, error) {
	variables, err := readEnv(in.Env, in.Variables)
	if err != nil {
		return nil, err
	}
	raw, err := checkRaw(in.File)
	if err != nil {
		return nil, err
	}
	// compose-go warns through logrus, which Dockwell doesn't use: what
	// matters comes back as an error.
	quiet.Do(func() { logrus.SetOutput(io.Discard) })

	built, err := takeBuilds(raw)
	if err != nil {
		return nil, err
	}
	dict, exposed, err := interpolate(ctx, raw, variables)
	if err != nil {
		return nil, err
	}
	limits, unkept, err := takeResources(dict)
	if err != nil {
		return nil, err
	}
	if err := checkMountOptions(dict); err != nil {
		return nil, err
	}
	if err := checkPortRanges(dict); err != nil {
		return nil, err
	}
	if dict, err = transform.Canonical(dict, false); err != nil {
		return nil, invalid("the Compose file isn't valid: %v", err)
	}
	if dict, err = transform.SetDefaultValues(dict); err != nil {
		return nil, invalid("the Compose file isn't valid: %v", err)
	}
	var m model
	if err := decode(dict, &m); err != nil {
		return nil, invalid("the Compose file isn't valid: %v", err)
	}
	if ctx.Err() != nil {
		return nil, stopped(ctx)
	}
	m.limits, m.notices = limits, unkept
	if err := m.resolve(variables, in.Directory); err != nil {
		return nil, err
	}
	for _, name := range built {
		image := m.Services[name].Image
		if err := container.CheckImage(image); err != nil {
			return nil, invalid("the service %s: %v", name, err)
		}
		m.notices = append(m.notices, Notice{
			Reason: "no_build", Service: name, Names: []string{image},
			Message: fmt.Sprintf("%s has build beside image, and Dockwell builds nothing, so it runs %s as named: that image must be on the server, or a pull must be able to get it", name, image),
		})
	}
	out, err := stack(&m)
	if err != nil {
		return nil, err
	}
	out.Name, _ = dict["name"].(string)
	out.Exposed, out.ExposedMore = exposed.report()
	if ctx.Err() != nil {
		return nil, stopped(ctx)
	}
	if err := checkPlanWords(out); err != nil {
		return nil, err
	}
	return out, nil
}

// A name a plan's notes say: what a service, a network, a container, an image, a
// variable or a field can be called, and nothing that could read as words. Square
// brackets are for an image's registry at an IPv6 address.
var planName = regexp.MustCompile(`^[A-Za-z0-9_\[][A-Za-z0-9_.:/@+\[\]-]*$`)

// maxPlanName is the most of one that a plan says: an image with a long tag and a
// digest fits.
const maxPlanName = 512

// checkPlanWords refuses a file whose names a plan couldn't say as they are, since
// a plan's notes are Dockwell's words with names in them. Each name has a check of
// its own, which says more. This is what holds if a new note names something that
// doesn't, so a file's values can't write what a person reads as the plan's. It
// doesn't repeat the name, as it may come from a variable.
func checkPlanWords(s *Stack) error {
	var names []string
	for _, n := range s.Notices {
		names = append(append(names, n.Service), n.Names...)
	}
	for _, e := range s.Exposed {
		names = append(names, e.Variable, e.Owner, e.Field)
	}
	for _, name := range names {
		if len(name) > maxPlanName || !planName.MatchString(name) {
			return invalid("the Compose file names something a plan can't say as it is. Use letters, digits, dots, dashes and underscores in its names")
		}
	}
	return nil
}

// decode reads a canonical Compose model into compose-go's types, as its
// loader does: each type that reads its own short forms, such as a
// command given as one string, does, and strings become the numbers and
// booleans they stand for.
func decode(dict map[string]any, out *model) error {
	hook := func(from, to reflect.Value) (any, error) {
		type decoder interface{ DecodeMapstructure(any) error }
		u, ok := to.Interface().(decoder)
		if !ok && to.CanAddr() {
			u, ok = to.Addr().Interface().(decoder)
		}
		if !ok {
			return from.Interface(), nil
		}
		if to.Kind() == reflect.Pointer && to.IsNil() {
			to.Set(reflect.New(to.Type().Elem()))
			u = to.Interface().(decoder)
		}
		if err := u.DecodeMapstructure(from.Interface()); err != nil {
			return to.Interface(), err
		}
		return to.Interface(), nil
	}
	d, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook:       hook,
		Result:           out,
		TagName:          "yaml",
		Squash:           true,
		WeaklyTypedInput: true,
	})
	if err != nil {
		return err
	}
	return d.Decode(dict)
}

// resolve does what compose-go's loader does after reading a file: a
// variable named without a value takes it from variables, a service that
// names no network joins the project's default one, and every volume,
// network and service a service names must be in the file. A path relative
// to the file starts from directory, once that is known.
func (m *model) resolve(variables map[string]string, directory string) error {
	for _, name := range slices.Sorted(maps.Keys(m.Services)) {
		s := m.Services[name]
		what := "the service " + name
		for k, v := range s.Environment {
			if val, ok := variables[k]; v == nil && ok {
				s.Environment[k] = &val
			}
		}
		if err := m.readNetworkMode(what, &s); err != nil {
			return err
		}
		if len(s.Networks) == 0 && s.NetworkMode == "" {
			s.Networks = map[string]*types.ServiceNetworkConfig{"default": nil}
			if _, ok := m.Networks["default"]; !ok {
				if m.Networks == nil {
					m.Networks = types.Networks{}
				}
				m.Networks["default"] = types.NetworkConfig{}
			}
		}
		for n := range s.Networks {
			if _, ok := m.Networks[n]; !ok {
				return invalid("%s joins the network %s, which the file doesn't list under networks. Add it there", what, n)
			}
		}
		for i, v := range s.Volumes {
			if _, ok := m.Volumes[v.Source]; v.Type == types.VolumeTypeVolume && v.Source != "" && !ok {
				return invalid("%s mounts the volume %s, which the file doesn't list under volumes. Add it there", what, v.Source)
			}
			if v.Type == types.VolumeTypeBind && directory != "" && !path.IsAbs(v.Source) && !strings.HasPrefix(v.Source, "~") {
				s.Volumes[i].Source = path.Join(directory, v.Source)
			}
		}
		for dep := range s.DependsOn {
			if _, ok := m.Services[dep]; !ok {
				return invalid("%s depends on %s, which isn't a service in the file", what, dep)
			}
		}
		m.Services[name] = s
	}
	return nil
}

// stack turns a read file into specs.
func stack(p *model) (*Stack, error) {
	out := &Stack{}
	for _, name := range slices.Sorted(maps.Keys(p.Networks)) {
		n := p.Networks[name]
		// One the server has makes no resource: each service that joins it says
		// so in its own spec (ADR 0021).
		if bool(n.External) {
			if n.Driver != "" || len(n.DriverOpts) > 0 || n.Internal || len(n.Labels) > 0 {
				return nil, invalid("the network %s is external, so Dockwell doesn't make it, and can't give it a driver, options, internal or labels. Remove them", name)
			}
			continue
		}
		if n.Name != "" {
			return nil, invalid("the network %s %s", name, why["name"])
		}
		out.Networks = append(out.Networks, Item[*resourcev1.Network]{Name: name, Spec: &resourcev1.Network{
			Driver: n.Driver, Options: nonEmpty(n.DriverOpts), Internal: n.Internal, Labels: userLabels(n.Labels),
		}})
	}
	for _, name := range slices.Sorted(maps.Keys(p.Volumes)) {
		v := p.Volumes[name]
		out.Volumes = append(out.Volumes, Item[*resourcev1.Volume]{Name: name, Spec: &resourcev1.Volume{
			Driver: v.Driver, Options: nonEmpty(v.DriverOpts), Labels: userLabels(v.Labels),
		}})
	}
	if len(p.Services) == 0 {
		return nil, invalid("the Compose file has no services. Give it one, such as web with image: nginx:1.27")
	}
	specs := map[string]*resourcev1.Service{}
	for _, name := range slices.Sorted(maps.Keys(p.Services)) {
		s := p.Services[name]
		uses, err := usesOf(name, &s)
		if err != nil {
			return nil, err
		}
		c, err := app(name, &s, uses, p)
		if err != nil {
			return nil, err
		}
		specs[name] = &resourcev1.Service{Runs: &resourcev1.Service_App{App: c}}
		for _, u := range uses {
			binding.Use(specs[name], u)
		}
	}
	out.Notices = p.notices
	slices.SortStableFunc(out.Notices, func(a, b Notice) int { return cmp.Compare(a.Service, b.Service) })
	order, err := ordered(p.Services)
	if err != nil {
		return nil, err
	}
	for _, name := range order {
		out.Services = append(out.Services, Item[*resourcev1.Service]{Name: name, Spec: specs[name]})
	}
	return out, nil
}

// usesOf reads what a service uses from the label an export writes, of
// the names its environment still has, so taking one out of the file stops
// using it. The revisions it records are the store's to set again.
func usesOf(name string, s *types.ServiceConfig) ([]resource.Use, error) {
	all, err := binding.ParseLabel(s.Labels[binding.LabelKey])
	if err != nil {
		return nil, invalid("the service %s has a label it can't use: %v. Remove the label, or export the file again", name, err)
	}
	var uses []resource.Use
	for _, u := range all {
		if _, ok := s.Environment[u.As]; ok {
			uses = append(uses, u)
		}
	}
	return uses, nil
}

// ordered lists services so each comes after those it depends on, by name
// where that leaves a choice, and refuses a cycle.
func ordered(services types.Services) ([]string, error) {
	var out []string
	const visiting, done = 1, 2
	state := map[string]int{}
	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch state[name] {
		case done:
			return nil
		case visiting:
			return invalid("the services depend on each other in a circle, %s. Break it, as nothing could start first",
				strings.Join(append(path, name), " → "))
		}
		state[name] = visiting
		for _, dep := range slices.Sorted(maps.Keys(services[name].DependsOn)) {
			if err := visit(dep, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = done
		out = append(out, name)
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(services)) {
		if err := visit(name, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func nonEmpty[M ~map[string]string](m M) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return maps.Clone(map[string]string(m))
}

// userLabels are a file's labels without Dockwell's and Compose's own,
// which an exported file carries and Dockwell sets again. What a service
// uses comes back as references (usesOf).
func userLabels(l types.Labels) map[string]string {
	out := map[string]string{}
	for k, v := range l {
		if !docker.IsOwnLabel(k) && k != binding.LabelKey {
			out[k] = v
		}
	}
	return nonEmpty(out)
}

var restarts = map[string]resourcev1.RestartPolicy{
	"":               resourcev1.RestartPolicy_RESTART_POLICY_NO,
	"no":             resourcev1.RestartPolicy_RESTART_POLICY_NO,
	"always":         resourcev1.RestartPolicy_RESTART_POLICY_ALWAYS,
	"unless-stopped": resourcev1.RestartPolicy_RESTART_POLICY_UNLESS_STOPPED,
	"on-failure":     resourcev1.RestartPolicy_RESTART_POLICY_ON_FAILURE,
}

// app turns one Compose service into the container it runs.
func app(name string, s *types.ServiceConfig, uses []resource.Use, p *model) (*resourcev1.Container, error) {
	if err := resource.CheckLabel("service", name, "api-v2"); err != nil {
		return nil, invalid("%v. Rename the service in the Compose file", err)
	}
	what := "the service " + name
	if s.Image == "" {
		return nil, invalid("%s has no image. Name one, such as image: nginx:1.27", what)
	}
	c := &resourcev1.Container{
		Image: s.Image, Command: s.Command, Entrypoint: s.Entrypoint,
		User: s.User, WorkingDir: s.WorkingDir, Labels: userLabels(s.Labels),
	}
	// entrypoint: [] and entrypoint: "" run none, not even the image's;
	// compose-go reads both as an empty list, and no entrypoint as nil.
	if s.Entrypoint != nil && len(s.Entrypoint) == 0 {
		c.NoEntrypoint = true
	}
	var err error
	if c.Restart, c.RestartRetries, err = readRestart(what, s.Restart); err != nil {
		return nil, err
	}
	c.CapAdd, c.CapDrop, c.GroupAdd, c.Privileged, c.Resources = s.CapAdd, s.CapDrop, s.GroupAdd, s.Privileged, p.limits[name]
	for _, k := range slices.Sorted(maps.Keys(s.Environment)) {
		v := s.Environment[k]
		if v == nil && slices.ContainsFunc(uses, func(u resource.Use) bool { return u.As == k }) {
			continue // the value is the environment's, and the service uses it by reference
		}
		if v == nil {
			return nil, invalidAt(place{what, "environment." + k}, "%s's variable %s has no value. Give it one in the file, such as %s: value, or pass it in variables", what, k, k)
		}
		if c.Env == nil {
			c.Env = map[string]string{}
		}
		c.Env[k] = *v
	}
	for i, p := range s.Ports {
		m, err := port(what, p)
		if err != nil {
			return nil, refusedAt(place{what, "ports"}.index(i), err)
		}
		c.Ports = append(c.Ports, m)
	}
	for i, v := range s.Volumes {
		at := place{what, "volumes"}.index(i)
		m := &resourcev1.Mount{Target: v.Target, ReadOnly: v.ReadOnly}
		if err := mountBind(what, &v, m); err != nil {
			return nil, refusedAt(at, err)
		}
		switch {
		case v.Type == types.VolumeTypeVolume && v.Source == "":
			return nil, invalidAt(at, "%s mounts an unnamed volume at %s. Name it, and list it under volumes, so Dockwell can keep its data", what, v.Target)
		case v.Type == types.VolumeTypeVolume:
			m.Source = &resourcev1.Mount_Volume{Volume: v.Source}
		case v.Type == types.VolumeTypeBind && !path.IsAbs(v.Source):
			return nil, invalidAt(at, "%s mounts %s, a path relative to the Compose file, and Dockwell doesn't know where the file is on the server. "+
				"Pass the directory it is in, as directory (--directory on the CLI), or use an absolute path, such as /srv/%s/data, or a named volume", what, v.Source, name)
		case v.Type == types.VolumeTypeBind:
			m.Source = &resourcev1.Mount_HostPath{HostPath: v.Source}
		default:
			return nil, invalidAt(at, "%s mounts a %s at %s, which Dockwell can't run yet. Use a named volume or a server path", what, v.Type, v.Target)
		}
		c.Mounts = append(c.Mounts, m)
	}
	if err := joinNetworks(name, s, c, p); err != nil {
		return nil, err
	}
	if hc := s.HealthCheck; hc != nil {
		if hc.Disable {
			c.Healthcheck = &resourcev1.Healthcheck{Test: []string{"NONE"}}
		} else {
			c.Healthcheck = &resourcev1.Healthcheck{Test: hc.Test}
			dur := func(d *types.Duration) string {
				if d == nil || *d == 0 {
					return ""
				}
				return time.Duration(*d).String()
			}
			c.Healthcheck.Interval, c.Healthcheck.Timeout, c.Healthcheck.StartPeriod = dur(hc.Interval), dur(hc.Timeout), dur(hc.StartPeriod)
			if hc.Retries != nil {
				c.Healthcheck.Retries = uint32(min(*hc.Retries, 1<<31))
			}
		}
	}
	return c, nil
}

// readRestart reads a service's restart, with the count on-failure may take, as
// on-failure:5.
func readRestart(what, restart string) (resourcev1.RestartPolicy, uint32, error) {
	name, count, counted := strings.Cut(restart, ":")
	policy, known := restarts[name]
	if !known {
		return 0, 0, invalidAt(place{what, "restart"}, "%s restarts %q, which Docker doesn't take. Use no, always, unless-stopped or on-failure", what, restart)
	}
	if !counted {
		return policy, 0, nil
	}
	if name != "on-failure" {
		return 0, 0, invalidAt(place{what, "restart"}, "%s restarts %q, and only on-failure takes a count. Use on-failure:%s, or leave the count out", what, restart, count)
	}
	n, err := strconv.ParseUint(count, 10, 31)
	if err != nil {
		return 0, 0, invalidAt(place{what, "restart"}, "%s restarts %q, and on-failure's count is a number of times, such as on-failure:5", what, restart)
	}
	return policy, uint32(n), nil
}

// mountBind reads what a mount says of binding a server path: relabelling
// it, which Dockwell runs, and its propagation and nocopy, which it can't
// yet. Each is refused by name.
func mountBind(what string, v *types.ServiceVolumeConfig, m *resourcev1.Mount) error {
	if v.Volume != nil && v.Volume.NoCopy {
		return invalid("%s mounts %s at %s with nocopy, which Dockwell can't run yet. Remove nocopy", what, v.Source, v.Target)
	}
	if v.Bind == nil {
		return nil
	}
	if p := v.Bind.Propagation; p != "" {
		return invalid("%s mounts %s at %s with %s propagation, which Dockwell can't run yet. Remove it", what, v.Source, v.Target, p)
	}
	switch v.Bind.SELinux {
	case "":
		return nil
	case "z":
		m.Relabel = resourcev1.Relabel_RELABEL_SHARED
	case "Z":
		m.Relabel = resourcev1.Relabel_RELABEL_PRIVATE
	default:
		return invalid("%s relabels %s with selinux %s, which Docker doesn't take. Use z, shared with other containers, or Z, private to this one", what, v.Source, v.Bind.SELinux)
	}
	if v.Type != types.VolumeTypeBind {
		return invalid("%s mounts the volume %s at %s with :%s, which Dockwell can do for a server path alone. Remove it", what, v.Source, v.Target, v.Bind.SELinux)
	}
	return nil
}

// port turns a Compose port into a published one. Ports must name the
// server's port: one Docker picks would move each time the service is
// recreated.
func port(what string, p types.ServicePortConfig) (*resourcev1.PortMapping, error) {
	if p.Published == "" {
		return nil, invalid("%s publishes port %d on a server port Docker would pick. Name the server's port, such as \"8080:%d\"", what, p.Target, p.Target)
	}
	hp, err := strconv.ParseUint(p.Published, 10, 16)
	if err != nil {
		return nil, invalid("%s publishes port %d on %s, a range Dockwell can't run yet. Name one server port, such as \"8080:%d\"", what, p.Target, p.Published, p.Target)
	}
	m := &resourcev1.PortMapping{HostPort: uint32(hp), ContainerPort: p.Target, HostIp: p.HostIP}
	switch cmp.Or(p.Protocol, "tcp") {
	case "tcp":
		m.Protocol = resourcev1.Protocol_PROTOCOL_TCP
	case "udp":
		m.Protocol = resourcev1.Protocol_PROTOCOL_UDP
	default:
		return nil, invalid("%s publishes port %d over %s, which Dockwell can't run yet. Use tcp or udp", what, p.Target, p.Protocol)
	}
	return m, nil
}
