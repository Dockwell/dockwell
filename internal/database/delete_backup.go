// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// deletedKeepFor matches volume.KeepFor (7 days): a deleted SQL database's
// before-drop backup is kept on the server for 7 days so Undo brings its data
// back.
const (
	deletedKeepFor  = 7 * 24 * time.Hour
	deletedKeepPast = time.Hour
	deletedPruneHit = time.Hour
)

type deletedBackupMeta struct {
	ChangeID string                    `json:"change_id"`
	Scope    string                    `json:"scope"`
	Service  string                    `json:"service"`
	Database string                    `json:"database"`
	Engine   dockwellv1.DatabaseEngine `json:"engine"`
	Owner    string                    `json:"owner,omitempty"`
	TakenAt  time.Time                 `json:"taken_at"`
	Size     int64                     `json:"size"`
	SHA256   string                    `json:"sha256"`
}

func deletedBackupsDir(dir string) string {
	return filepath.Join(dir, "deleted")
}

func deletedBackupKey(changeID, scope, service, db string) string {
	sum := sha256.Sum256([]byte(changeID + "\x00" + scope + "\x00" + service + "\x00" + db))
	return hex.EncodeToString(sum[:12])
}

func deletedBackupPaths(dir, changeID, scope, service, db string) (metaPath, dataPath string) {
	base := filepath.Join(deletedBackupsDir(dir), deletedBackupKey(changeID, scope, service, db))
	return base + ".json", base + ".sql.gz"
}

// activeDatabaseUsers asks the running SQL server which users currently have
// active sessions connected to db.
func activeDatabaseUsers(ctx context.Context, client *docker.Client, c *docker.Container, e *Engine, db string) ([]string, error) {
	if err := CheckSQLName("database", db); err != nil {
		return nil, err
	}
	cmd, _ := e.reading(PortOf(e, c.Config.Cmd))
	var sql string
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		sql = "SELECT DISTINCT usename FROM pg_stat_activity WHERE datname = '" + db + "' AND pid <> pg_backend_pid() AND usename IS NOT NULL ORDER BY usename;\n"
	} else {
		sql = "SELECT DISTINCT USER FROM information_schema.PROCESSLIST WHERE DB = '" + db + "' AND ID <> CONNECTION_ID() AND USER IS NOT NULL ORDER BY USER;\n"
	}
	said, err := Reader{Client: client}.ask(ctx, c.ID, e, cmd, sql)
	if err != nil {
		return nil, err
	}
	var users []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(said)), "\n") {
		u := strings.TrimSpace(line)
		if u != "" && !slices.Contains(users, u) {
			users = append(users, u)
		}
	}
	slices.Sort(users)
	return users, nil
}

func dumpCompleteMarker(kind dockwellv1.DatabaseEngine) string {
	if kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		return "PostgreSQL database dump complete"
	}
	return "Dump completed"
}

// backupBeforeDrop dumps db from c into dir/deleted/ keyed by the deleting
// change's ID before DROP DATABASE runs. If the dump fails or doesn't finish
// cleanly, it returns an error so the caller refuses to drop the database.
func backupBeforeDrop(ctx context.Context, client *docker.Client, dir string, now time.Time, changeID string, c *docker.Container, e *Engine, scope, service, db, owner string) error {
	if dir == "" || !e.SQL() || changeID == "" {
		return nil
	}
	dumpCmd := dumpOne[e.Kind]
	if dumpCmd == nil {
		return fmt.Errorf("backing up %s before deleting isn't supported for %s, so nothing was dropped", db, e.Name)
	}
	cmd := append(withPort("PGPORT", PortOf(e, c.Config.Cmd), slices.Clone(dumpCmd)), db)
	if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
		pgOwner, err := OwnerOf(ctx, client, c, e, db)
		if err != nil {
			// If the database doesn't exist anymore on the server, there's nothing to dump.
			if strings.Contains(err.Error(), "has no database") {
				return nil
			}
			return fmt.Errorf("can't read who owns %s on %s before deleting it, so nothing was dropped: %w", db, service, err)
		}
		if pgOwner == "" {
			pgOwner = owner
		}
		if pgOwner == "" {
			pgOwner = "postgres"
		}
		cmd = append(cmd, pgOwner)
		owner = pgOwner
	}

	if err := os.MkdirAll(deletedBackupsDir(dir), 0o700); err != nil {
		return fmt.Errorf("can't make a place to back up %s before deleting it, so nothing was dropped: %w", db, err)
	}
	metaPath, dataPath := deletedBackupPaths(dir, changeID, scope, service, db)
	tmpData := dataPath + ".tmp"
	f, err := os.OpenFile(tmpData, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // under agent databases dir
	if err != nil {
		return fmt.Errorf("can't create backup file for %s before deleting it, so nothing was dropped: %w", db, err)
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmpData)
	}

	hash := sha256.New()
	gz, err := gzip.NewWriterLevel(io.MultiWriter(f, hash), gzip.BestSpeed)
	if err != nil {
		cleanup()
		return err
	}
	tailBuf := &ringTail{max: 4 << 10}
	errs := &capped{max: 4 << 10}
	resource.Progress(ctx, "Backing up "+db+" before deleting (kept for 7 days)")
	code, err := client.Exec(ctx, c.ID, cmd, docker.Streams{
		Stdin:  strings.NewReader(""),
		Stdout: io.MultiWriter(gz, tailBuf),
		Stderr: errs,
	})
	if err != nil {
		_ = gz.Close()
		cleanup()
		return fmt.Errorf("can't back up %s before deleting it, so nothing was dropped: %w", db, err)
	}
	if code != 0 {
		_ = gz.Close()
		cleanup()
		said := scrubbed(strings.TrimSpace(errs.String()))
		if said != "" {
			return fmt.Errorf("can't back up %s before deleting it, so nothing was dropped (%s exited with code %d: %s)", db, e.Name, code, said)
		}
		return fmt.Errorf("can't back up %s before deleting it, so nothing was dropped (%s exited with code %d)", db, e.Name, code)
	}
	if err := gz.Close(); err != nil {
		cleanup()
		return fmt.Errorf("can't finish backing up %s before deleting it, so nothing was dropped: %w", db, err)
	}
	if marker := dumpCompleteMarker(e.Kind); !strings.Contains(tailBuf.String(), marker) {
		cleanup()
		return fmt.Errorf("can't verify the backup of %s before deleting it (missing %q), so nothing was dropped", db, marker)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("can't save the backup of %s before deleting it, so nothing was dropped: %w", db, err)
	}
	fi, err := f.Stat()
	if err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpData)
		return err
	}
	if err := os.Rename(tmpData, dataPath); err != nil {
		_ = os.Remove(tmpData)
		return err
	}
	meta := deletedBackupMeta{
		ChangeID: changeID,
		Scope:    scope,
		Service:  service,
		Database: db,
		Engine:   e.Kind,
		Owner:    owner,
		TakenAt:  now.UTC(),
		Size:     fi.Size(),
		SHA256:   hex.EncodeToString(hash.Sum(nil)),
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		_ = os.Remove(dataPath)
		return err
	}
	tmpMeta := metaPath + ".tmp"
	if err := os.WriteFile(tmpMeta, b, 0o600); err != nil {
		_ = os.Remove(dataPath)
		return err
	}
	if err := os.Rename(tmpMeta, metaPath); err != nil {
		_ = os.Remove(tmpMeta)
		_ = os.Remove(dataPath)
		return err
	}
	return nil
}

// hasDeletedBackup reports whether a valid, non-expired pre-delete backup exists
// for (changeID, scope, service, db) so Undo of that deleting change restores it.
// A plain create or reinstall has an empty changeID and never restores a kept backup.
func hasDeletedBackup(dir string, now time.Time, changeID, scope, service, db string) bool {
	if dir == "" || changeID == "" {
		return false
	}
	meta, dataPath, err := readDeletedBackup(dir, now, changeID, scope, service, db)
	if err != nil || meta == nil {
		return false
	}
	fi, err := os.Stat(dataPath)
	return err == nil && fi.Size() == meta.Size
}

func readDeletedBackup(dir string, now time.Time, changeID, scope, service, db string) (*deletedBackupMeta, string, error) {
	if changeID == "" {
		return nil, "", nil
	}
	metaPath, dataPath := deletedBackupPaths(dir, changeID, scope, service, db)
	b, err := os.ReadFile(metaPath) //nolint:gosec // under agent databases dir
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var m deletedBackupMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, "", fmt.Errorf("can't read kept backup metadata for %s: %w", db, err)
	}
	if m.ChangeID != "" && m.ChangeID != changeID {
		return nil, "", nil
	}
	if now.Sub(m.TakenAt) > deletedKeepFor+deletedKeepPast {
		return nil, "", nil
	}
	return &m, dataPath, nil
}

// restoreDeletedBackup verifies and loads the pre-delete backup for db from
// changeID back into the freshly recreated database, as the database's owner
// whenever possible, and removes that change's kept backup once restored.
func restoreDeletedBackup(ctx context.Context, client *docker.Client, dir string, now time.Time, changeID string, c *docker.Container, e *Engine, scope, service, db, owner, password string) error {
	meta, dataPath, err := readDeletedBackup(dir, now, changeID, scope, service, db)
	if err != nil {
		return err
	}
	if meta == nil {
		return fmt.Errorf("this server no longer has the backup of %s from before it was deleted, as it keeps it for 7 days. Restore a backup of the volume instead", db)
	}
	f, err := os.Open(dataPath) //nolint:gosec // under agent databases dir
	if err != nil {
		return fmt.Errorf("the backup of %s from before it was deleted is missing on this server. Restore a backup of the volume instead", db)
	}
	defer f.Close()

	// Verify integrity against the recorded SHA-256 before loading.
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return fmt.Errorf("can't read the backup of %s: %w", db, err)
	}
	if hex.EncodeToString(hash.Sum(nil)) != meta.SHA256 {
		return fmt.Errorf("the backup of %s from before it was deleted is damaged. Restore a backup of the volume instead", db)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("can't open the backup of %s: %w", db, err)
	}
	defer gz.Close()

	resource.Progress(ctx, "Restoring "+db+" from its pre-delete backup")
	switch {
	case password != "" && owner != "" && !ReservedUser(owner):
		// UserDriver Undo: we know the freshly recreated owner and its password.
		if err := (Loader{Client: client}).Load(ctx, Load{
			To: c, Engine: e, User: owner, Password: password, Database: db, LockTimeout: "30s",
		}, gz); err != nil {
			return fmt.Errorf("can't restore %s from its pre-delete backup: %w", db, err)
		}
	case owner != "" && !ReservedUser(owner):
		// LogicalDriver Undo with a non-superuser owner: use ADR 0084's lockout
		// loader so every restored object belongs to owner.
		lockout, err := ReadLockout(ctx, client, c, e, db)
		if err != nil {
			return fmt.Errorf("can't prepare owner load to restore %s: %w", db, err)
		}
		loadPass, err := LockOut(ctx, client, c, e, db, lockout)
		if err != nil {
			_ = LetIn(context.WithoutCancel(ctx), client, c, e, db, lockout)
			return fmt.Errorf("can't lock out %s while restoring %s: %w", owner, db, err)
		}
		user, prelude := lockout.Signer(loadPass)
		loadErr := (Loader{Client: client}).Load(ctx, Load{
			To: c, Engine: e, User: user, Password: loadPass, Database: db, Prelude: prelude, LockTimeout: "30s",
		}, gz)
		letErr := LetIn(context.WithoutCancel(ctx), client, c, e, db, lockout)
		if loadErr != nil {
			return fmt.Errorf("can't restore %s from its pre-delete backup: %w", db, loadErr)
		}
		if letErr != nil && !errors.Is(letErr, ErrLoaderLeft) {
			return fmt.Errorf("restored %s, but couldn't let %s back in: %w", db, owner, letErr)
		}
	default:
		// Database owned by the engine's default admin user (postgres / root).
		var cmd []string
		if e.Kind == dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL {
			cmd = withPort("PGPORT", PortOf(e, c.Config.Cmd), []string{
				"env", "DW_DB=" + db, "sh", "-c",
				pgAdminShell + `; exec psql -X -q -1 -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-postgres}" -d "$DW_DB" -f -`,
			})
		} else {
			cmd = []string{
				"env", "DW_DB=" + db, "sh", "-c",
				myAdminShell + `; c=mysql; command -v mariadb >/dev/null 2>&1 && c=mariadb; exec "$c" -uroot -- "$DW_DB"`,
			}
		}
		errs := &capped{max: 4 << 10}
		code, err := client.Exec(ctx, c.ID, cmd, docker.Streams{Stdin: gz, Stdout: io.Discard, Stderr: errs})
		if err != nil {
			return fmt.Errorf("can't restore %s from its pre-delete backup: %w", db, err)
		}
		if code != 0 {
			return fmt.Errorf("can't restore %s from its pre-delete backup (%s exited with code %d: %s)", db, e.Name, code, scrubbed(strings.TrimSpace(errs.String())))
		}
	}

	metaPath, _ := deletedBackupPaths(dir, changeID, scope, service, db)
	_ = os.Remove(dataPath)
	_ = os.Remove(metaPath)
	return nil
}

// PruneDeletedBackups removes pre-delete database backups kept past 7 days,
// along with crash-leftover .tmp files and orphan .sql.gz files older than an hour.
func PruneDeletedBackups(dir string, now time.Time) error {
	if dir == "" {
		return nil
	}
	deletedDir := deletedBackupsDir(dir)
	entries, err := os.ReadDir(deletedDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		key := strings.TrimSuffix(name, ".json")
		metaPath := filepath.Join(deletedDir, name)
		dataPath := filepath.Join(deletedDir, key+".sql.gz")
		b, err := os.ReadFile(metaPath) //nolint:gosec // under agent databases dir
		if err != nil {
			continue
		}
		var m deletedBackupMeta
		if err := json.Unmarshal(b, &m); err != nil || now.Sub(m.TakenAt) >= deletedKeepFor+deletedKeepPast {
			_ = os.Remove(dataPath)
			_ = os.Remove(metaPath)
			continue
		}
		live[key] = true
	}
	for _, entry := range entries {
		name := entry.Name()
		full := filepath.Join(deletedDir, name)
		switch {
		case strings.HasSuffix(name, ".tmp"):
			if fi, err := entry.Info(); err == nil && now.Sub(fi.ModTime()) >= time.Hour {
				_ = os.Remove(full)
			}
		case strings.HasSuffix(name, ".sql.gz"):
			key := strings.TrimSuffix(name, ".sql.gz")
			if !live[key] {
				if fi, err := entry.Info(); err == nil && now.Sub(fi.ModTime()) >= time.Hour {
					_ = os.Remove(full)
				}
			}
		}
	}
	return nil
}

// KeepDeletedBackupsPruned prunes expired pre-delete database backups every hour
// until ctx is done.
func KeepDeletedBackupsPruned(ctx context.Context, dir string) {
	t := time.NewTicker(deletedPruneHit)
	defer t.Stop()
	for {
		_ = PruneDeletedBackups(dir, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ringTail keeps the trailing max bytes written to it, for checking a dump's
// completion marker without buffering the whole dump in memory.
type ringTail struct {
	max int
	buf bytes.Buffer
}

func (t *ringTail) Write(p []byte) (int, error) {
	n := len(p)
	if n >= t.max {
		t.buf.Reset()
		t.buf.Write(p[n-t.max:])
		return n, nil
	}
	if over := t.buf.Len() + n - t.max; over > 0 {
		t.buf.Next(over)
	}
	t.buf.Write(p)
	return n, nil
}

func (t *ringTail) String() string { return t.buf.String() }
