// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"

	"github.com/dockwell/dockwell/internal/lazyre"
)

// LogicalResourceName returns the resource name for a database.logical on svc
// for SQL database dbName: "<svc>-<dbName>" when it fits within 63 characters,
// or a truncated prefix plus an 8-character hash suffix so two engines in the
// same environment can each hold a database with the same SQL name.
func LogicalResourceName(svc, dbName string) string {
	full := svc + "-" + dbName
	if len(full) <= 63 {
		return full
	}
	sum := sha256.Sum256([]byte(svc + "\x00" + dbName))
	suffix := "-" + hex.EncodeToString(sum[:4])
	prefix := full[:63-len(suffix)]
	prefix = strings.TrimRight(prefix, "-_")
	return prefix + suffix
}

var (
	templateVarRe = lazyre.Regexp(`\$\{[^}]*\}`)
	sqlURLRe      = lazyre.Regexp(`(?i)(?:jdbc:)?(?:postgres(?:ql)?|mysql|mariadb)://[^\s'"` + "`" + `<>]+`)
	libpqHostRe   = lazyre.Regexp(`(?i)(?:^|[\s;])host\s*=\s*([^\s;'"]+)`)
	libpqPortRe   = lazyre.Regexp(`(?i)(?:^|[\s;])port\s*=\s*([0-9]+)`)
	libpqDBRe     = lazyre.Regexp(`(?i)(?:^|[\s;])dbname\s*=\s*([^\s;'"]+)`)
)

// ValueNamesDatabase reports whether val (a secret value, template text, or
// service environment value) contains a SQL connection URL or libpq DSN that
// targets database dbName on host svc.
func ValueNamesDatabase(val, svc, dbName string) bool {
	return ValueNamesDatabaseHosts(val, []string{svc}, dbName)
}

// ValueNamesDatabaseHosts reports whether val contains a SQL connection URL or
// libpq DSN targeting database dbName on any of hosts. Each entry in hosts may
// be a bare hostname/alias/container name/IP or "host:port" for a published port.
func ValueNamesDatabaseHosts(val string, hosts []string, dbName string) bool {
	if val == "" || len(hosts) == 0 || dbName == "" {
		return false
	}
	// Replace ${VAR} template placeholders so URL parsing succeeds on templates.
	norm := templateVarRe().ReplaceAllString(val, "placeholder")
	for _, raw := range sqlURLRe().FindAllString(norm, -1) {
		raw = strings.TrimPrefix(raw, "jdbc:")
		raw = strings.TrimPrefix(raw, "JDBC:")
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if !hostMatchesAny(u.Hostname(), u.Port(), hosts) {
			continue
		}
		db := strings.TrimPrefix(u.Path, "/")
		if db == dbName {
			return true
		}
	}
	// Check libpq key=value connection strings (host=<h> [port=<p>] ... dbname=<dbName>).
	hostMatches := libpqHostRe().FindAllStringSubmatch(norm, -1)
	dbMatches := libpqDBRe().FindAllStringSubmatch(norm, -1)
	if len(hostMatches) > 0 && len(dbMatches) > 0 {
		port := ""
		if pm := libpqPortRe().FindStringSubmatch(norm); len(pm) > 1 {
			port = pm[1]
		}
		hostOK := false
		for _, m := range hostMatches {
			if hostMatchesAny(m[1], port, hosts) {
				hostOK = true
				break
			}
		}
		if hostOK {
			for _, m := range dbMatches {
				if m[1] == dbName {
					return true
				}
			}
		}
	}
	return false
}

// ValueReachesHost reports whether val references any host/alias/address in
// hosts, either as a plain host / host:port value, a SQL URL, or a libpq DSN.
func ValueReachesHost(val string, hosts []string) bool {
	val = strings.TrimSpace(val)
	if val == "" || len(hosts) == 0 {
		return false
	}
	// Direct host or host:port value (e.g. DB_HOST=pg or DB_HOST=pg:5432).
	if host, port, ok := splitHostPortLoose(val); ok {
		if hostMatchesAny(host, port, hosts) {
			return true
		}
	}
	norm := templateVarRe().ReplaceAllString(val, "placeholder")
	for _, raw := range sqlURLRe().FindAllString(norm, -1) {
		raw = strings.TrimPrefix(raw, "jdbc:")
		raw = strings.TrimPrefix(raw, "JDBC:")
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if hostMatchesAny(u.Hostname(), u.Port(), hosts) {
			return true
		}
	}
	if hostMatches := libpqHostRe().FindAllStringSubmatch(norm, -1); len(hostMatches) > 0 {
		port := ""
		if pm := libpqPortRe().FindStringSubmatch(norm); len(pm) > 1 {
			port = pm[1]
		}
		for _, m := range hostMatches {
			if hostMatchesAny(m[1], port, hosts) {
				return true
			}
		}
	}
	return false
}

// ValueNamesOtherDatabase reports whether val explicitly names a specific
// database on one of hosts that is NOT dbName (e.g. another database on the
// same engine).
func ValueNamesOtherDatabase(val string, hosts []string, dbName string) bool {
	if val == "" || len(hosts) == 0 {
		return false
	}
	norm := templateVarRe().ReplaceAllString(val, "placeholder")
	for _, raw := range sqlURLRe().FindAllString(norm, -1) {
		raw = strings.TrimPrefix(raw, "jdbc:")
		raw = strings.TrimPrefix(raw, "JDBC:")
		u, err := url.Parse(raw)
		if err != nil || !hostMatchesAny(u.Hostname(), u.Port(), hosts) {
			continue
		}
		db := strings.TrimPrefix(u.Path, "/")
		if db != "" && db != "placeholder" && db != dbName {
			return true
		}
	}
	hostMatches := libpqHostRe().FindAllStringSubmatch(norm, -1)
	dbMatches := libpqDBRe().FindAllStringSubmatch(norm, -1)
	if len(hostMatches) > 0 && len(dbMatches) > 0 {
		port := ""
		if pm := libpqPortRe().FindStringSubmatch(norm); len(pm) > 1 {
			port = pm[1]
		}
		for _, hm := range hostMatches {
			if hostMatchesAny(hm[1], port, hosts) {
				for _, dm := range dbMatches {
					if dm[1] != "" && dm[1] != "placeholder" && dm[1] != dbName {
						return true
					}
				}
			}
		}
	}
	return false
}

// IsDBNameEnvKey reports whether an environment variable name conventionally
// holds a SQL database or schema name (such as DB_NAME, POSTGRES_DB,
// MYSQL_DATABASE, PGDATABASE, or *_DB_NAME).
func IsDBNameEnvKey(key string) bool {
	u := strings.ToUpper(strings.TrimSpace(key))
	switch u {
	case "DB", "DB_NAME", "DB_DATABASE", "DB_SCHEMA", "DATABASE", "DATABASE_NAME",
		"POSTGRES_DB", "POSTGRES_DATABASE", "PGDATABASE", "PG_DATABASE", "PG_DB",
		"MYSQL_DATABASE", "MYSQL_DB", "MARIADB_DATABASE", "MARIADB_DB",
		"SQL_DATABASE", "SQL_DB":
		return true
	}
	return strings.HasSuffix(u, "_DB_NAME") ||
		strings.HasSuffix(u, "_DATABASE_NAME") ||
		strings.HasSuffix(u, "_DATABASE") ||
		strings.HasSuffix(u, "_POSTGRES_DB") ||
		strings.HasSuffix(u, "_MYSQL_DB") ||
		strings.HasSuffix(u, "_MARIADB_DB") ||
		strings.HasSuffix(u, "_PGDATABASE")
}

func splitHostPortLoose(val string) (host, port string, ok bool) {
	if strings.ContainsAny(val, " \t\r\n/") {
		return "", "", false
	}
	if strings.HasPrefix(val, "[") {
		end := strings.IndexByte(val, ']')
		if end <= 1 {
			return "", "", false
		}
		h := val[1:end]
		rest := val[end+1:]
		if rest == "" {
			return h, "", true
		}
		if strings.HasPrefix(rest, ":") {
			return h, rest[1:], true
		}
		return "", "", false
	}
	if strings.Count(val, ":") == 1 {
		h, p, _ := strings.Cut(val, ":")
		if h != "" && p != "" && strings.Trim(p, "0123456789") == "" {
			return h, p, true
		}
		return "", "", false
	}
	if strings.Contains(val, ":") {
		// Bare IPv6 address.
		return val, "", true
	}
	return val, "", true
}

func hostMatchesAny(host, port string, candidates []string) bool {
	if host == "" {
		return false
	}
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		ch, cp, ok := splitHostPortLoose(c)
		if !ok {
			continue
		}
		if cp != "" {
			// A host:port candidate (published port) matches only when both host and port match.
			if strings.EqualFold(host, ch) && port == cp {
				return true
			}
			continue
		}
		if strings.EqualFold(host, ch) {
			return true
		}
	}
	return false
}
