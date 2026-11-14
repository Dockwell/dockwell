// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
)

// An app's own user on Redis 6.2 and later, or Valkey: an ACL user, signed in with a
// password of its own, while the default user's password stays the administrator's
// (ADR 0084). It may do what an app does, never administer the server (@admin:
// CONFIG, ACL, SHUTDOWN, REPLICAOF and the like). On a server other apps share it may
// do only what an app does with its data, by an allowlist of categories, and nothing
// that reaches past the keys it works with (@dangerous: KEYS, FLUSHALL and the like).
// Where its need sets a key prefix, it reaches only keys and channels under it, so
// apps that share a server keep apart; otherwise it reaches every key, which a shared
// server's choice says. The password goes to the server as its SHA-256, as Redis keeps
// it, never as itself, so no process list shows it. A server that requires no
// password, or is older than 6.2, which has no channel rules, gets no user.
//
// Redis forgets users made as it runs when it restarts, unless it keeps an ACL file,
// which the official images don't. So where it keeps one, the user is saved there,
// and in any case the agent keeps each user's rules, with that hash, and puts them
// back when the server starts again (KeepRedisUsers).

// redisCommands runs redis-cli, or valkey-cli, on the commands it's given on stdin,
// one a line, signed in as the default user, as the image's environment has it. A
// password only the server's command line requires goes first on stdin
// (redisAsk), never as an argument.
var redisCommands = []string{"sh", "-c", `p="${VALKEY_PASSWORD:-${REDIS_PASSWORD:-${REDISCLI_AUTH:-}}}"; [ -n "$p" ] && export REDISCLI_AUTH="$p"; c=redis-cli; command -v valkey-cli >/dev/null 2>&1 && c=valkey-cli; exec timeout 7 "$c" -p "${DW_PORT:-6379}"`}

// redisQuoted is s as redis-cli reads a quoted argument.
func redisQuoted(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`).Replace(s) + `"`
}

// redisShared is what a user of a server other apps share may do: what an app does
// with its data, and its connection, and nothing that administers the server or
// reaches past the keys it works with, whatever else a category takes in. Past the
// keys are also key names it would read without its pattern (SCAN, RANDOMKEY),
// others' channels (PUBSUB's lists), and the server's functions and scripts, which
// run with whoever calls them's rights, so one app could replace another's.
const redisShared = "-@all +@connection +@read +@write +@keyspace +@pubsub +@transaction +@scripting -@admin -@dangerous -scan -randomkey"

// redisServer is what a Redis or Valkey says of itself that its users' rules follow.
type redisServer struct {
	// subcommands says it takes a rule for one subcommand of a command, as 7 does:
	// before, the whole command goes.
	subcommands bool
	// search says it runs the query engine, whose indexes are no keys, so a key
	// pattern doesn't keep one app out of another's.
	search bool
}

// redisRules is what an app's ACL user may do, by its password's hash: under its key
// prefix where it has one, or every key.
func redisRules(hash string, shared bool, prefix string, srv redisServer) string {
	keys := "~* &*"
	if prefix != "" {
		keys = "~" + prefix + "* &" + prefix + "*"
	}
	rules := "reset on #" + hash + " " + keys
	if !shared {
		return rules + " +@all -@admin"
	}
	rules += " " + redisShared
	if srv.subcommands {
		rules += " -function -script|flush -script|kill -pubsub|channels -pubsub|numsub -pubsub|shardchannels -pubsub|shardnumsub"
	} else {
		rules += " -script -pubsub"
	}
	if srv.search {
		rules += " -@search"
	}
	return rules
}

// redisInfo reads what the server says of itself: its users, its version and its
// modules. refuse says to refuse one an app's user would mean nothing on: any user
// that asks no password, as the default does when the server requires none, since
// any app on its network signs in as it; or a Redis before 6.2, which has no channel
// rules, so a user reaches every other's messages.
func (d *UserDriver) redisInfo(ctx context.Context, c *docker.Container, e *Engine, refuse bool) (redisServer, error) {
	srv := redisServer{subcommands: e.Kind != dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS}
	said, err := d.redisAsk(ctx, c, e, "ACL LIST\nINFO server\nMODULE LIST\n")
	if err != nil {
		return srv, err
	}
	for _, line := range said {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		switch {
		case len(fields) >= 2 && fields[0] == "user" && slices.Contains(fields, "on") && slices.Contains(fields, "nopass"):
			if refuse && fields[1] == "default" {
				return srv, fmt.Errorf("%s asks no one for a password, so any app on its network signs in as its administrator, and a user of its own keeps no one out. Give it a password (requirepass), then try again", e.Name)
			}
			if refuse {
				return srv, fmt.Errorf("%s has a user %s that asks no password, so any app on its network signs in as it, and a user of its own keeps no one out. Give %s a password, or turn it off, then try again", e.Name, fields[1], fields[1])
			}
		case strings.HasPrefix(line, "redis_version:") && e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS:
			v := strings.TrimPrefix(line, "redis_version:")
			n := versionNumbers(v)
			srv.subcommands = len(n) > 0 && n[0] >= 7
			if refuse && len(n) >= 2 && (n[0] < 6 || n[0] == 6 && n[1] < 2) {
				return srv, fmt.Errorf("this Redis is %s, which has no rules for channels, so a user of its own would read every app's messages. Upgrade it to 6.2 or later, then try again", v)
			}
		case line == "search" || line == "ft":
			srv.search = true
		}
	}
	return srv, nil
}

// versionNumbers are a version's numbers, such as 6 and 2 for 6.2.14.
func versionNumbers(v string) []int {
	var out []int
	for _, f := range strings.Split(strings.TrimSpace(v), ".") {
		n, err := strconv.Atoi(f)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

// redisHash is a password as Redis keeps it.
func redisHash(password string) string {
	sum := sha256.Sum256([]byte(password))
	return hex.EncodeToString(sum[:])
}

// redisAsk runs commands on a Redis or Valkey, and refuses what it said back as an
// error, which redis-cli prints as a line of its own.
func (d *UserDriver) redisAsk(ctx context.Context, c *docker.Container, e *Engine, commands string) ([]string, error) {
	// A password only its command line requires, as no variable of its image holds it.
	authed := false
	if pw := FlagValue(c.Config.Cmd, "--requirepass"); pw != "" {
		commands, authed = "AUTH "+redisQuoted(pw)+"\n"+commands, true
	}
	said, err := Reader{Client: d.Client}.ask(ctx, c.ID, e, withPort("DW_PORT", PortOf(e, c.Config.Cmd), redisCommands), commands)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(said), "\n"), "\n")
	if authed && len(lines) > 0 && strings.TrimSpace(lines[0]) == "OK" {
		lines = lines[1:]
	}
	for _, line := range lines {
		// A server reading its data back after a start answers that it's loading.
		if strings.HasPrefix(line, "LOADING ") {
			return nil, fmt.Errorf("%s %w", e.Name, ErrStarting)
		}
		for _, refused := range []string{"ERR unknown command 'ACL'", "ERR unknown command `ACL`"} {
			if strings.HasPrefix(line, refused) {
				return nil, fmt.Errorf("%s keeps one password before 6, so an app can't have a user of its own on it", e.Name)
			}
		}
		for _, refused := range []string{"ERR", "NOPERM", "WRONGPASS", "NOAUTH", "WRONGTYPE"} {
			if strings.HasPrefix(line, refused+" ") {
				return nil, fmt.Errorf("%s refused it: %s", e.Name, line)
			}
		}
	}
	return lines, nil
}

// redisSet gives user its rules, and saves them where the server keeps an ACL file.
func (d *UserDriver) redisSet(ctx context.Context, c *docker.Container, e *Engine, user, rules string) error {
	said, err := d.redisAsk(ctx, c, e, "ACL SETUSER "+user+" "+rules+"\nCONFIG GET aclfile\n")
	if err != nil {
		return err
	}
	// CONFIG GET answers its name, then its value: a path where it keeps a file.
	if len(said) >= 3 && strings.TrimSpace(said[2]) != "" {
		if _, err := d.redisAsk(ctx, c, e, "ACL SAVE\n"); err != nil {
			return fmt.Errorf("%s has the user %s, but can't save it to its ACL file: %w", e.Name, user, err)
		}
	}
	return nil
}

// redisHas says whether the server has user.
func (d *UserDriver) redisHas(ctx context.Context, c *docker.Container, e *Engine, user string) (bool, error) {
	said, err := d.redisAsk(ctx, c, e, "ACL GETUSER "+user+"\n")
	if err != nil {
		return false, err
	}
	// A user it has comes back as its fields, flags first; one it hasn't, as nothing.
	return len(said) > 1 || len(said) == 1 && strings.TrimSpace(said[0]) != "", nil
}

// redisDrop takes the user away, and saves that where the server keeps an ACL file.
func (d *UserDriver) redisDrop(ctx context.Context, c *docker.Container, e *Engine, user string) error {
	said, err := d.redisAsk(ctx, c, e, "ACL DELUSER "+user+"\nCONFIG GET aclfile\n")
	if err != nil {
		return err
	}
	if len(said) >= 3 && strings.TrimSpace(said[2]) != "" {
		_, err = d.redisAsk(ctx, c, e, "ACL SAVE\n")
	}
	return err
}

// redisEvery is how often the agent looks for a Redis or Valkey that started again
// without the users it gave it.
const redisEvery = 15 * time.Second

// KeepRedisUsers puts back, until ctx is done, each Redis or Valkey user the agent
// made, by the rules and hash it kept, on a server that started again without it.
// It looks at a server only once it has started since it last did.
func (d *UserDriver) KeepRedisUsers(ctx context.Context) {
	if d.Client == nil {
		return
	}
	started := map[string]time.Time{} // by container ID
	tick := time.NewTicker(redisEvery)
	defer tick.Stop()
	for {
		d.putRedisUsersBack(ctx, started)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// putRedisUsersBack is one look of KeepRedisUsers'. It holds the Redis lock for
// each user, and reads its record again under it, so a user a step drops meanwhile
// isn't put back. A server it couldn't sign in to, or that was still starting, it
// looks at again next time; what it forgets of servers that are gone.
func (d *UserDriver) putRedisUsersBack(ctx context.Context, started map[string]time.Time) {
	d.mu.Lock()
	all, err := d.records()
	d.mu.Unlock()
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, key := range slices.Sorted(maps.Keys(all)) {
		g := all[key]
		if g.Redis == "" || g.Env == "" {
			continue
		}
		c, err := d.Client.ContainerOf(ctx, docker.Project(g.Env), g.Service, 1)
		if err != nil || !c.State.Running {
			continue
		}
		seen[c.ID] = true
		if started[c.ID].Equal(c.State.StartedAt) {
			continue
		}
		e := Recognise(c.Config.Image)
		if e == nil {
			continue
		}
		if err := d.putRedisUserBack(ctx, c, e, key); err == nil {
			started[c.ID] = c.State.StartedAt
		}
	}
	for id := range started {
		if !seen[id] {
			delete(started, id)
		}
	}
}

// putRedisUserBack gives a server the user its record keeps, if the record still
// keeps it and the server hasn't it.
func (d *UserDriver) putRedisUserBack(ctx context.Context, c *docker.Container, e *Engine, key string) error {
	d.redisMu.Lock()
	defer d.redisMu.Unlock()
	d.mu.Lock()
	all, err := d.records()
	d.mu.Unlock()
	if err != nil {
		return err
	}
	g, ok := all[key]
	if !ok || g.Redis == "" {
		return nil // dropped meanwhile
	}
	has, err := d.redisHas(ctx, c, e, g.User)
	if err == nil && !has {
		err = d.redisSet(ctx, c, e, g.User, g.Redis)
	}
	return err
}
