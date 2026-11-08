// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// parse reads what the engine's reading printed.
func (e *Engine) parse(said []byte) (*dockwellv1.DatabaseReading, error) {
	switch e.Kind {
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		return parsePostgres(said)
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL, dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		return parseMySQL(e, said)
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB:
		return parseMongo(said)
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED, dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS,
		dockwellv1.DatabaseEngine_DATABASE_ENGINE_VALKEY:
	}
	return parseRedis(e, said)
}

// postgresSaid is what postgresRead prints.
type postgresSaid struct {
	Version        string `json:"version"`
	User           string `json:"user"`
	Connections    int64  `json:"connections"`
	MaxConnections int64  `json:"max_connections"`
	Uptime         int64  `json:"uptime"`
	Hit            int64  `json:"hit"`
	Read           int64  `json:"read"`
	Size           int64  `json:"size"`
	DatabaseCount  int    `json:"database_count"`
	UserCount      int    `json:"user_count"`
	Databases      []struct {
		Name  string `json:"name"`
		Owner string `json:"owner"`
		Size  int64  `json:"size"`
	} `json:"databases"`
	Users []struct {
		Name  string `json:"name"`
		Super bool   `json:"super"`
		Login bool   `json:"login"`
	} `json:"users"`
}

func parsePostgres(said []byte) (*dockwellv1.DatabaseReading, error) {
	var s postgresSaid
	if err := json.Unmarshal(bytes.TrimSpace(said), &s); err != nil {
		return nil, fmt.Errorf("it isn't the JSON asked for: %w", err)
	}
	r := &dockwellv1.DatabaseReading{
		Version: firstWord(s.Version), User: s.User, SizeBytes: s.Size,
		Connections: s.Connections, MaxConnections: s.MaxConnections, UptimeSeconds: s.Uptime,
		CacheHit: share(s.Hit, s.Read),
	}
	owns := map[string][]string{}
	for _, d := range s.Databases {
		r.Databases = append(r.Databases, &dockwellv1.InnerDatabase{Name: d.Name, Owner: d.Owner, SizeBytes: d.Size, System: d.Name == "postgres"})
		owns[d.Owner] = append(owns[d.Owner], d.Name)
	}
	for _, u := range s.Users {
		r.Users = append(r.Users, &dockwellv1.DatabaseAccount{Name: u.Name, Superuser: u.Super, CanSignIn: u.Login, Databases: owns[u.Name]})
	}
	r.Unlisted = int32(max(0, s.DatabaseCount-len(s.Databases)) + max(0, s.UserCount-len(s.Users))) //nolint:gosec // two counts of at most what a server holds
	return r, nil
}

// mongoSaid is what mongoRead prints.
type mongoSaid struct {
	Version        string `json:"version"`
	User           string `json:"user"`
	Uptime         int64  `json:"uptime"`
	Connections    int64  `json:"connections"`
	MaxConnections int64  `json:"max_connections"`
	Requested      int64  `json:"requested"`
	Read           int64  `json:"read"`
	Size           int64  `json:"size"`
	FCV            string `json:"fcv"`
	ReplicaSet     string `json:"replica_set"`
	Auth           bool   `json:"auth"`
	DatabaseCount  int    `json:"database_count"`
	UserCount      int    `json:"user_count"`
	Databases      []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"databases"`
	Users []struct {
		Name  string `json:"name"`
		DB    string `json:"db"`
		Roles []struct {
			Role string `json:"role"`
			DB   string `json:"db"`
		} `json:"roles"`
	} `json:"users"`
}

// mongoSystem are the databases MongoDB keeps for itself.
var mongoSystem = []string{"admin", "config", "local"}

// mongoDatabaseRoles are the roles that make a database a user's, in the database
// they name.
var mongoDatabaseRoles = []string{"dbOwner", "readWrite", "read", "dbAdmin", "userAdmin"}

func parseMongo(said []byte) (*dockwellv1.DatabaseReading, error) {
	// mongosh can print a warning first; the reading is its last line.
	lines := bytes.Split(bytes.TrimSpace(said), []byte("\n"))
	var s mongoSaid
	if err := json.Unmarshal(lines[len(lines)-1], &s); err != nil {
		return nil, fmt.Errorf("it isn't the JSON asked for: %w", err)
	}
	if s.Version == "" {
		return nil, errors.New("it didn't say its version")
	}
	read := min(max(s.Read, 0), max(s.Requested, 0))
	r := &dockwellv1.DatabaseReading{
		Version: s.Version, User: s.User, SizeBytes: s.Size,
		Connections: s.Connections, MaxConnections: s.MaxConnections, UptimeSeconds: s.Uptime,
		CacheHit: share(max(s.Requested, 0)-read, read), FeatureCompatibility: s.FCV, ReplicaSet: s.ReplicaSet, ChecksNoOne: !s.Auth,
	}
	for _, d := range s.Databases {
		r.Databases = append(r.Databases, &dockwellv1.InnerDatabase{Name: d.Name, SizeBytes: d.Size, System: slices.Contains(mongoSystem, d.Name)})
	}
	for _, u := range s.Users {
		a := &dockwellv1.DatabaseAccount{Name: u.Name, AuthDatabase: u.DB, CanSignIn: true}
		for _, role := range u.Roles {
			switch {
			case role.Role == "root" || role.Role == "__system":
				a.Superuser = true
			case slices.Contains(mongoDatabaseRoles, role.Role) && !slices.Contains(a.Databases, role.DB):
				a.Databases = append(a.Databases, role.DB)
			}
		}
		r.Users = append(r.Users, a)
	}
	r.Unlisted = int32(max(0, s.DatabaseCount-len(s.Databases)) + max(0, s.UserCount-len(s.Users))) //nolint:gosec // two counts of at most what a server holds
	return r, nil
}

// share is the part of reads that hit, or nil before any.
func share(hit, missed int64) *float64 {
	if hit+missed <= 0 {
		return nil
	}
	return proto.Float64(float64(hit) / float64(hit+missed))
}

// firstWord is "17.2" of "17.2 (Debian 17.2-1.pgdg120+1)".
func firstWord(s string) string {
	word, _, _ := strings.Cut(strings.TrimSpace(s), " ")
	return word
}

// mysqlSystem are the schemas MySQL and MariaDB keep for themselves.
var mysqlSystem = []string{"information_schema", "mysql", "performance_schema", "sys"}

// mysqlSystemUser says a user is one MySQL, MariaDB or its image made for itself.
func mysqlSystemUser(e *Engine, name string) bool {
	return strings.HasPrefix(name, "mysql.") || name == "mariadb.sys" ||
		e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB && name == "healthcheck"
}

func parseMySQL(e *Engine, said []byte) (*dockwellv1.DatabaseReading, error) {
	r := &dockwellv1.DatabaseReading{}
	status := map[string]int64{}
	byUser := map[string]*dockwellv1.DatabaseAccount{}
	seenVersion := false
	databases, users := 0, 0
	sc := bufio.NewScanner(bytes.NewReader(said))
	sc.Buffer(make([]byte, 64<<10), maxSaid)
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		for i := range f {
			f[i] = unescapeBatch(f[i])
		}
		switch {
		case f[0] == "v" && len(f) == 4:
			seenVersion = true
			r.Version = mysqlVersion(f[1])
			r.User, _, _ = strings.Cut(f[2], "@")
			r.MaxConnections, _ = strconv.ParseInt(f[3], 10, 64)
		case f[0] == "d" && len(f) == 3:
			size, _ := strconv.ParseInt(f[2], 10, 64)
			if slices.Contains(mysqlSystem, f[1]) && f[1] != "mysql" && f[1] != "sys" {
				// They hold nothing on disk, whatever size they say.
				continue
			}
			r.SizeBytes += size
			databases++
			if len(r.Databases) < MaxListed {
				r.Databases = append(r.Databases, &dockwellv1.InnerDatabase{Name: f[1], SizeBytes: size, System: slices.Contains(mysqlSystem, f[1])})
			}
		case f[0] == "u" && len(f) == 4:
			users++
			u := &dockwellv1.DatabaseAccount{Name: f[1], Host: f[2], Superuser: f[3] == "Y", CanSignIn: true, System: mysqlSystemUser(e, f[1])}
			byUser[f[1]+"@"+f[2]] = u
			if len(r.Users) < MaxListed {
				r.Users = append(r.Users, u)
			}
		case f[0] == "g" && len(f) == 4:
			if u := byUser[f[1]+"@"+f[2]]; u != nil && !slices.Contains(u.Databases, f[3]) {
				u.Databases = append(u.Databases, f[3])
			}
		case len(f) == 2:
			n, err := strconv.ParseInt(f[1], 10, 64)
			if err == nil {
				status[f[0]] = n
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if !seenVersion {
		return nil, errors.New("it didn't say its version")
	}
	// Of those connected, one is this reading.
	r.Connections = max(0, status["Threads_connected"]-1)
	r.UptimeSeconds = status["Uptime"]
	requests, reads := status["Innodb_buffer_pool_read_requests"], status["Innodb_buffer_pool_reads"]
	r.CacheHit = share(requests-min(reads, requests), min(reads, requests))
	r.Unlisted = int32(max(0, databases-len(r.Databases)) + max(0, users-len(r.Users)))
	return r, nil
}

// mysqlVersion is "11.4.4" of "11.4.4-MariaDB-ubu2404", and "8.4.3" as it is.
func mysqlVersion(v string) string {
	v, _, _ = strings.Cut(v, "-")
	return v
}

// unescapeBatch undoes what batch mode does to a value: \t, \n, \\ and \0.
func unescapeBatch(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case '0':
			b.WriteByte(0)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func parseRedis(e *Engine, said []byte) (*dockwellv1.DatabaseReading, error) {
	info := map[string]string{}
	r := &dockwellv1.DatabaseReading{}
	sc := bufio.NewScanner(bytes.NewReader(said))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		info[k] = v
		// db0:keys=12,expires=0,avg_ttl=0
		if strings.HasPrefix(k, "db") {
			for _, part := range strings.Split(v, ",") {
				if n, ok := strings.CutPrefix(part, "keys="); ok {
					keys, _ := strconv.ParseInt(n, 10, 64)
					r.Keys += keys
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	r.Version = info["valkey_version"]
	if r.Version == "" || e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_REDIS {
		r.Version = info["redis_version"]
	}
	if r.Version == "" {
		return nil, errors.New("it didn't say its version")
	}
	num := func(k string) int64 {
		n, _ := strconv.ParseInt(info[k], 10, 64)
		return n
	}
	r.SizeBytes = num("used_memory")
	// Of those connected, one is this reading.
	r.Connections = max(0, num("connected_clients")-1)
	r.MaxConnections = num("maxclients")
	r.UptimeSeconds = num("uptime_in_seconds")
	r.CacheHit = share(num("keyspace_hits"), num("keyspace_misses"))
	return r, nil
}
