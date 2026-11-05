// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/buildinfo"
	"github.com/dockwell/dockwell/internal/cli/apicli"
	"github.com/dockwell/dockwell/internal/cli/apicmd"
	"github.com/dockwell/dockwell/internal/datadir"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/seal"
	"github.com/dockwell/dockwell/internal/store"
)

// The API commands are generated from the schema (internal/cli/apicmd) and
// run by shared machinery (internal/cli/apicli). This file gives them what
// they run with: which control plane to call, and with which API token.
// See docs/adr/0019-a-cli-generated-from-the-schema.md.
//
// The control plane and the token come from, in order:
//  1. --url and --token;
//  2. DOCKWELL_URL and DOCKWELL_TOKEN;
//  3. the config file dockwell login writes, which holds a token for each
//     control plane it signed in to, and never sends one to another;
//  4. local access, on the control server, as a user who can read its data
//     directory: the CLI makes its own token, read-only unless dockwell
//     login --local asks for more, for LocalTokenTTL.

// apiCommands are the generated commands, with login and logout.
func apiCommands(stdio IO, g *globalFlags) ([]*cobra.Command, error) {
	a := &apiAccess{stdio: stdio, g: g}
	env := &apicli.Env{
		Out: stdio.Out, Err: stdio.Err, In: stdio.In, Getenv: stdio.Getenv,
		Interactive: stdio.Interactive,
		ReadSecret:  a.readValue,
		Connect:     a.connect,
		Kinds:       controlKinds().With(scope.Types()...),
	}
	methods, err := apicli.Commands(env, apiMethods(), apicmd.Nouns(), apicmd.Kinds())
	if err != nil {
		return nil, err
	}
	return append([]*cobra.Command{a.loginCmd(), a.logoutCmd()}, methods...), nil
}

// apiMethods describes the API's methods' commands. Tests replace it.
var apiMethods = apicmd.Methods

// apiAccess finds the control plane and the token to call it with.
type apiAccess struct {
	stdio IO
	g     *globalFlags
}

// cliConfig is the config file dockwell login writes.
type cliConfig struct {
	// URL is the control plane dockwell login signed in to last, which
	// commands call by default.
	URL string `json:"url,omitempty"`
	// Tokens are the API tokens for each control plane, by URL.
	Tokens map[string]string `json:"tokens,omitempty"`
}

// configPath is the config file, in the user's config directory, as
// os.UserConfigDir finds it.
func configPath(getenv func(string) string) (string, error) {
	if dir := getenv("XDG_CONFIG_HOME"); dir != "" && runtime.GOOS != "windows" {
		return filepath.Join(dir, "dockwell", "config.json"), nil
	}
	home := getenv("HOME")
	switch {
	case runtime.GOOS == "windows" && getenv("AppData") != "":
		return filepath.Join(getenv("AppData"), "dockwell", "config.json"), nil
	case home == "":
		return "", errNoConfigDir
	case runtime.GOOS == "darwin":
		return filepath.Join(home, "Library", "Application Support", "dockwell", "config.json"), nil
	}
	return filepath.Join(home, ".config", "dockwell", "config.json"), nil
}

// errNoConfigDir says there's nowhere to look for the config file.
var errNoConfigDir = errors.New("can't find your config directory, as HOME isn't set, so dockwell has no config file to read or write. " +
	"Set HOME, or set " + apicli.EnvURL + " and " + apicli.EnvToken + " instead")

// loadConfig reads the config file dockwell login writes. A file that isn't
// there is an empty config; one it can't use, because others can read it
// or it doesn't parse, is an error that says how to fix it.
func loadConfig(getenv func(string) string) (cliConfig, string, error) {
	path, err := configPath(getenv)
	if err != nil {
		return cliConfig{}, "", err
	}
	var cfg cliConfig
	raw, err := os.ReadFile(path) //nolint:gosec // the user's own config file, by design
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, path, nil
	}
	if err != nil {
		return cfg, path, fmt.Errorf("can't read %s: %w", path, err)
	}
	if info, err := os.Stat(path); err == nil && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return cfg, path, fmt.Errorf("%s holds API tokens, but others can read it, so dockwell won't use them. "+
			"Run chmod 600 %s, and revoke its tokens in the panel if someone else could have read them", path, path)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, path, fmt.Errorf("%s isn't a config file dockwell can read: %w. Remove it, then run dockwell login again", path, err)
	}
	return cfg, path, nil
}

// writePrivate writes a file only its owner can read, in a directory only
// they can open, all at once.
func writePrivate(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("can't create %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("can't write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("can't write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func saveConfig(path string, cfg cliConfig) error {
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, append(raw, '\n'))
}

// errNoControlPlane says the CLI doesn't know what to call.
var errNoControlPlane = errors.New("dockwell doesn't know which control plane to call. " +
	"Sign in with dockwell login and the panel's address, with an API token you make in the panel under Profile and security, " +
	"or set " + apicli.EnvURL + " and " + apicli.EnvToken)

func (a *apiAccess) client(url, token string, local bool) *apicli.Client {
	return &apicli.Client{
		HTTP: apicli.NewHTTPClient(), URL: url, Token: token, Local: local,
		Version: buildinfo.Read().Version,
	}
}

// connect resolves the control plane and the token, as the comment above
// says.
func (a *apiAccess) connect(ctx context.Context, s apicli.Settings) (*apicli.Client, error) {
	getenv := a.stdio.Getenv
	url, token := s.URL, s.Token
	urlFrom := "--url"
	if url == "" {
		url, urlFrom = getenv(apicli.EnvURL), apicli.EnvURL
	}
	if token == "" {
		token = getenv(apicli.EnvToken)
	}
	var cfg cliConfig
	var path string
	if url == "" || token == "" {
		// The config file gives what the flags and the environment leave
		// out. One the CLI can't use is said first, with its fix, rather
		// than passed over for local access.
		var err error
		cfg, path, err = loadConfig(getenv)
		// Without a config directory, there is no config file, and local
		// access may still answer.
		noFile := errors.Is(err, errNoConfigDir) && url == "" && token == ""
		if err != nil && !noFile {
			return nil, err
		}
	}
	if url == "" && token == "" && cfg.URL == "" {
		return a.local(ctx, s)
	}
	if url == "" {
		url, urlFrom = cfg.URL, path
	}
	if url == "" {
		return nil, fmt.Errorf("an API token is set, but not which control plane to call. Set %s, or pass --url", apicli.EnvURL)
	}
	url, err := apicli.NormalizeURL(url)
	if err != nil {
		return nil, usageError{fmt.Errorf("%s: %w", urlFrom, err)}
	}
	if s.Locate {
		return a.client(url, "", false), nil
	}
	if token == "" {
		token = cfg.Tokens[url]
	}
	if token == "" {
		return nil, fmt.Errorf("there's no API token for %s. Sign in with dockwell login %s, or set %s", url, url, apicli.EnvToken)
	}
	if !auth.WellFormedToken(token) {
		return nil, usageError{fmt.Errorf("the API token for %s isn't a whole one: tokens start with %s and have %d characters. Copy it again from the panel", url, auth.TokenPrefix, auth.TokenLen)}
	}
	return a.client(url, token, false), nil
}

// localToken is what the CLI keeps of the token it made through local
// access, in the data directory, where only those who could make it can
// read it.
type localToken struct {
	URL     string    `json:"url"`
	ID      string    `json:"id"`
	Secret  string    `json:"token"`
	Scope   string    `json:"scope"`
	Expires time.Time `json:"expires"`
}

// localGrace is how long a local token must have left for the CLI to keep
// using it.
const localGrace = time.Minute

func (a *apiAccess) dataDir() (datadir.Dir, error) { return a.g.dir(a.stdio) }

func readLocalToken(dir datadir.Dir) (localToken, bool, error) {
	var t localToken
	raw, err := os.ReadFile(dir.LocalToken())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return t, false, nil
	case err != nil:
		return t, false, err
	}
	// One that can't be read is made again.
	ok := json.Unmarshal(raw, &t) == nil
	return t, ok, nil
}

// local is the CLI's access on the control server itself: as a user who
// can read the control plane's data directory, it makes its own API token
// for the install's owner. Whoever can read that directory controls the
// install already, as for dockwell recover. The token is read-only unless
// dockwell login --local asked for more, and lasts LocalTokenTTL. Renew
// makes a new read-only one, as when the control plane rejected the one it
// had; Moved reads where the control plane answers now, and keeps the
// token; Locate only finds where it answers, and makes no token.
//
// Local access opens the control plane's state as it is, and never
// migrates it: see openLocalState.
func (a *apiAccess) local(ctx context.Context, s apicli.Settings) (*apicli.Client, error) {
	dir, err := a.dataDir()
	if err != nil {
		return nil, err
	}
	if !hasControlPlane(dir) {
		return nil, a.noControlPlane(dir)
	}
	t, ok, err := readLocalToken(dir)
	if errors.Is(err, fs.ErrPermission) {
		return nil, a.cantReadDataDir(dir)
	}
	if err != nil {
		return nil, err
	}
	if s.Locate {
		if ok && t.URL != "" {
			return a.client(t.URL, "", true), nil
		}
		url, err := a.localURL(ctx, dir)
		if err != nil {
			return nil, err
		}
		return a.client(url, "", true), nil
	}
	usable := ok && time.Until(t.Expires) > localGrace && auth.WellFormedToken(t.Secret)
	if usable && s.Moved {
		if t.URL, err = a.localURL(ctx, dir); err != nil {
			return nil, err
		}
		if err := writeLocalToken(dir, t); err != nil {
			return nil, err
		}
	}
	if usable && !s.Renew {
		return a.client(t.URL, t.Secret, true), nil
	}
	made, err := a.makeLocalToken(ctx, dir, store.ScopeReadOnly, t)
	if err != nil {
		return nil, err
	}
	return a.client(made.URL, made.Secret, true), nil
}

// systemDataDir is where the system service keeps its data, which a user
// other than root doesn't look in by default. Tests move it.
var systemDataDir = datadir.SystemRoot

// noControlPlane says there's no control plane in dir to call. A user
// other than root looks in a data directory of their own by default, so on
// the control server the control plane is elsewhere, in the system's: when
// it is there, or can't be looked at, the error says to use sudo.
func (a *apiAccess) noControlPlane(dir datadir.Dir) error {
	if a.g.dataDir != "" || dir.Root == systemDataDir {
		return errNoControlPlane
	}
	system := datadir.Dir{Root: systemDataDir}
	_, err := os.Stat(system.Control())
	switch {
	case err == nil && hasControlPlane(system):
		return fmt.Errorf("there's no API token set, and this server's control plane keeps its state in %s, where dockwell looks only for root. "+
			"Run this with sudo, or pass --data-dir %s", system.Root, system.Root)
	case err == nil:
		return a.cantReadDataDir(system)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("there's no API token set, and only root can look in %s, where a control plane on this server keeps its state, so dockwell can't use it. "+
			"On the control server, run this with sudo. Elsewhere, sign in with dockwell login and the panel's address, "+
			"with an API token you make in the panel under Profile and security", system.Root)
	}
	return errNoControlPlane
}

// openLocalState opens the control plane's state for a command run beside
// it, such as local access or dockwell claim, as it is. It never migrates
// it: a migration under a control plane that runs an older dockwell, as
// between an upgrade and its restart, would change the schema beneath it.
// A state at another schema version than this dockwell's is refused, with
// what to do.
func openLocalState(ctx context.Context, dir datadir.Dir, location string) (*store.DB, error) {
	if err := controlState(dir, location); err != nil {
		return nil, err
	}
	db, err := store.OpenExisting(ctx, location)
	var schema *store.SchemaError
	switch {
	case errors.As(err, &schema) && schema.Database < schema.Binary:
		return nil, fmt.Errorf("the control plane here runs an older dockwell than this one: its state has schema version %d, and this dockwell's is %d. "+
			"Only dockwell run upgrades the state, as it starts the control plane, so nothing changes it under the one running now. "+
			"Restart the control plane so it runs this dockwell, with sudo systemctl restart dockwell or sudo rc-service dockwell restart, then try again",
			schema.Database, schema.Binary)
	case errors.As(err, &schema):
		return nil, fmt.Errorf("the control plane here runs a newer dockwell than this one: its state has schema version %d, and this dockwell knows %d. "+
			"Run the dockwell the control plane runs, as its service starts it", schema.Database, schema.Binary)
	}
	return db, err
}

// openLocalPeople opens the control plane's people for local access, with
// its state as it is.
func openLocalPeople(ctx context.Context, dir datadir.Dir, location string, logger *slog.Logger) (*auth.Service, *store.DB, panelAddress, error) {
	db, err := openLocalState(ctx, dir, location)
	if err != nil {
		return nil, nil, panelAddress{}, err
	}
	key, err := seal.Load(dir.SealKey())
	if err != nil {
		_ = db.Close()
		return nil, nil, panelAddress{}, err
	}
	return peopleOn(ctx, key, db, logger)
}

// localURL is where the control plane in dir answers on loopback, as
// dockwell run recorded it when it started. It reads that one setting.
func (a *apiAccess) localURL(ctx context.Context, dir datadir.Dir) (string, error) {
	location, err := a.g.databaseOf(a.stdio, dir)
	if err != nil {
		return "", err
	}
	db, err := openLocalState(ctx, dir, location)
	if errors.Is(err, fs.ErrPermission) {
		return "", a.cantReadDataDir(dir)
	}
	if err != nil {
		return "", err
	}
	defer db.Close()
	addr, known, err := loadPanelAddress(ctx, db)
	if err == nil && !known {
		err = errors.New("the control plane hasn't started yet. Start it with dockwell run")
	}
	if err != nil {
		return "", err
	}
	return addr.loopbackURL(), nil
}

// loopbackURL is the panel's address on loopback, for the CLI on the
// control server.
func (a panelAddress) loopbackURL() string {
	return "http://" + net.JoinHostPort(strings.Trim(a.tunnelHost(), "[]"), a.Port)
}

func writeLocalToken(dir datadir.Dir, t localToken) error {
	// The secret is kept on purpose, in a file only its makers can read.
	raw, err := json.MarshalIndent(t, "", "  ") //nolint:gosec // see above
	if err != nil {
		return err
	}
	return writePrivate(dir.LocalToken(), append(raw, '\n'))
}

func (a *apiAccess) cantReadDataDir(dir datadir.Dir) error {
	return fmt.Errorf("there's no API token set, and you can't read this server's control plane in %s, so dockwell can't make one. "+
		"Run it as root, with sudo, or sign in with dockwell login", dir.Root)
}

// makeLocalToken makes a token for local access with scope, replacing
// old, and keeps it in the data directory.
func (a *apiAccess) makeLocalToken(ctx context.Context, dir datadir.Dir, tokenScope string, old localToken) (localToken, error) {
	logger, err := a.g.logger(a.stdio.Err)
	if err != nil {
		return localToken{}, err
	}
	location, err := a.g.databaseOf(a.stdio, dir)
	if err != nil {
		return localToken{}, err
	}
	people, db, addr, err := openLocalPeople(ctx, dir, location, logger)
	if errors.Is(err, fs.ErrPermission) {
		return localToken{}, a.cantReadDataDir(dir)
	}
	if err != nil {
		return localToken{}, err
	}
	defer db.Close()
	host, _ := os.Hostname()
	name := "Local access on " + orPlaceholder(host, "the control server")
	if tokenScope == store.ScopeReadWrite {
		name += ", read-write"
	} else {
		name += ", read-only"
	}
	made, err := people.CreateLocalToken(ctx, actor(), name, tokenScope)
	if err != nil {
		return localToken{}, err
	}
	if old.ID != "" {
		// Best effort: an old token expires on its own.
		_, _ = people.RevokeLocalToken(ctx, actor(), old.ID)
	}
	t := localToken{
		URL: addr.loopbackURL(),
		ID:  made.Token.ID, Secret: made.Secret, Scope: tokenScope, Expires: *made.Token.ExpiresAt,
	}
	if err := writeLocalToken(dir, t); err != nil {
		return localToken{}, err
	}
	return t, nil
}

// readSecret reads a token: without echo on a terminal, or the first line
// of standard input.
func (a *apiAccess) readSecret(prompt string) (string, error) {
	if f, ok := a.stdio.In.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(a.stdio.Err, prompt)
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(a.stdio.Err)
		return strings.TrimSpace(string(b)), err
	}
	b, err := io.ReadAll(io.LimitReader(a.stdio.In, 4096))
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line), nil
}

// readValue reads a secret's value on a terminal, without echo. Unlike a
// token, it is kept as typed: only the line's end is dropped.
func (a *apiAccess) readValue(prompt string) (string, error) {
	f, ok := a.stdio.In.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return "", errors.New("standard input isn't a terminal, so the value can't be asked for. Pipe it in instead")
	}
	fmt.Fprint(a.stdio.Err, prompt)
	b, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(a.stdio.Err)
	return string(b), err
}

func (a *apiAccess) loginCmd() *cobra.Command {
	var url, token string
	var local bool
	cmd := &cobra.Command{
		Use:     "login [<url>]",
		Short:   "Sign the CLI in to a control plane with an API token",
		GroupID: apicli.GroupID,
		Long: "Stores an API token for the control plane at url, such as https://panel.acme.dev, in your config directory, " +
			"readable only by you, after checking that it works. Commands then call that control plane with it.\n\n" +
			"Make the token in the panel, under Profile and security. (dockwell token create makes join tickets for servers, not API tokens.) " +
			"The token comes from --token or " + apicli.EnvToken + "; otherwise dockwell asks for it on a terminal without showing it, " +
			"or reads it from standard input.\n\n" +
			"On the control server, the CLI needs no sign-in: as a user who can read the control plane's data directory, such as root, " +
			"it makes its own read-only token for the install's owner. --local lets that token make changes too, for " + apicli.Span(auth.LocalTokenTTL) + ".",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return usageError{fmt.Errorf("%s takes at most one argument, the panel's address. Run %q to see how to use it", cmd.CommandPath(), cmd.CommandPath()+" --help")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if local {
				return a.loginLocal(cmd)
			}
			if len(args) == 1 {
				url = args[0]
			}
			if url == "" {
				url = a.stdio.Getenv(apicli.EnvURL)
			}
			if url == "" {
				return usageError{errors.New("dockwell login needs the panel's address, and got none. Pass it, as in dockwell login https://panel.acme.dev")}
			}
			base, err := apicli.NormalizeURL(url)
			if err != nil {
				return usageError{err}
			}
			if token == "" {
				token = a.stdio.Getenv(apicli.EnvToken)
			}
			if token == "" {
				if token, err = a.readSecret("API token for " + base + ": "); err != nil {
					return err
				}
			}
			if !auth.WellFormedToken(token) {
				return usageError{fmt.Errorf("that isn't a whole API token: tokens start with %s and have %d characters. Copy it again from the panel, under Profile and security",
					auth.TokenPrefix, auth.TokenLen)}
			}
			c := a.client(base, token, false)
			servers := dockwellv1connect.NewServerServiceClient(c.HTTP, c.URL, apicli.ClientOptions(c)...)
			if _, err := servers.ListServers(cmd.Context(), connect.NewRequest(&dockwellv1.ListServersRequest{})); err != nil {
				return apicli.Explain(err, c, cmd.CommandPath())
			}
			cfg, path, err := loadConfig(a.stdio.Getenv)
			if err != nil {
				return err
			}
			if cfg.Tokens == nil {
				cfg.Tokens = map[string]string{}
			}
			cfg.URL, cfg.Tokens[base] = base, token
			if err := saveConfig(path, cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Signed in to %s with %s. Commands use it from now on; dockwell logout forgets it.\n", base, auth.TokenHint(token))
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "the panel's address, in place of the argument")
	cmd.Flags().StringVar(&token, "token", "", "the API token; set "+apicli.EnvToken+" or type it when asked instead, so it stays out of the process list")
	cmd.Flags().BoolVar(&local, "local", false, "on the control server, let local access make changes for "+apicli.Span(auth.LocalTokenTTL))
	return cmd
}

// loginLocal gives local access a read-write token.
func (a *apiAccess) loginLocal(cmd *cobra.Command) error {
	dir, err := a.dataDir()
	if err != nil {
		return err
	}
	if !hasControlPlane(dir) {
		return fmt.Errorf("there's no control plane in %s, so there's no local access to sign in to. Run it on the control server, or pass its --data-dir", dir.Root)
	}
	old, _, err := readLocalToken(dir)
	if errors.Is(err, fs.ErrPermission) {
		return a.cantReadDataDir(dir)
	}
	t, err := a.makeLocalToken(cmd.Context(), dir, store.ScopeReadWrite, old)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Local access on this server can make changes until %s, %s, as the install's owner. dockwell logout --local ends it sooner.\n",
		t.Expires.Local().Format("15:04 on 2 Jan"), apicli.In(t.Expires, time.Now()))
	return nil
}

func (a *apiAccess) logoutCmd() *cobra.Command {
	var url string
	var local bool
	cmd := &cobra.Command{
		Use:     "logout [<url>]",
		Short:   "Forget the API token for a control plane",
		GroupID: apicli.GroupID,
		Long: "Removes the API token dockwell login stored for the control plane at url, or for the one it signed in to last. " +
			"The token still works until you revoke it in the panel, under Profile and security.\n\n" +
			"--local revokes the token local access made on the control server instead; the next command makes a read-only one.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return usageError{fmt.Errorf("%s takes at most one argument, the panel's address. Run %q to see how to use it", cmd.CommandPath(), cmd.CommandPath()+" --help")}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if local {
				return a.logoutLocal(cmd)
			}
			cfg, path, err := loadConfig(a.stdio.Getenv)
			if err != nil {
				return err
			}
			if len(args) == 1 {
				url = args[0]
			}
			if url == "" {
				url = cfg.URL
			}
			if url == "" {
				return errors.New("dockwell isn't signed in to any control plane, so there's nothing to forget")
			}
			base, err := apicli.NormalizeURL(url)
			if err != nil {
				return usageError{err}
			}
			if _, ok := cfg.Tokens[base]; !ok {
				return fmt.Errorf("dockwell has no API token for %s, so there's nothing to forget", base)
			}
			delete(cfg.Tokens, base)
			if cfg.URL == base {
				cfg.URL = ""
			}
			if err := saveConfig(path, cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Forgot the API token for %s. It still works until you revoke it in the panel, under Profile and security.\n", base)
			return nil
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "the panel's address, in place of the argument")
	cmd.Flags().BoolVar(&local, "local", false, "revoke the token local access made on the control server")
	return cmd
}

func (a *apiAccess) logoutLocal(cmd *cobra.Command) error {
	dir, err := a.dataDir()
	if err != nil {
		return err
	}
	t, ok, err := readLocalToken(dir)
	if errors.Is(err, fs.ErrPermission) {
		return a.cantReadDataDir(dir)
	}
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(cmd.OutOrStdout(), "Local access has no token on this server, so there's nothing to end.")
		return nil
	}
	logger, err := a.g.logger(a.stdio.Err)
	if err != nil {
		return err
	}
	location, err := a.g.databaseOf(a.stdio, dir)
	if err != nil {
		return err
	}
	people, db, _, err := openLocalPeople(cmd.Context(), dir, location, logger)
	if errors.Is(err, fs.ErrPermission) {
		return a.cantReadDataDir(dir)
	}
	if err != nil {
		return err
	}
	defer db.Close()
	revoked, err := people.RevokeLocalToken(cmd.Context(), actor(), t.ID)
	if err != nil {
		return err
	}
	if err := os.Remove(dir.LocalToken()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("revoked local access's token, but can't remove %s: %w. Remove it yourself", dir.LocalToken(), err)
	}
	if !revoked {
		fmt.Fprintln(cmd.OutOrStdout(), "Local access's token no longer worked, so there was nothing to revoke. The next command on this server makes a read-only one.")
		return nil
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Revoked local access's token. The next command on this server makes a read-only one.")
	return nil
}
