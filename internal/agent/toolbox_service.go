// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

const (
	toolboxUnitDir    = "/etc/systemd/system"
	toolboxEnvDir     = "/etc/dockwell-toolbox"
	toolboxUnitPrefix = "dockwell-toolbox-"
	toolboxUnitMarker = "# Managed by Dockwell toolbox\n"
)

var (
	toolboxUnitName = toolboxPattern(`^[a-z0-9][a-z0-9-]{0,62}$`)
	toolboxUserName = toolboxPattern(`^(?:[a-zA-Z_][a-zA-Z0-9_-]{0,31}|\d{1,10})$`)
	toolboxEnvName  = toolboxPattern(`^[a-zA-Z_][a-zA-Z0-9_]{0,127}$`)
)

func (t *hostToolbox) serviceNames() []string {
	entries, err := os.ReadDir(t.path(toolboxUnitDir))
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		tail, found := strings.CutPrefix(entry.Name(), toolboxUnitPrefix)
		name, service := strings.CutSuffix(tail, ".service")
		if !found || !service || !toolboxUnitName().MatchString(name) {
			continue
		}
		w, err := t.file(toolboxUnitDir+"/"+entry.Name(), 0o644)
		if err == nil && strings.HasPrefix(string(w.old), toolboxUnitMarker) {
			names = append(names, entry.Name())
		}
	}
	return names
}

func toolboxUnitQuote(s string) string {
	return strconv.Quote(strings.ReplaceAll(s, "%", "%%"))
}

func toolboxLiteral(s string) bool {
	return len(s) <= 2048 && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

func toolboxServiceFile(spec *v1.ToolboxService, unit string) (string, error) {
	if !toolboxUserName().MatchString(spec.GetUser()) {
		return "", errors.New("enter a system account name or numeric user ID")
	}
	dir := spec.GetWorkingDirectory()
	if !toolboxLiteral(dir) || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || strings.TrimSpace(dir) != dir || strings.HasSuffix(dir, `\`) {
		return "", errors.New("enter an absolute working directory")
	}
	argv := spec.GetArgv()
	if len(argv) == 0 || len(argv) > 64 || !filepath.IsAbs(argv[0]) || filepath.Clean(argv[0]) != argv[0] {
		return "", errors.New("enter an absolute executable and up to 63 arguments")
	}
	restart := spec.GetRestart()
	if !slices.Contains([]string{"no", "on-failure", "always"}, restart) {
		return "", errors.New("choose a restart policy")
	}
	words := make([]string, 0, len(argv))
	for _, arg := range argv {
		if !toolboxLiteral(arg) {
			return "", errors.New("each command argument must be plain text, up to 2048 bytes")
		}
		words = append(words, toolboxUnitQuote(arg))
	}
	// WorkingDirectory is a literal path with specifiers, unlike the quoted
	// ExecStart arguments. Its parser does not unescape internal backslashes.
	var out strings.Builder
	fmt.Fprintf(&out, "%s[Unit]\nDescription=Dockwell service %s\n\n[Service]\nType=exec\nUser=%s\nWorkingDirectory=%s\nExecStart=:%s\nRestart=%s\n",
		toolboxUnitMarker, spec.GetName(), spec.GetUser(), strings.ReplaceAll(dir, "%", "%%"), strings.Join(words, " "), restart)
	if len(spec.GetEnvironment()) > 0 {
		fmt.Fprintf(&out, "EnvironmentFile=%s/%s.env\n", toolboxEnvDir, unit)
	}
	out.WriteString("\n[Install]\nWantedBy=multi-user.target\n")
	return out.String(), nil
}

func (t *hostToolbox) environmentFile(unit string, bindings []*v1.ToolboxBinding) (toolboxWrite, error) {
	name := toolboxEnvDir + "/" + unit + ".env"
	if len(bindings) > 32 {
		return toolboxWrite{}, errors.New("choose up to 32 environment values")
	}
	seen := map[string]bool{}
	for _, b := range bindings {
		if b.LiteralValue != nil {
			return toolboxWrite{}, errors.New("a direct environment value wasn't sealed. Review a new plan")
		}
		if !toolboxEnvName().MatchString(b.GetName()) || seen[b.GetName()] {
			return toolboxWrite{}, errors.New("environment bindings must have distinct names")
		}
		if len(b.GetSealedLiteral()) > 0 {
			if !b.GetSecret() || len(b.GetValueDigest()) != sha256.Size || b.GetRevision() != 0 || b.GetSource() != "" || b.GetEnvironmentId() != "" || b.GetReviewedValue() != "" {
				return toolboxWrite{}, errors.New("a direct environment value wasn't pinned. Review a new plan")
			}
		} else if b.GetRevision() < 1 || b.GetSource() == "" || b.GetEnvironmentId() == "" {
			return toolboxWrite{}, errors.New("environment bindings must have distinct names and refer to a Dockwell value")
		}
		seen[b.GetName()] = true
	}
	_, err := os.Lstat(t.path(toolboxEnvDir))
	if errors.Is(err, os.ErrNotExist) {
		return toolboxWrite{path: name, mode: 0o600, wasMissing: true}, t.trustedParent(toolboxEnvDir)
	}
	w, err := t.file(name, 0o600)
	if err == nil && !w.wasMissing && w.mode != 0o600 {
		err = errors.New("the service's environment file isn't private")
	}
	return w, err
}

func toolboxEnvironment(ctx context.Context, bindings []*v1.ToolboxBinding) ([]byte, error) {
	var out strings.Builder
	out.WriteString(toolboxUnitMarker)
	quote := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")
	values := resource.ValuesFrom(ctx)
	for _, b := range bindings {
		value, ok := values[b.GetName()]
		if len(b.GetSealedLiteral()) > 0 {
			key := values[resource.ToolboxLiteralKey(b.GetName())]
			if !ok || key == "" || !hmac.Equal(b.GetValueDigest(), resource.ToolboxLiteralDigest(key, value)) {
				return nil, errors.New("a direct environment value differs from the reviewed plan")
			}
		}
		if !ok || !utf8.ValidString(value) || strings.ContainsRune(value, 0) || len(value) > 32<<10 {
			return nil, errors.New("a Dockwell environment value can't be written to systemd")
		}
		fmt.Fprintf(&out, "%s=\"%s\"\n", b.GetName(), quote.Replace(value))
	}
	return []byte(out.String()), nil
}

func (t *hostToolbox) planService(ctx context.Context, edit *toolboxEdit, spec *v1.ToolboxService) error {
	if !t.systemd() {
		return errors.New("background services require systemd on this server")
	}
	if !toolboxUnitName().MatchString(spec.GetName()) {
		return errors.New("enter a service name using lowercase letters, numbers and hyphens")
	}
	unit := toolboxUnitPrefix + spec.GetName() + ".service"
	w, err := t.file(toolboxUnitDir+"/"+unit, 0o644)
	if err != nil {
		return err
	}
	env, err := t.environmentFile(unit, spec.GetEnvironment())
	if err != nil {
		return err
	}
	if !env.wasMissing && !strings.HasPrefix(string(env.old), toolboxUnitMarker) {
		return errors.New("the service's environment file isn't owned by Dockwell")
	}
	if spec.GetRemove() {
		if w.wasMissing || !strings.HasPrefix(string(w.old), toolboxUnitMarker) {
			return errors.New("that system service isn't owned by Dockwell")
		}
		fragment, err := t.run(ctx, toolboxSystem, "show", "--property=FragmentPath", "--value", "--", unit)
		if err != nil || strings.TrimSpace(string(fragment)) != w.path {
			return errors.New("systemd isn't running this service from Dockwell's unit file")
		}
		dropins, err := t.serviceDropins(ctx, unit)
		if err != nil {
			return err
		}
		if dropins != "" {
			toolboxDifference(edit.plan, "Systemd drop-ins", dropins, dropins)
		}
		edit.plan.UnitFile = string(w.old)
		running, _ := t.run(ctx, toolboxSystem, "is-active", "--", unit)
		enabled, _ := t.run(ctx, toolboxSystem, "is-enabled", "--", unit)
		wasRunning := strings.TrimSpace(string(running)) == "active"
		wasEnabled := strings.TrimSpace(string(enabled)) == "enabled"
		toolboxDifference(edit.plan, unit, "Installed", "Stop and remove")
		w.remove, env.remove = true, true
		edit.files = []toolboxWrite{w, env}
		edit.apply = func(ctx context.Context) error {
			return t.removeService(ctx, unit, w, env, wasRunning, wasEnabled)
		}
		return nil
	}
	if !w.wasMissing || !env.wasMissing {
		return errors.New("a service with that name already exists")
	}
	fragment, err := t.run(ctx, toolboxSystem, "show", "--property=FragmentPath", "--value", "--", unit)
	if err != nil || strings.TrimSpace(string(fragment)) != "" {
		return errors.New("systemd couldn't confirm that this service name is available")
	}
	file, err := toolboxServiceFile(spec, unit)
	if err != nil {
		return err
	}
	version, err := t.run(ctx, toolboxSystem, "--version")
	if err != nil {
		return err
	}
	fields := strings.Fields(string(version))
	if len(fields) < 2 || fields[0] != "systemd" {
		return errors.New("systemd didn't report its version")
	}
	number, err := strconv.Atoi(fields[1])
	if err != nil || number < 243 {
		return fmt.Errorf("background services need systemd 243 or later. This server runs %s", fields[1])
	}
	dropins, err := t.planServiceDropins(ctx, unit, number >= 244)
	if err != nil {
		return err
	}
	if len(dropins.paths) > 0 {
		toolboxDifference(edit.plan, "Systemd drop-ins for all services", "", strings.Join(dropins.paths, ", "))
	}
	edit.extra = dropins.digest
	if err := t.checkServicePaths(spec); err != nil {
		return err
	}
	w.next = []byte(file)
	edit.files = []toolboxWrite{w, env}
	edit.plan.UnitFile = file
	toolboxDifference(edit.plan, unit, "", "Install and start")
	toolboxDifference(edit.plan, "Start at boot", "", strconv.FormatBool(spec.GetStartOnBoot()))
	edit.apply = func(ctx context.Context) error { return t.createService(ctx, unit, w, env, spec, dropins) }
	return nil
}

func (t *hostToolbox) serviceDropins(ctx context.Context, unit string) (string, error) {
	raw, err := t.run(ctx, toolboxSystem, "show", "--property=DropInPaths", "--value", "--", unit)
	return strings.TrimSpace(string(raw)), err
}

func (t *hostToolbox) checkServicePaths(spec *v1.ToolboxService) error {
	account := spec.GetUser()
	uid, numeric := strconv.ParseUint(account, 10, 32)
	if t.tools.Root == "" {
		lookup := user.Lookup
		if numeric == nil {
			lookup = user.LookupId
			account = strconv.FormatUint(uid, 10)
		}
		row, err := lookup(account)
		if err != nil {
			return errors.New("that system account doesn't exist on this server")
		}
		uid, err = strconv.ParseUint(row.Uid, 10, 32)
		if err != nil {
			return errors.New("that system account has no usable user ID")
		}
	}
	if uid == 0 {
		for _, b := range spec.GetEnvironment() {
			if len(b.GetSealedLiteral()) == 0 && resource.ToolboxSensitiveEnv(b.GetName()) {
				return fmt.Errorf("%s can change how a root service starts. Enter its value directly in this plan", b.GetName())
			}
		}
	}
	accountID := uint32(uid)
	argv := spec.GetArgv()
	if err := t.checkServiceExecutable(argv[0], accountID); err != nil {
		return err
	}
	for _, arg := range argv[1:] {
		if filepath.IsAbs(arg) {
			if _, err := t.checkServicePath(arg, accountID, serviceArgument); err != nil {
				return err
			}
		}
		if _, value, found := strings.Cut(arg, "=/"); found {
			if _, err := t.checkServicePath("/"+value, accountID, serviceArgument); err != nil {
				return err
			}
		}
	}
	_, err := t.checkServicePath(spec.GetWorkingDirectory(), accountID, serviceDirectory)
	return err
}

func (t *hostToolbox) checkServiceExecutable(name string, uid uint32) error {
	pending := []string{name}
	for checked := 0; len(pending) > 0; checked++ {
		if checked == 8 {
			return errors.New("the service executable has too many script interpreters")
		}
		resolved, err := t.checkServicePath(pending[0], uid, serviceExecutable)
		if err != nil {
			return err
		}
		pending = pending[1:]
		f, err := os.Open(t.path(resolved))
		if err != nil {
			return err
		}
		var header [256]byte // Linux's initial executable read
		n, err := io.ReadFull(f, header[:])
		_ = f.Close()
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return err
		}
		line, script := strings.CutPrefix(string(header[:n]), "#!")
		if !script {
			continue
		}
		line, _, _ = strings.Cut(line, "\n")
		line, _, _ = strings.Cut(line, "\x00")
		line = strings.TrimLeft(line, " \t")
		interpreter, args := line, ""
		if at := strings.IndexAny(line, " \t"); at >= 0 {
			interpreter, args = line[:at], strings.Trim(line[at:], " \t")
		}
		if !filepath.IsAbs(interpreter) {
			return fmt.Errorf("%s must name an absolute script interpreter", resolved)
		}
		pending = append(pending, interpreter)
		if filepath.Base(interpreter) == "env" && filepath.IsAbs(args) {
			// Linux passes the whole optional argument as one word to env.
			pending = append(pending, args)
		}
	}
	return nil
}

type servicePathKind int

const (
	serviceDirectory servicePathKind = iota
	serviceExecutable
	serviceArgument
)

func (t *hostToolbox) checkServicePath(name string, uid uint32, kind servicePathKind) (string, error) {
	root, err := os.Stat(t.root())
	if err != nil {
		return "", err
	}
	if !t.owns(root) || root.Mode().Perm()&0o022 != 0 {
		return "", errors.New("/ is not owned by root or permits group or other writes. Restore root ownership and remove those write permissions")
	}
	// Walk from the root so all parents have been checked before following
	// a system symlink, including /bin on merged-/usr distributions.
	dir, pending := "/", strings.Split(strings.TrimPrefix(name, "/"), "/")
	for links := 0; len(pending) > 0; {
		part := pending[0]
		pending = pending[1:]
		path := filepath.Join(dir, part)
		info, err := os.Lstat(t.path(path))
		if err != nil {
			// An argument can name an output file. A missing path is safe
			// if only a trusted account can create its first missing part.
			if kind == serviceArgument && errors.Is(err, os.ErrNotExist) {
				return "", nil
			}
			return "", fmt.Errorf("%s is unavailable. Choose an existing service path: %w", path, err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		link := info.Mode()&os.ModeSymlink != 0
		if !ok || (!t.owns(info) && (uid == 0 || st.Uid != uid)) || (!link && info.Mode().Perm()&0o022 != 0) {
			return "", fmt.Errorf("%s can be changed by another account. Set its owner to root or the service account and remove group and other write permissions", path)
		}
		if link {
			links++
			if links > 40 {
				return "", fmt.Errorf("%s has too many symbolic links. Choose a direct service path", name)
			}
			target, err := os.Readlink(t.path(path))
			if err != nil {
				return "", err
			}
			if !filepath.IsAbs(target) {
				target = strings.TrimSuffix(dir, "/") + "/" + target
			}
			dir, pending = "/", append(strings.Split(strings.TrimPrefix(target, "/"), "/"), pending...)
			continue
		}
		if len(pending) == 0 && kind == serviceExecutable {
			if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				return "", fmt.Errorf("%s is not executable. Choose an executable file", path)
			}
		} else if (len(pending) > 0 || kind == serviceDirectory) && !info.IsDir() {
			return "", fmt.Errorf("%s is not a directory. Choose a service path through directories", path)
		}
		dir = path
	}
	return dir, nil
}

func (t *hostToolbox) createService(ctx context.Context, unit string, file, env toolboxWrite, spec *v1.ToolboxService, dropins toolboxServiceDropins) (err error) {
	if len(spec.GetEnvironment()) > 0 {
		env.next, err = toolboxEnvironment(ctx, spec.GetEnvironment())
		if err != nil {
			return err
		}
		defer clear(env.next)
		if err := os.Mkdir(t.path(toolboxEnvDir), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := t.trustedParent(env.path); err != nil {
			return err
		}
		if err := t.write(env); err != nil {
			return errors.Join(err, t.undo(env))
		}
	}
	var started, enabled bool
	defer func() {
		if err == nil {
			return
		}
		revert := context.WithoutCancel(ctx)
		var stop, disable error
		if started {
			_, stop = t.run(revert, toolboxSystem, "stop", "--", unit)
		}
		if enabled {
			_, disable = t.run(revert, toolboxSystem, "disable", "--", unit)
		}
		err = errors.Join(err, stop, disable, t.undo(file), t.undo(env))
		_, reload := t.run(revert, toolboxSystem, "daemon-reload")
		err = errors.Join(err, reload)
	}()
	if err := t.write(file); err != nil {
		return err
	}
	if _, err := t.run(ctx, toolboxSystem, "daemon-reload"); err != nil {
		return err
	}
	fragment, err := t.run(ctx, toolboxSystem, "show", "--property=FragmentPath", "--value", "--", unit)
	if err != nil || strings.TrimSpace(string(fragment)) != file.path {
		return errors.New("systemd didn't load Dockwell's unit file. Check the system log")
	}
	if err := t.checkLoadedServiceDropins(ctx, unit, dropins); err != nil {
		return err
	}
	if err := t.checkServicePaths(spec); err != nil {
		return err
	}
	if spec.GetStartOnBoot() {
		enabled = true
		if _, err := t.run(ctx, toolboxSystem, "enable", "--", unit); err != nil {
			return err
		}
	}
	// Scripts and applications can emit their environment in diagnostics. The
	// plan points to the unit journal and never carries systemctl's start output.
	started = true
	if _, err := t.run(ctx, toolboxSystem, "start", "--", unit); err != nil {
		return errors.New("the service didn't start. Check its system log")
	}
	return nil
}

func (t *hostToolbox) removeService(ctx context.Context, unit string, file, env toolboxWrite, running, enabled bool) (err error) {
	if _, err := t.run(ctx, toolboxSystem, "stop", "--", unit); err != nil {
		return errors.New("the service didn't stop. Check its system log")
	}
	defer func() {
		if err == nil {
			return
		}
		revert := context.WithoutCancel(ctx)
		err = errors.Join(err, t.undo(file), t.undo(env))
		_, reload := t.run(revert, toolboxSystem, "daemon-reload")
		err = errors.Join(err, reload)
		if enabled {
			_, back := t.run(revert, toolboxSystem, "enable", "--", unit)
			err = errors.Join(err, back)
		}
		if running {
			if _, back := t.run(revert, toolboxSystem, "start", "--", unit); back != nil {
				err = errors.Join(err, errors.New("the service didn't restart. Check its system log"))
			}
		}
	}()
	if _, err := t.run(ctx, toolboxSystem, "disable", "--", unit); err != nil {
		return err
	}
	if err := t.write(file); err != nil {
		return err
	}
	if !env.wasMissing {
		if err := t.write(env); err != nil {
			return err
		}
	}
	_, err = t.run(ctx, toolboxSystem, "daemon-reload")
	return err
}
