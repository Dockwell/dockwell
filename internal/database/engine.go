// SPDX-License-Identifier: AGPL-3.0-only

// Package database knows the databases Dockwell can read and manage from inside
// their own containers: PostgreSQL, MySQL, MariaDB, Redis, Valkey and MongoDB,
// from their official images. The control plane uses it to tell a database from any other
// service by its spec, and the agent to ask one what it holds, with the database's
// own tools, which sign in with what its environment already has, so no password
// passes through Dockwell. See docs/adr/0038-databases.md.
package database

import (
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// Engine is a database Dockwell knows, as its official image runs it.
type Engine struct {
	// Kind is the API's name for it.
	Kind dockwellv1.DatabaseEngine
	// Name is what people call it.
	Name string
	// Images are its official images' repositories, as Docker Hub names them.
	Images []string
	// Servers are the commands that run its server, as a service's command names
	// them, such as postgres. A service of its image that runs anything else, such as
	// pg_dump, isn't a database.
	Servers []string
	// Data is where its image keeps its data, and Volume where the image declares the
	// volume that holds it, which Docker makes anonymous unless a mount is there: each
	// for a release of the image. DataVar names a variable that moves the data, where
	// there is one.
	Data, Volume func(major int) string
	DataVar      string
	// DataFlag names an option of its server's command that moves the data, as
	// MongoDB's --dbpath does, where there is one.
	DataFlag string
	// Port is what it listens on unless its command says otherwise.
	Port uint32
	// Password is the variable its image sets its first user's password from, in
	// the order it reads them.
	Password []string
	// User is the variable its image sets its first user's name from, where that
	// isn't always the same user, as MongoDB's root can be anyone.
	User string
	// Oldest is the oldest release Dockwell reads, where an older one's image lacks
	// the tools it runs, and its words for that release. Zero reads any.
	Oldest     int
	OldestName string
}

// Engines are the databases Dockwell knows.
var Engines = []*Engine{
	{
		Kind: dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL, Name: "PostgreSQL", Images: []string{"postgres"},
		Servers: []string{"postgres"}, Data: postgresData, Volume: postgresVolume, DataVar: "PGDATA",
		Port: 5432, Password: []string{"POSTGRES_PASSWORD"},
	},
	{
		Kind: dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL, Name: "MySQL", Images: []string{"mysql"},
		Servers: []string{"mysqld"}, Data: always("/var/lib/mysql"), Volume: always("/var/lib/mysql"),
		Port: 3306, Password: []string{"MYSQL_ROOT_PASSWORD"},
	},
	{
		Kind: dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB, Name: "MariaDB", Images: []string{"mariadb"},
		Servers: []string{"mariadbd", "mysqld"}, Data: always("/var/lib/mysql"), Volume: always("/var/lib/mysql"),
		Port: 3306, Password: []string{"MARIADB_ROOT_PASSWORD", "MYSQL_ROOT_PASSWORD"},
	},
	{
		Kind: dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS, Name: "Redis", Images: []string{"redis"},
		Servers: []string{"redis-server"}, Data: always("/data"), Volume: always("/data"),
		Port: 6379, Password: []string{"REDIS_PASSWORD"},
	},
	{
		Kind: dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY, Name: "Valkey", Images: []string{"valkey/valkey"},
		Servers: []string{"valkey-server", "redis-server"}, Data: always("/data"), Volume: always("/data"),
		Port: 6379, Password: []string{"VALKEY_PASSWORD", "REDIS_PASSWORD"},
	},
	{
		// Its image declares /data/configdb too, which only a config server of a
		// sharded cluster keeps its data in.
		Kind: dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB, Name: "MongoDB", Images: []string{"mongo"},
		Servers: []string{"mongod"}, Data: always("/data/db"), Volume: always("/data/db"), DataFlag: "--dbpath",
		Port: 27017, Password: []string{"MONGO_INITDB_ROOT_PASSWORD"}, User: "MONGO_INITDB_ROOT_USERNAME",
		// Its images have mongosh from 6.0, and the old mongo shell before.
		Oldest: 6, OldestName: "6.0",
	},
}

func always(dir string) func(int) string { return func(int) string { return dir } }

// postgresNewest is the release a tag that names none, such as latest, runs now.
const postgresNewest = 18

// postgresData is where PostgreSQL keeps its data: in the volume its image declares
// up to 17, and in a directory of the release under it from 18, which declares its
// volume one up.
func postgresData(major int) string {
	if major > 0 && major < 18 {
		return "/var/lib/postgresql/data"
	}
	return "/var/lib/postgresql/" + strconv.Itoa(max(major, postgresNewest)) + "/docker"
}

func postgresVolume(major int) string {
	if major > 0 && major < 18 {
		return "/var/lib/postgresql/data"
	}
	return "/var/lib/postgresql"
}

// Major is the release an image's tag names, such as 17 for postgres:17.2-alpine, or
// 0 where it names none.
func Major(image string) int {
	first, _, _ := strings.Cut(Version(image), ".")
	n, err := strconv.Atoi(first)
	if err != nil {
		return 0
	}
	return n
}

// Of is the engine of kind, or nil.
func Of(kind dockwellv1.DatabaseEngine) *Engine {
	for _, e := range Engines {
		if e.Kind == kind {
			return e
		}
	}
	return nil
}

// SQL says the engine speaks SQL.
func (e *Engine) SQL() bool {
	switch e.Kind {
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS, dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY,
		dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB:
		return false
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED, dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL,
		dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL, dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:
	}
	return true
}

// KeyValue says the engine keeps keys and values, as Redis and Valkey do, with no
// databases or users of its own here.
func (e *Engine) KeyValue() bool {
	return e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS || e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY
}

// SignsInWith names the variables its tools sign in with, for people.
func (e *Engine) SignsInWith() string {
	with := strings.Join(e.Password, " or ")
	if e.User != "" {
		with = e.User + " and " + with
	}
	return with
}

// TooOld says why Dockwell can't read a database on an image older than it reads,
// or nothing.
func (e *Engine) TooOld(image string) string {
	if major := Major(image); e.Oldest > 0 && major > 0 && major < e.Oldest {
		return fmt.Sprintf("%s %s is older than Dockwell reads, as its image lacks the tools that ask it. Upgrade it to %s or later",
			e.Name, Version(image), e.OldestName)
	}
	return ""
}

// Repository is an image's repository as Docker Hub names it, without a tag, a
// digest, docker.io or library/: "postgres" for "docker.io/library/postgres:17@sha256:…".
// An image from any other registry keeps its host, so it's no official image.
func Repository(image string) string {
	repo, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	for _, host := range []string{"docker.io/", "index.docker.io/", "registry-1.docker.io/"} {
		repo = strings.TrimPrefix(repo, host)
	}
	return strings.TrimPrefix(repo, "library/")
}

// Tag is an image's tag, or "latest" where it names none.
func Tag(image string) string {
	image, _, _ = strings.Cut(image, "@")
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[i+1:]
	}
	return "latest"
}

// Version is the version an image's tag names, such as "17.2" for
// "postgres:17.2-alpine", or the tag itself where it names none, such as "latest".
func Version(image string) string {
	tag := Tag(image)
	v, _, _ := strings.Cut(tag, "-")
	for _, part := range strings.Split(v, ".") {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return tag
		}
	}
	return v
}

// Recognise is the engine an image runs, or nil for one Dockwell doesn't know.
func Recognise(image string) *Engine {
	repo := Repository(image)
	for _, e := range Engines {
		for _, img := range e.Images {
			if repo == img {
				return e
			}
		}
	}
	return nil
}

// Found is what a service's spec says of the database it runs.
type Found struct {
	Engine *Engine
	// Volume holds its data, by name, and External says Dockwell didn't make it.
	// Empty when nothing does.
	Volume   string
	External bool
	// Port is what it listens on in its container.
	Port uint32
	// PasswordSecret is the environment's secret its first user's password is in,
	// when it uses one.
	PasswordSecret string
}

// FromSpec reads what a service runs as a database, or says it isn't one.
func FromSpec(svc *resourcev1.Service) (Found, bool) {
	app := svc.GetApp()
	if app == nil {
		return Found{}, false
	}
	e := Recognise(app.GetImage())
	if e == nil {
		return Found{}, false
	}
	if !startsServer(e, app) {
		return Found{}, false
	}
	f := Found{Engine: e, Port: e.Port}
	major := Major(app.GetImage())
	dir := e.Data(major)
	moved := false
	if v := app.GetEnv()[e.DataVar]; e.DataVar != "" && v != "" {
		dir, moved = path.Clean(v), true
	}
	if v := FlagValue(app.GetCommand(), e.DataFlag); v != "" {
		dir, moved = path.Clean(v), true
	}
	m := dataMount(app.GetMounts(), dir, e.Volume(major))
	if old := e.Data(1); m == nil && major == 0 && !moved && old != dir {
		// A tag that names no release, such as latest or alpine, may run an older
		// one, so a volume where those keep their data counts too.
		m = dataMount(app.GetMounts(), old, e.Volume(1))
	}
	if m != nil {
		f.Volume, f.External = m.GetVolume(), m.GetExternal()
	}
	if p := PortOf(e, app.GetCommand()); p != 0 {
		f.Port = p
	}
	for _, name := range e.Password {
		if secret, ok := svc.GetSecrets()[name]; ok {
			f.PasswordSecret = secret
			if secret == "" {
				f.PasswordSecret = name
			}
			break
		}
	}
	return f, true
}

// dataMount is the volume a data directory is on: the deepest mount that holds it,
// unless that's above where the image declares its volume, which Docker then makes
// anonymous, beneath it, with the data in. A server path holds data too, but it's no
// volume, so it's left out.
func dataMount(mounts []*resourcev1.Mount, dir, imageVolume string) *resourcev1.Mount {
	var deepest *resourcev1.Mount
	for _, m := range mounts {
		target := path.Clean(m.GetTarget())
		if Holds(target, dir) && (deepest == nil || len(target) > len(path.Clean(deepest.GetTarget()))) {
			deepest = m
		}
	}
	if deepest.GetVolume() == "" {
		return nil
	}
	target := path.Clean(deepest.GetTarget())
	if target != imageVolume && Holds(target, imageVolume) && Holds(imageVolume, dir) {
		return nil
	}
	return deepest
}

// entrypoints are the scripts official images start with, which run the server
// with what follows.
var entrypoints = []string{"docker-entrypoint.sh", "docker-entrypoint"}

// startsServer says a service runs its image's server: with no command of its own,
// with options to the server, or with the server named first, as postgres -c port=5433.
// One that runs anything else, such as a dump, or its own entrypoint, isn't a database.
func startsServer(e *Engine, app *resourcev1.Container) bool {
	if app.GetNoEntrypoint() {
		return false
	}
	if ep := app.GetEntrypoint(); len(ep) > 0 && (len(ep) > 1 || !slices.Contains(entrypoints, path.Base(ep[0]))) {
		return false
	}
	cmd := app.GetCommand()
	if len(cmd) == 0 || strings.HasPrefix(cmd[0], "-") {
		return true
	}
	if shell := path.Base(cmd[0]); (shell == "sh" || shell == "bash") && len(cmd) >= 3 && cmd[1] == "-c" {
		// A script that starts the server, as sh -c 'exec redis-server --requirepass
		// "$REDIS_PASSWORD"' does, by its first word after an exec.
		words := strings.Fields(cmd[2])
		if len(words) > 0 && words[0] == "exec" {
			words = words[1:]
		}
		return len(words) > 0 && (slices.Contains(e.Servers, path.Base(words[0])) || e.startsFromConf(words[0]))
	}
	return slices.Contains(e.Servers, path.Base(cmd[0])) || e.startsFromConf(cmd[0])
}

// startsFromConf says Redis's or Valkey's image starts its server with a first
// argument that's a .conf file, as their entrypoints put the server before it.
func (e *Engine) startsFromConf(arg string) bool {
	return (e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS || e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY) &&
		strings.HasSuffix(arg, ".conf")
}

// FlagValue reads an option's value from a command line, as --dbpath /srv or
// --dbpath=/srv, or nothing. A script's options, as sh -c gives one, are read by
// its words.
func FlagValue(cmd []string, flag string) string {
	if flag == "" {
		return ""
	}
	if len(cmd) >= 3 && (path.Base(cmd[0]) == "sh" || path.Base(cmd[0]) == "bash") && cmd[1] == "-c" {
		cmd = strings.Fields(cmd[2])
	}
	for i, arg := range cmd {
		if v, ok := strings.CutPrefix(arg, flag+"="); ok {
			return v
		}
		if arg == flag && i+1 < len(cmd) {
			return cmd[i+1]
		}
	}
	return ""
}

// Holds says a mount at target has dir on it: it's dir, or dir is under it.
func Holds(target, dir string) bool {
	target = strings.TrimSuffix(path.Clean(target), "/")
	return target == dir || strings.HasPrefix(dir, target+"/")
}

// PortOf reads the port a server's command line sets, or 0: -p 5433, -p5433,
// --port=5433, --port 5433 or -c port=5433.
func PortOf(e *Engine, cmd []string) uint32 {
	for i, arg := range cmd {
		next := ""
		if i+1 < len(cmd) {
			next = cmd[i+1]
		}
		var v string
		switch {
		case arg == "--port":
			v = next
		case arg == "-p" && e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
			v = next
		case arg == "-c" && strings.HasPrefix(next, "port="):
			v = strings.TrimPrefix(next, "port=")
		case strings.HasPrefix(arg, "-cport="):
			v = strings.TrimPrefix(arg, "-cport=")
		case strings.HasPrefix(arg, "--port="):
			v = strings.TrimPrefix(arg, "--port=")
		case strings.HasPrefix(arg, "-p") && len(arg) > 2 && !strings.HasPrefix(arg, "--") && e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
			v = arg[2:]
		default:
			continue
		}
		if n, ok := port(v); ok {
			return n
		}
	}
	return 0
}

func port(s string) (uint32, bool) {
	if s == "" || len(s) > 5 {
		return 0, false
	}
	var n uint32
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint32(c-'0')
	}
	return n, n > 0 && n < 65536
}
