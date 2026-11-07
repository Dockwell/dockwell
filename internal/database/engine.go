// SPDX-License-Identifier: AGPL-3.0-only

// Package database knows the databases Dockwell can read and manage from inside
// their own containers: PostgreSQL, MySQL, MariaDB, Redis and Valkey, from their
// official images. The control plane uses it to tell a database from any other
// service by its spec, and the agent to ask one what it holds, with the database's
// own tools, which sign in with what its environment already has, so no password
// passes through Dockwell. See docs/adr/0038-databases.md.
package database

import (
	"path"
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
	// Data are where its image keeps its data, the first being where a volume
	// usually goes. DataVar names a variable that moves it, where there is one.
	Data    []string
	DataVar string
	// Port is what it listens on unless its command says otherwise.
	Port uint32
	// Password is the variable its image sets its first user's password from, in
	// the order it reads them.
	Password []string
}

// Engines are the databases Dockwell knows.
var Engines = []*Engine{
	{
		Kind: dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL, Name: "PostgreSQL", Images: []string{"postgres"},
		// postgres:18 keeps its data under /var/lib/postgresql/18/docker, and asks for
		// the volume one up.
		Data: []string{"/var/lib/postgresql/data", "/var/lib/postgresql"}, DataVar: "PGDATA",
		Port: 5432, Password: []string{"POSTGRES_PASSWORD"},
	},
	{
		Kind: dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL, Name: "MySQL", Images: []string{"mysql"},
		Data: []string{"/var/lib/mysql"}, Port: 3306, Password: []string{"MYSQL_ROOT_PASSWORD"},
	},
	{
		Kind: dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB, Name: "MariaDB", Images: []string{"mariadb"},
		Data: []string{"/var/lib/mysql"}, Port: 3306, Password: []string{"MARIADB_ROOT_PASSWORD", "MYSQL_ROOT_PASSWORD"},
	},
	{
		Kind: dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS, Name: "Redis", Images: []string{"redis"},
		Data: []string{"/data"}, Port: 6379, Password: []string{"REDIS_PASSWORD"},
	},
	{
		Kind: dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY, Name: "Valkey", Images: []string{"valkey/valkey"},
		Data: []string{"/data"}, Port: 6379, Password: []string{"VALKEY_PASSWORD", "REDIS_PASSWORD"},
	},
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

// SQL says the engine speaks SQL, so it has databases and users of its own.
func (e *Engine) SQL() bool {
	switch e.Kind {
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS, dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY:
		return false
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED, dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL,
		dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL, dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:
	}
	return true
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
	f := Found{Engine: e, Port: e.Port}
	dirs := e.Data
	if v := app.GetEnv()[e.DataVar]; e.DataVar != "" && v != "" {
		dirs = []string{path.Clean(v)}
	}
	if m := dataMount(app.GetMounts(), dirs); m != nil {
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

// dataMount is the volume a data directory is on: the deepest of those that hold
// one of dirs. A server path holds data too, but it's no volume, so it's left out.
func dataMount(mounts []*resourcev1.Mount, dirs []string) *resourcev1.Mount {
	var deepest *resourcev1.Mount
	for _, m := range mounts {
		target := path.Clean(m.GetTarget())
		held := false
		for _, dir := range dirs {
			held = held || Holds(target, dir)
		}
		if held && (deepest == nil || len(target) > len(path.Clean(deepest.GetTarget()))) {
			deepest = m
		}
	}
	if deepest.GetVolume() == "" {
		return nil
	}
	return deepest
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
