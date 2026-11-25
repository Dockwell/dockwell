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
	libpqDBRe     = lazyre.Regexp(`(?i)(?:^|[\s;])dbname\s*=\s*([^\s;'"]+)`)
)

// ValueNamesDatabase reports whether val (a secret value, template text, or
// service environment value) contains a SQL connection URL or libpq DSN that
// targets database dbName on host svc.
func ValueNamesDatabase(val, svc, dbName string) bool {
	if val == "" || svc == "" || dbName == "" {
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
		if !strings.EqualFold(u.Hostname(), svc) {
			continue
		}
		db := strings.TrimPrefix(u.Path, "/")
		if db == dbName {
			return true
		}
	}
	// Check libpq key=value connection strings (host=<svc> ... dbname=<dbName>).
	hostMatches := libpqHostRe().FindAllStringSubmatch(norm, -1)
	dbMatches := libpqDBRe().FindAllStringSubmatch(norm, -1)
	if len(hostMatches) > 0 && len(dbMatches) > 0 {
		hostOK := false
		for _, m := range hostMatches {
			if strings.EqualFold(m[1], svc) {
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
