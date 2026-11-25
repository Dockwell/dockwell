// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	engines "github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

const (
	// ImportFormatSQL is a plain SQL text dump (.sql).
	ImportFormatSQL = "sql"
	// ImportFormatSQLGZ is a gzip-compressed SQL dump (.sql.gz).
	ImportFormatSQLGZ = "sql.gz"
	// ImportFormatDump is a PostgreSQL pg_dump custom archive (.dump).
	ImportFormatDump = "dump"
	// ImportFormatTar is a PostgreSQL pg_dump tar archive (.tar).
	ImportFormatTar = "tar"

	// MaxImportBytes bounds any single staged dump upload (16 GiB).
	MaxImportBytes uint64 = 16 << 30
	// ImportPartBytes is the suggested chunk size per upload PUT (8 MiB).
	ImportPartBytes uint64 = 8 << 20

	importTTL        = 24 * time.Hour
	maxStagedImports = 16
	importTailLines  = 15
	importTailBytes  = 4 << 10
)

var validImportIDRe = regexp.MustCompile(`^imp_[0-9A-Za-z]{10,40}$`)

// ValidImportID reports whether s is a well-formed staged import identifier.
func ValidImportID(s string) bool {
	return validImportIDRe.MatchString(s)
}

// DetectImportFormat normalizes a dump filename's extension to one of sql,
// sql.gz, dump, or tar.
func DetectImportFormat(filename string) (string, error) {
	base := strings.TrimSpace(filepath.Base(filename))
	if base == "" || base == "." || base == ".." || strings.ContainsAny(filename, "/\\\x00") {
		return "", errors.New("choose a dump file with a plain filename ending in .sql, .sql.gz, .dump, or .tar")
	}
	lower := strings.ToLower(base)
	switch {
	case strings.HasSuffix(lower, ".sql.gz"):
		return ImportFormatSQLGZ, nil
	case strings.HasSuffix(lower, ".sql"):
		return ImportFormatSQL, nil
	case strings.HasSuffix(lower, ".dump"):
		return ImportFormatDump, nil
	case strings.HasSuffix(lower, ".tar"):
		return ImportFormatTar, nil
	default:
		return "", fmt.Errorf("%s isn't a supported dump format. Upload a .sql, .sql.gz, .dump, or .tar file", base)
	}
}

// CheckImportEngineFormat verifies that engine can restore a dump of format.
func CheckImportEngineFormat(engine dockwellv1.DatabaseEngine, format string) error {
	switch engine {
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL:
		switch format {
		case ImportFormatSQL, ImportFormatSQLGZ, ImportFormatDump, ImportFormatTar:
			return nil
		}
		return fmt.Errorf("PostgreSQL doesn't support %s imports here. Upload a .sql, .sql.gz, .dump, or .tar file", format)
	case dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL, dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB:
		switch format {
		case ImportFormatSQL, ImportFormatSQLGZ:
			return nil
		case ImportFormatDump, ImportFormatTar:
			return fmt.Errorf("a .%s file is a PostgreSQL archive and can't load into MySQL or MariaDB. Upload a .sql or .sql.gz dump", format)
		}
		return fmt.Errorf("MySQL and MariaDB don't support %s imports here. Upload a .sql or .sql.gz dump", format)
	default:
		return errors.New("dump import is supported for PostgreSQL, MySQL, and MariaDB databases")
	}
}

// ImportMeta describes a staged dump upload on disk.
type ImportMeta struct {
	ID        string    `json:"id"`
	Owner     string    `json:"owner"`
	ServiceID string    `json:"service_id"`
	Database  string    `json:"database"`
	Filename  string    `json:"filename"`
	Format    string    `json:"format"`
	SizeBytes uint64    `json:"size_bytes"`
	Offset    uint64    `json:"offset"`
	Complete  bool      `json:"complete"`
	CreatedAt time.Time `json:"created_at"`
}

// ImportError carries a machine-readable reason for staged upload refusals.
type ImportError struct {
	Reason  string
	Message string
}

func (e *ImportError) Error() string { return e.Message }

func (r *Runner) importsDir() string {
	return filepath.Join(r.Root, "imports")
}

func (r *Runner) importMetaPath(importID string) string {
	return filepath.Join(r.importsDir(), importID+".json")
}

func (r *Runner) importDataPath(importID string) string {
	return filepath.Join(r.importsDir(), importID+".part")
}

func (r *Runner) readImportMeta(importID string) (*ImportMeta, error) {
	if !ValidImportID(importID) {
		return nil, &ImportError{Reason: "invalid", Message: "invalid import ID"}
	}
	b, err := os.ReadFile(r.importMetaPath(importID)) //nolint:gosec // validated ID under agent backup root
	if errors.Is(err, os.ErrNotExist) {
		return nil, &ImportError{Reason: "import_gone", Message: "that staged dump upload wasn't found or has expired. Upload the file again"}
	}
	if err != nil {
		return nil, err
	}
	var m ImportMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("can't read staged import metadata: %w", err)
	}
	return &m, nil
}

func (r *Runner) saveImportMeta(m *ImportMeta) error {
	if err := os.MkdirAll(r.importsDir(), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(r.importMetaPath(m.ID), b)
}

func (r *Runner) removeImportFiles(importID string) {
	_ = os.Remove(r.importDataPath(importID))
	_ = os.Remove(r.importMetaPath(importID))
}

func (r *Runner) sweepExpiredImportsLocked() {
	entries, err := os.ReadDir(r.importsDir())
	if err != nil {
		return
	}
	cutoff := r.now().UTC().Add(-importTTL)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		importID := strings.TrimSuffix(name, ".json")
		if r.importBusy[importID] {
			continue
		}
		m, err := r.readImportMeta(importID)
		if err != nil || m.CreatedAt.Before(cutoff) {
			r.removeImportFiles(importID)
		}
	}
}

func (r *Runner) countStagedImportsLocked() int {
	entries, err := os.ReadDir(r.importsDir())
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// BeginImport creates a new staged dump upload or inspects an existing one for resume.
func (r *Runner) BeginImport(owner, importID, serviceID, database, filename, format string, sizeBytes uint64) (*ImportMeta, error) {
	if owner == "" {
		return nil, &ImportError{Reason: "invalid", Message: "staged import has no owner"}
	}
	r.importMu.Lock()
	defer r.importMu.Unlock()
	if r.importBusy == nil {
		r.importBusy = map[string]bool{}
	}
	r.sweepExpiredImportsLocked()

	if importID != "" {
		m, err := r.readImportMeta(importID)
		if err != nil {
			return nil, err
		}
		if m.Owner != owner {
			return nil, &ImportError{Reason: "import_gone", Message: "that staged dump upload wasn't found or belongs to another session. Upload the file again"}
		}
		return m, nil
	}

	if err := engines.CheckSQLName("database", database); err != nil {
		return nil, &ImportError{Reason: "invalid_database", Message: err.Error()}
	}
	if engines.SystemDatabase(database) {
		return nil, &ImportError{Reason: "invalid_database", Message: fmt.Sprintf("%s is a system database and can't be imported into. Choose one of your own databases", database)}
	}
	detected, err := DetectImportFormat(filename)
	if err != nil {
		return nil, &ImportError{Reason: "invalid_format", Message: err.Error()}
	}
	if format != "" && format != detected {
		return nil, &ImportError{Reason: "invalid_format", Message: fmt.Sprintf("filename %s doesn't match format %s", filepath.Base(filename), format)}
	}
	format = detected
	if sizeBytes == 0 {
		return nil, &ImportError{Reason: "invalid", Message: "that dump file is empty. Choose a non-empty dump file"}
	}
	if sizeBytes > MaxImportBytes {
		return nil, &ImportError{Reason: "too_large", Message: "that dump file is larger than the 16 GiB limit"}
	}
	if r.countStagedImportsLocked() >= maxStagedImports {
		return nil, &ImportError{Reason: "too_many_imports", Message: "this server already has too many staged dump uploads in progress. Cancel or finish one first"}
	}
	if err := os.MkdirAll(r.importsDir(), 0o700); err != nil {
		return nil, err
	}
	if r.Disk != nil {
		d := r.Disk()
		// Need room for the staged file plus a safety margin for the pre-import backup.
		need := int64(sizeBytes) + d.reserve //nolint:gosec // bounded by MaxImportBytes (16 GiB)
		if d.free > 0 && d.free < need {
			return nil, &ImportError{Reason: "no_space", Message: "this server doesn't have enough free disk space to stage this dump and back up the database first"}
		}
	}

	meta := &ImportMeta{
		ID:        id.New("imp"),
		Owner:     owner,
		ServiceID: serviceID,
		Database:  database,
		Filename:  filepath.Base(filename),
		Format:    format,
		SizeBytes: sizeBytes,
		Offset:    0,
		Complete:  false,
		CreatedAt: r.now().UTC(),
	}
	f, err := os.OpenFile(r.importDataPath(meta.ID), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // validated ID under agent backup root
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	if err := r.saveImportMeta(meta); err != nil {
		r.removeImportFiles(meta.ID)
		return nil, err
	}
	return meta, nil
}

// WriteImport appends bounded chunks from next onto a staged dump starting at offset.
func (r *Runner) WriteImport(ctx context.Context, importID, owner string, offset uint64, next func() ([]byte, error)) (*ImportMeta, error) {
	r.importMu.Lock()
	if r.importBusy == nil {
		r.importBusy = map[string]bool{}
	}
	if r.importBusy[importID] {
		r.importMu.Unlock()
		return nil, &ImportError{Reason: "upload_busy", Message: "another chunk of this dump is already uploading. Wait for it to finish"}
	}
	meta, err := r.readImportMeta(importID)
	if err != nil {
		r.importMu.Unlock()
		return nil, err
	}
	if meta.Owner != owner {
		r.importMu.Unlock()
		return nil, &ImportError{Reason: "import_gone", Message: "that staged dump upload wasn't found or belongs to another session. Upload the file again"}
	}
	if offset != meta.Offset {
		r.importMu.Unlock()
		return nil, &ImportError{Reason: "upload_offset", Message: fmt.Sprintf("upload offset %d doesn't match the %d bytes the server holds", offset, meta.Offset)}
	}
	r.importBusy[importID] = true
	r.importMu.Unlock()
	defer func() {
		r.importMu.Lock()
		delete(r.importBusy, importID)
		r.importMu.Unlock()
	}()

	f, err := os.OpenFile(r.importDataPath(importID), os.O_WRONLY, 0o600) //nolint:gosec // validated ID under agent backup root
	if err != nil {
		return nil, &ImportError{Reason: "import_gone", Message: "the staged dump file is missing on the server. Upload the file again"}
	}
	defer f.Close()
	if _, err := f.Seek(int64(offset), io.SeekStart); err != nil { //nolint:gosec // bounded by MaxImportBytes
		return nil, err
	}

	written := offset
	for {
		if err := ctx.Err(); err != nil {
			break
		}
		chunk, err := next()
		if len(chunk) > 0 {
			if written+uint64(len(chunk)) > meta.SizeBytes {
				return nil, &ImportError{Reason: "too_large", Message: "the upload sent more bytes than the declared file size"}
			}
			n, werr := f.Write(chunk)
			written += uint64(n) //nolint:gosec // n >= 0
			if werr != nil {
				return nil, werr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Save progress received so far so the client can resume from meta.Offset.
			meta.Offset = written
			meta.Complete = written == meta.SizeBytes
			_ = r.saveImportMeta(meta)
			return nil, err
		}
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	meta.Offset = written
	meta.Complete = written == meta.SizeBytes
	if err := r.saveImportMeta(meta); err != nil {
		return nil, err
	}
	return meta, nil
}

// InspectImport validates that a staged dump is complete and its header matches
// its declared format. When owner is non-empty, ownership is checked.
func (r *Runner) InspectImport(importID, owner string) (*ImportMeta, error) {
	r.importMu.Lock()
	busy := r.importBusy[importID]
	r.importMu.Unlock()
	if busy {
		return nil, &ImportError{Reason: "upload_busy", Message: "this dump is still uploading. Wait for the upload to finish"}
	}
	meta, err := r.readImportMeta(importID)
	if err != nil {
		return nil, err
	}
	if owner != "" && meta.Owner != owner {
		return nil, &ImportError{Reason: "import_gone", Message: "that staged dump upload wasn't found or belongs to another session. Upload the file again"}
	}
	if !meta.Complete || meta.Offset != meta.SizeBytes {
		return nil, &ImportError{Reason: "incomplete", Message: fmt.Sprintf("only %d of %d bytes of %s have uploaded. Finish uploading before planning", meta.Offset, meta.SizeBytes, meta.Filename)}
	}
	path := r.importDataPath(importID)
	fi, err := os.Stat(path)
	if err != nil || uint64(fi.Size()) != meta.SizeBytes { //nolint:gosec // fi.Size >= 0
		return nil, &ImportError{Reason: "import_gone", Message: "the staged dump file is missing or incomplete on the server. Upload the file again"}
	}
	if err := validateImportHeader(path, meta.Format); err != nil {
		return nil, &ImportError{Reason: "invalid_format", Message: err.Error()}
	}
	return meta, nil
}

// CancelImport removes a staged dump upload owned by owner.
func (r *Runner) CancelImport(importID, owner string) error {
	r.importMu.Lock()
	defer r.importMu.Unlock()
	if r.importBusy[importID] {
		return &ImportError{Reason: "upload_busy", Message: "this dump is currently receiving data. Stop the upload first"}
	}
	meta, err := r.readImportMeta(importID)
	if err != nil {
		var ie *ImportError
		if errors.As(err, &ie) && ie.Reason == "import_gone" {
			return nil
		}
		return err
	}
	if owner != "" && meta.Owner != owner {
		return &ImportError{Reason: "import_gone", Message: "that staged dump upload wasn't found or belongs to another session"}
	}
	r.removeImportFiles(importID)
	return nil
}

// validateImportHeader checks the leading bytes of a staged dump against format.
func validateImportHeader(path, format string) error {
	f, err := os.Open(path) //nolint:gosec // validated staged dump path
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	head = head[:n]
	if len(head) == 0 {
		return errors.New("the uploaded dump file is empty")
	}
	switch format {
	case ImportFormatSQLGZ:
		if len(head) < 2 || head[0] != 0x1f || head[1] != 0x8b {
			return errors.New("this .sql.gz file isn't gzip-compressed. Upload a valid .sql.gz file or rename plain SQL to .sql")
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("this .sql.gz file can't be opened as gzip: %w", err)
		}
		_ = gz.Close()
		return nil
	case ImportFormatDump:
		if len(head) < 5 || string(head[:5]) != "PGDMP" {
			return errors.New("this .dump file isn't a PostgreSQL custom-format dump (missing PGDMP header). Use pg_dump -Fc or upload a .sql file")
		}
		return nil
	case ImportFormatTar:
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		tr := tar.NewReader(f)
		if _, err := tr.Next(); err != nil {
			return fmt.Errorf("this .tar file isn't a valid tar archive: %w", err)
		}
		return nil
	case ImportFormatSQL:
		if len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b {
			return errors.New("this .sql file looks gzip-compressed. Rename it to .sql.gz and upload again")
		}
		if len(head) >= 5 && string(head[:5]) == "PGDMP" {
			return errors.New("this .sql file looks like a PostgreSQL custom-format dump. Rename it to .dump and upload again")
		}
		if bytes.IndexByte(head, 0) >= 0 {
			return errors.New("this .sql file looks like binary data rather than plain SQL")
		}
		return nil
	default:
		return fmt.Errorf("unsupported dump format %q", format)
	}
}

// judgeImport plans importing a staged dump into one logical database.
func (r *Runner) judgeImport(ctx context.Context, ref resource.Ref, src resource.RestoreSource) (resource.Decision, bool, error) {
	meta, err := r.InspectImport(src.ImportID, "")
	if err != nil {
		return resource.Decision{}, false, err
	}
	if src.ImportDatabase != "" && meta.Database != src.ImportDatabase {
		return resource.Decision{}, false, fmt.Errorf("this staged dump is for %s, not %s", meta.Database, src.ImportDatabase)
	}
	dbName := meta.Database
	t := volume.TargetOf(ref)
	if _, err := r.Client.InspectVolume(ctx, t.Name); err != nil {
		return resource.Decision{}, false, r.noVolume(ref, err)
	}
	u, err := r.users(ctx, t)
	if err != nil {
		return resource.Decision{}, false, err
	}
	db := u.database()
	if db == nil || db.kind.dumpOne == nil {
		return resource.Decision{}, false, fmt.Errorf("%s holds no PostgreSQL, MySQL or MariaDB server, so %s can't be imported here", ref.Name, meta.Filename)
	}
	if !db.running {
		return resource.Decision{}, false, fmt.Errorf("%s isn't running, and importing into %s needs it. Start it, then plan this again", db.name, dbName)
	}
	engineKind := engineKindNamed(db.kind.name)
	if err := CheckImportEngineFormat(engineKind, meta.Format); err != nil {
		return resource.Decision{}, false, err
	}
	if err := r.checkLogicalExists(ctx, db, dbName); err != nil {
		return resource.Decision{}, false, err
	}
	est := estimate(&Manifest{Format: formatDump, Size: int64(meta.SizeBytes)}) //nolint:gosec // bounded by MaxImportBytes
	return resource.Decision{
		Action:   resource.ActionReplace,
		Effect:   resource.EffectRestart,
		Destroys: true,
		Downtime: est,
		Restore: &resource.RestorePlan{
			Method:       resource.BackupDump,
			Database:     db.kind.name,
			OneDatabase:  dbName,
			Size:         int64(meta.SizeBytes), //nolint:gosec // bounded by MaxImportBytes
			ImportFile:   meta.Filename,
			ImportFormat: meta.Format,
		},
	}, true, nil
}

// judgeUndoImport plans undoing a previous dump import by restoring its
// pre-import backup of that single logical database.
func (r *Runner) judgeUndoImport(ctx context.Context, ref resource.Ref, src resource.RestoreSource, m *Manifest) (resource.Decision, bool, error) {
	t := volume.TargetOf(ref)
	if _, err := r.Client.InspectVolume(ctx, t.Name); err != nil {
		return resource.Decision{}, false, r.noVolume(ref, err)
	}
	u, err := r.users(ctx, t)
	if err != nil {
		return resource.Decision{}, false, err
	}
	db, err := oneServer(u, m)
	if err != nil {
		return resource.Decision{}, false, err
	}
	if err := r.checkLogicalExists(ctx, db, m.OneDatabase); err != nil {
		return resource.Decision{}, false, err
	}
	return resource.Decision{
		Action:   resource.ActionReplace,
		Effect:   resource.EffectRestart,
		Destroys: true,
		Downtime: estimate(m),
		Restore: &resource.RestorePlan{
			Undoes:      src.Undoes,
			Method:      m.Method,
			Database:    m.Database,
			OneDatabase: m.OneDatabase,
			Taken:       m.Taken,
			Size:        m.Size,
		},
	}, true, nil
}

// applyImport backs up the target logical database first, then streams the
// staged dump into the engine's own tool inside its container with progress
// and last-lines failure diagnostics, rolling back on failure.
func (r *Runner) applyImport(ctx context.Context, ref resource.Ref, t volume.Target, st *state, src resource.RestoreSource) error {
	meta, err := r.InspectImport(src.ImportID, "")
	if err != nil {
		return err
	}
	dbName := meta.Database
	u, err := r.users(ctx, t)
	if err != nil {
		return err
	}
	db := u.database()
	if db == nil || db.kind.dumpOne == nil {
		return fmt.Errorf("%s holds no PostgreSQL, MySQL or MariaDB server, so %s can't be imported here", ref.Name, meta.Filename)
	}
	if !db.running {
		return fmt.Errorf("%s isn't running, and importing into %s needs it. Start it, then try again", db.name, dbName)
	}
	engineKind := engineKindNamed(db.kind.name)
	if err := CheckImportEngineFormat(engineKind, meta.Format); err != nil {
		return err
	}
	if err := r.checkLogicalExists(ctx, db, dbName); err != nil {
		return err
	}
	if !reached(st.Stage, stageCopied) {
		resource.Progress(ctx, "Backing up "+dbName+" before importing (kept for 7 days)")
		st.Imported = true
		st.Manifest = Manifest{
			ID:          beforePrefix + st.Change,
			Environment: ref.Scope,
			Volume:      ref.Name,
			Format:      db.kind.dumpFormat(),
			Method:      resource.BackupDump,
			Database:    db.kind.name,
			OneDatabase: dbName,
			Image:       db.image,
			Taken:       r.now().UTC(),
		}
		if err := os.MkdirAll(r.dir(ref), 0o700); err != nil {
			return fmt.Errorf("can't make a place for the pre-import backup: %w", err)
		}
		err := r.write(ref, &st.Manifest, func(w io.Writer) (int64, error) {
			var derr error
			st.Unpacked, derr = r.dumpOneAdmin(ctx, db, dbName, w)
			return 0, derr
		}, verifyOne(db.kind))
		if err != nil {
			return fmt.Errorf("can't back up %s before importing, so nothing was changed: %w", dbName, err)
		}
		if err := r.saveState(ref, st, stageCopied); err != nil {
			return err
		}
	}
	before := filepath.Join(r.dir(ref), st.ID+"."+st.Format)
	if !reached(st.Stage, stageReplaced) {
		if err := r.saveState(ref, st, stageReplacing); err != nil {
			return err
		}
		resource.Progress(ctx, fmt.Sprintf("Importing %s into %s", meta.Filename, dbName))
		if err := r.runDumpImport(ctx, db, dbName, r.importDataPath(meta.ID), meta.Format, meta.Filename, meta.SizeBytes); err != nil {
			failed := fmt.Errorf("importing %s into %s failed: %w", meta.Filename, dbName, err)
			// Restore the pre-import backup so the database is not left half-imported.
			putCtx, cancel := resource.Finishing(ctx, putBackWait)
			defer cancel()
			resource.Progress(putCtx, "Putting back what was in "+dbName+" before the import")
			if perr := r.restoreAdminBackup(putCtx, db, dbName, before); perr != nil {
				return fmt.Errorf("%w. Putting back what was there failed too: %w. A backup from before the import is at %s on this server", failed, perr, before)
			}
			_ = r.saveState(ref, st, stageUndone)
			return fmt.Errorf("%w. What was in %s before the import is back", failed, dbName)
		}
		if err := r.saveState(ref, st, stageReplaced); err != nil {
			return err
		}
	}
	r.removeImportFiles(meta.ID)
	return r.saveState(ref, st, stageFinished)
}

// applyUndoImport puts back the pre-import backup taken by an earlier dump import.
func (r *Runner) applyUndoImport(ctx context.Context, ref resource.Ref, t volume.Target, st *state, m *Manifest, file string) error {
	u, err := r.users(ctx, t)
	if err != nil {
		return err
	}
	db, err := oneServer(u, m)
	if err != nil {
		return err
	}
	dbName := m.OneDatabase
	if err := r.checkLogicalExists(ctx, db, dbName); err != nil {
		return err
	}
	if !reached(st.Stage, stageCopied) {
		resource.Progress(ctx, "Backing up "+dbName+" before undoing the import")
		st.Imported = true
		st.Manifest = Manifest{
			ID:          beforePrefix + st.Change,
			Environment: ref.Scope,
			Volume:      ref.Name,
			Format:      db.kind.dumpFormat(),
			Method:      resource.BackupDump,
			Database:    db.kind.name,
			OneDatabase: dbName,
			Image:       db.image,
			Taken:       r.now().UTC(),
		}
		if err := os.MkdirAll(r.dir(ref), 0o700); err != nil {
			return err
		}
		if err := r.write(ref, &st.Manifest, func(w io.Writer) (int64, error) {
			var derr error
			st.Unpacked, derr = r.dumpOneAdmin(ctx, db, dbName, w)
			return 0, derr
		}, verifyOne(db.kind)); err != nil {
			return fmt.Errorf("can't back up %s before undoing the import: %w", dbName, err)
		}
		if err := r.saveState(ref, st, stageCopied); err != nil {
			return err
		}
	}
	if !reached(st.Stage, stageReplaced) {
		if err := r.saveState(ref, st, stageReplacing); err != nil {
			return err
		}
		resource.Progress(ctx, "Restoring "+dbName+" to what was there before the import")
		if err := r.restoreAdminBackup(ctx, db, dbName, file); err != nil {
			return fmt.Errorf("undoing the import into %s failed: %w", dbName, err)
		}
		if err := r.saveState(ref, st, stageReplaced); err != nil {
			return err
		}
	}
	return r.saveState(ref, st, stageFinished)
}

func engineKindNamed(name string) dockwellv1.DatabaseEngine {
	switch name {
	case "PostgreSQL":
		return dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL
	case "MySQL":
		return dockwellv1.DatabaseEngine_DATABASE_ENGINE_MYSQL
	case "MariaDB":
		return dockwellv1.DatabaseEngine_DATABASE_ENGINE_MARIADB
	default:
		return dockwellv1.DatabaseEngine_DATABASE_ENGINE_UNSPECIFIED
	}
}

// checkLogicalExists verifies dbName exists on the running SQL engine.
func (r *Runner) checkLogicalExists(ctx context.Context, db *database, dbName string) error {
	if err := engines.CheckSQLName("database", dbName); err != nil {
		return err
	}
	if engines.SystemDatabase(dbName) {
		return fmt.Errorf("%s is a system database and can't be imported into", dbName)
	}
	out := &tail{}
	errs := &tail{}
	var cmd []string
	var sql string
	if db.kind.name == "PostgreSQL" {
		cmd = []string{"sh", "-c", `exec psql -XAt -U "${POSTGRES_USER:-postgres}" -d postgres`}
		sql = fmt.Sprintf("SELECT 1 FROM pg_database WHERE datname = '%s';\n", dbName)
	} else {
		cmd = []string{"sh", "-c", `export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; t=$(command -v mariadb || command -v mysql); exec "$t" -uroot -N -B`}
		sql = fmt.Sprintf("SELECT 1 FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = '%s';\n", dbName)
	}
	code, err := r.Client.Exec(ctx, db.id, db.tool(cmd), docker.Streams{Stdin: strings.NewReader(sql), Stdout: out, Stderr: errs})
	if err != nil {
		return fmt.Errorf("can't check whether %s exists in %s: %w", dbName, db.name, err)
	}
	if code != 0 {
		return fmt.Errorf("can't check whether %s exists in %s: %s", dbName, db.name, lastLines(errs.String(), 4))
	}
	if strings.TrimSpace(out.String()) != "1" {
		return fmt.Errorf("%s has no database called %s. Create it first, or choose an existing database", db.name, dbName)
	}
	return nil
}

// dumpOneAdmin dumps one logical database using the engine's admin tool inside
// its container, gzipped into w, without exposing any password in argv.
func (r *Runner) dumpOneAdmin(ctx context.Context, db *database, dbName string, w io.Writer) (int64, error) {
	var cmd []string
	if db.kind.name == "PostgreSQL" {
		cmd = []string{"env", "DW_DB=" + dbName, "sh", "-c",
			`exec pg_dump --clean --if-exists --no-owner --no-acl -U "${POSTGRES_USER:-postgres}" -- "$DW_DB"`}
	} else {
		cmd = []string{"env", "DW_DB=" + dbName, "sh", "-c",
			`export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; t=$(command -v mariadb-dump || command -v mysqldump); exec "$t" -uroot --single-transaction --routines --triggers --events --add-drop-table -- "$DW_DB"`}
	}
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return 0, err
	}
	errs := &tail{}
	plain := &counter{w: gz}
	code, err := r.Client.Exec(ctx, db.id, db.tool(cmd), docker.Streams{Stdout: plain, Stderr: errs})
	if err != nil {
		return 0, fmt.Errorf("can't back up %s: %w", dbName, err)
	}
	if code != 0 {
		return 0, fmt.Errorf("%s's dump exited with code %d: %s", db.kind.name, code, lastLines(errs.String(), 6))
	}
	return plain.n, gz.Close()
}

// restoreAdminBackup restores a pre-import gzipped SQL backup back into dbName.
func (r *Runner) restoreAdminBackup(ctx context.Context, db *database, dbName, path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	return r.runDumpImport(ctx, db, dbName, path, ImportFormatSQLGZ, filepath.Base(path), uint64(fi.Size())) //nolint:gosec // fi.Size >= 0
}

// runDumpImport streams a staged dump file into the engine's native restore
// tool inside its container, reporting byte progress and capturing the tool's
// last lines on failure.
func (r *Runner) runDumpImport(ctx context.Context, db *database, dbName, path, format, filename string, sizeBytes uint64) error {
	f, err := os.Open(path) //nolint:gosec // validated staged dump or backup path
	if err != nil {
		return err
	}
	defer f.Close()

	prog := &progressReader{
		r:     f,
		total: sizeBytes,
		say: func(done uint64) {
			if sizeBytes > 0 {
				pct := min(100, (done*100)/sizeBytes)
				resource.Progress(ctx, fmt.Sprintf("Importing %s into %s (%d%%)", filename, dbName, pct))
			}
		},
	}

	var stdin io.Reader = prog
	var gz *gzip.Reader
	if format == ImportFormatSQLGZ {
		gz, err = gzip.NewReader(prog)
		if err != nil {
			return fmt.Errorf("can't decompress %s: %w", filename, err)
		}
		defer gz.Close()
		stdin = gz
	}

	var cmd []string
	if db.kind.name == "PostgreSQL" {
		// Terminate other sessions on dbName so DROP/CREATE/pg_restore --clean don't deadlock.
		termSQL := fmt.Sprintf("SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '%s' AND pid <> pg_backend_pid();\n", dbName)
		_, _ = r.Client.Exec(ctx, db.id, db.tool([]string{"sh", "-c", `exec psql -XAt -U "${POSTGRES_USER:-postgres}" -d postgres`}), docker.Streams{Stdin: strings.NewReader(termSQL)})
		switch format {
		case ImportFormatSQL, ImportFormatSQLGZ:
			cmd = []string{"env", "DW_DB=" + dbName, "sh", "-c",
				`exec psql -X -v ON_ERROR_STOP=1 --single-transaction -U "${POSTGRES_USER:-postgres}" -d "$DW_DB"`}
		case ImportFormatDump:
			cmd = []string{"env", "DW_DB=" + dbName, "sh", "-c",
				`exec pg_restore --no-owner --no-acl --clean --if-exists -e -Fc -U "${POSTGRES_USER:-postgres}" -d "$DW_DB"`}
		case ImportFormatTar:
			cmd = []string{"env", "DW_DB=" + dbName, "sh", "-c",
				`exec pg_restore --no-owner --no-acl --clean --if-exists -e -Ft -U "${POSTGRES_USER:-postgres}" -d "$DW_DB"`}
		default:
			return fmt.Errorf("unsupported PostgreSQL import format %q", format)
		}
	} else {
		if format != ImportFormatSQL && format != ImportFormatSQLGZ {
			return fmt.Errorf("unsupported %s import format %q", db.kind.name, format)
		}
		cmd = []string{"env", "DW_DB=" + dbName, "sh", "-c",
			`export MYSQL_PWD="${MARIADB_ROOT_PASSWORD:-${MYSQL_ROOT_PASSWORD:-}}"; t=$(command -v mariadb || command -v mysql); exec "$t" -uroot --binary-mode=1 -- "$DW_DB"`}
	}

	outTail := &tail{}
	errTail := &tail{}
	code, err := r.Client.Exec(ctx, db.id, db.tool(cmd), docker.Streams{Stdin: stdin, Stdout: outTail, Stderr: errTail})
	if err != nil {
		return err
	}
	if code != 0 {
		said := lastLines(errTail.String(), importTailLines)
		if said == "" {
			said = lastLines(outTail.String(), importTailLines)
		}
		if said == "" {
			return fmt.Errorf("%s exited with code %d", db.kind.name, code)
		}
		return fmt.Errorf("%s exited with code %d:\n%s", db.kind.name, code, said)
	}
	return nil
}

// lastLines returns up to maxLines trailing non-empty lines, bounded to importTailBytes.
func lastLines(s string, maxLines int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	joined := strings.Join(lines, "\n")
	if len(joined) > importTailBytes {
		joined = joined[len(joined)-importTailBytes:]
	}
	return joined
}

type progressReader struct {
	r      io.Reader
	total  uint64
	read   uint64
	lastAt time.Time
	lastN  uint64
	say    func(done uint64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.read += uint64(n) //nolint:gosec // n > 0
		now := time.Now()
		if p.say != nil && (p.lastAt.IsZero() || now.Sub(p.lastAt) >= 250*time.Millisecond || p.read-p.lastN >= 2<<20 || p.read >= p.total) {
			p.lastAt = now
			p.lastN = p.read
			p.say(p.read)
		}
	}
	return n, err
}

// BeginImport forwards staged dump start/resume to the underlying Runner.
func (c Calls) BeginImport(owner, importID, serviceID, database, filename, format string, sizeBytes uint64) (*ImportMeta, error) {
	return c.Runner.BeginImport(owner, importID, serviceID, database, filename, format, sizeBytes)
}

// WriteImport forwards staged dump streaming writes to the underlying Runner.
func (c Calls) WriteImport(ctx context.Context, importID, owner string, offset uint64, next func() ([]byte, error)) (*ImportMeta, error) {
	return c.Runner.WriteImport(ctx, importID, owner, offset, next)
}

// InspectImport forwards staged dump validation to the underlying Runner.
func (c Calls) InspectImport(importID, owner string) (*ImportMeta, error) {
	return c.Runner.InspectImport(importID, owner)
}

// CancelImport forwards staged dump cancellation to the underlying Runner.
func (c Calls) CancelImport(importID, owner string) error {
	return c.Runner.CancelImport(importID, owner)
}
