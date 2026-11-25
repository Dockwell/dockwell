// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	engines "github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/lazyre"
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

	importTTL           = 24 * time.Hour
	importIncompleteTTL = time.Hour
	importSweepEvery    = 10 * time.Minute
	maxStagedImports    = 16
	importTailLines     = 10
	importTailBytes     = 4 << 10
	importLockTimeout   = "30s"
)

var validImportIDRe = lazyre.Regexp(`^imp_[0-9A-Za-z]+$`)

// ValidImportID reports whether s is a well-formed staged import identifier.
func ValidImportID(s string) bool {
	return len(s) >= 14 && len(s) <= 44 && validImportIDRe().MatchString(s)
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
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

func (m *ImportMeta) lastTouched() time.Time {
	if !m.UpdatedAt.IsZero() {
		return m.UpdatedAt
	}
	return m.CreatedAt
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
		return nil, &ImportError{Reason: "invalid", Message: "that staged import ID isn't valid. Start a new dump upload"}
	}
	b, err := os.ReadFile(r.importMetaPath(importID))
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

// SweepImports removes expired completed uploads (after 24 h), idle incomplete
// uploads (after 1 h), and orphan partial files without metadata.
func (r *Runner) SweepImports() {
	r.importMu.Lock()
	defer r.importMu.Unlock()
	r.sweepExpiredImportsLocked()
}

// acquireImport marks a staged import as in use by Judge or Apply so neither
// CancelImport nor the background sweeper can delete its files underneath it (N5).
func (r *Runner) acquireImport(importID string) func() {
	if importID == "" {
		return func() {}
	}
	r.importMu.Lock()
	if r.importInUse == nil {
		r.importInUse = map[string]int{}
	}
	r.importInUse[importID]++
	r.importMu.Unlock()
	return func() {
		r.importMu.Lock()
		if n := r.importInUse[importID]; n <= 1 {
			delete(r.importInUse, importID)
		} else {
			r.importInUse[importID] = n - 1
		}
		r.importMu.Unlock()
	}
}

func (r *Runner) sweepExpiredImportsLocked() {
	entries, err := os.ReadDir(r.importsDir())
	if err != nil {
		return
	}
	now := r.now().UTC()
	seenJSON := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		importID := strings.TrimSuffix(name, ".json")
		seenJSON[importID] = true
		if r.importBusy[importID] || r.importInUse[importID] > 0 {
			continue
		}
		m, err := r.readImportMeta(importID)
		if err != nil {
			r.removeImportFiles(importID)
			continue
		}
		ttl := importTTL
		if !m.Complete {
			ttl = importIncompleteTTL
		}
		if now.Sub(m.lastTouched()) > ttl {
			r.removeImportFiles(importID)
		}
	}
	// Remove orphan .part files with no matching metadata.
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".part") {
			continue
		}
		importID := strings.TrimSuffix(name, ".part")
		if seenJSON[importID] || r.importBusy[importID] || r.importInUse[importID] > 0 {
			continue
		}
		_ = os.Remove(filepath.Join(r.importsDir(), name))
	}
}

func (r *Runner) stagedImportsStatsLocked() (count int, unwritten uint64) {
	entries, err := os.ReadDir(r.importsDir())
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		count++
		importID := strings.TrimSuffix(name, ".json")
		m, err := r.readImportMeta(importID)
		if err != nil {
			continue
		}
		if m.SizeBytes > m.Offset {
			unwritten += m.SizeBytes - m.Offset
		}
	}
	return count, unwritten
}

// estimatedRestoredBytes estimates the uncompressed space needed in the
// database engine for a dump of format and sizeBytes.
func estimatedRestoredBytes(format string, sizeBytes uint64) uint64 {
	switch format {
	case ImportFormatSQLGZ, ImportFormatDump:
		return sizeBytes * 3
	default:
		return sizeBytes
	}
}

// BeginImport creates a new staged dump upload or inspects an existing one for resume.
func (r *Runner) BeginImport(owner, importID, serviceID, database, filename, format string, sizeBytes, dbSizeBytes uint64) (*ImportMeta, error) {
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
			return nil, &ImportError{Reason: "import_gone", Message: "that staged dump upload wasn't found or belongs to another user. Upload the file again"}
		}
		return m, nil
	}

	if err := engines.CheckSQLName("database", database); err != nil {
		return nil, &ImportError{Reason: "invalid_database", Message: err.Error()}
	}
	if engines.SystemDatabase(database) {
		return nil, &ImportError{Reason: "invalid_database", Message: database + " is a system database and can't be imported into. Choose one of your own databases"}
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
		return nil, &ImportError{Reason: "too_large", Message: "that dump file is larger than the 16 GiB limit. Split it, or import it from the server's console"}
	}
	stagedCount, stagedUnwritten := r.stagedImportsStatsLocked()
	if stagedCount >= maxStagedImports {
		return nil, &ImportError{Reason: "too_many_imports", Message: "this server already has too many staged dump uploads in progress. Cancel or finish one first"}
	}
	if err := os.MkdirAll(r.importsDir(), 0o700); err != nil {
		return nil, err
	}
	if d, derr := r.disk(); derr == nil && d.free > 0 {
		// Count: remaining bytes of other staged uploads + this upload + pre-import
		// backup of the target database + estimated restored size + disk reserve.
		restored := estimatedRestoredBytes(format, sizeBytes)
		need := stagedUnwritten + sizeBytes + dbSizeBytes + restored
		if d.reserve > 0 {
			need += uint64(d.reserve)
		}
		if uint64(d.free) < need {
			return nil, &ImportError{Reason: "no_space", Message: "this server doesn't have enough free disk space to stage this dump, back up the database first, and load it"}
		}
	}

	now := r.now().UTC()
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
		CreatedAt: now,
		UpdatedAt: now,
	}
	f, err := os.OpenFile(r.importDataPath(meta.ID), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
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
	if r.importCancel == nil {
		r.importCancel = map[string]context.CancelFunc{}
	}
	if r.importCanceled == nil {
		r.importCanceled = map[string]bool{}
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
		return nil, &ImportError{Reason: "import_gone", Message: "that staged dump upload wasn't found or belongs to another user. Upload the file again"}
	}
	if offset != meta.Offset {
		r.importMu.Unlock()
		return nil, &ImportError{Reason: "upload_offset", Message: fmt.Sprintf("upload offset %d doesn't match the %d bytes the server holds", offset, meta.Offset)}
	}
	writeCtx, cancelWrite := context.WithCancel(ctx)
	r.importBusy[importID] = true
	r.importCancel[importID] = cancelWrite
	delete(r.importCanceled, importID)
	r.importMu.Unlock()

	var f *os.File
	removeOnExit := false
	defer func() {
		if f != nil {
			_ = f.Close()
		}
		cancelWrite()
		r.importMu.Lock()
		canceled := r.importCanceled[importID]
		delete(r.importBusy, importID)
		delete(r.importCancel, importID)
		delete(r.importCanceled, importID)
		if canceled || removeOnExit {
			r.removeImportFiles(importID)
		}
		r.importMu.Unlock()
	}()

	f, err = os.OpenFile(r.importDataPath(importID), os.O_WRONLY, 0o600)
	if err != nil {
		return nil, &ImportError{Reason: "import_gone", Message: "the staged dump file is missing on the server. Upload the file again"}
	}
	if _, err := f.Seek(int64(offset), io.SeekStart); err != nil { //nolint:gosec // bounded by MaxImportBytes
		return nil, err
	}

	written := offset
	for {
		if err := writeCtx.Err(); err != nil {
			r.importMu.Lock()
			canceled := r.importCanceled[importID]
			r.importMu.Unlock()
			if canceled {
				return nil, &ImportError{Reason: "import_gone", Message: "that staged dump upload was cancelled"}
			}
			break
		}
		chunk, err := next()
		if len(chunk) > 0 {
			if written+uint64(len(chunk)) > meta.SizeBytes {
				removeOnExit = true
				return nil, &ImportError{Reason: "too_large", Message: "the upload sent more bytes than the declared file size"}
			}
			n, werr := f.Write(chunk)
			written += uint64(n) //nolint:gosec // n >= 0
			if werr != nil {
				removeOnExit = true
				return nil, werr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			r.importMu.Lock()
			canceled := r.importCanceled[importID]
			r.importMu.Unlock()
			if canceled {
				return nil, &ImportError{Reason: "import_gone", Message: "that staged dump upload was cancelled"}
			}
			// Save progress received so far so the client can resume from meta.Offset.
			meta.Offset = written
			meta.Complete = written == meta.SizeBytes
			meta.UpdatedAt = r.now().UTC()
			_ = r.saveImportMeta(meta)
			return nil, err
		}
	}
	r.importMu.Lock()
	canceled := r.importCanceled[importID]
	r.importMu.Unlock()
	if canceled {
		return nil, &ImportError{Reason: "import_gone", Message: "that staged dump upload was cancelled"}
	}
	if err := f.Sync(); err != nil {
		removeOnExit = true
		return nil, err
	}
	meta.Offset = written
	meta.Complete = written == meta.SizeBytes
	meta.UpdatedAt = r.now().UTC()
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
		return nil, &ImportError{Reason: "import_gone", Message: "that staged dump upload wasn't found or belongs to another user. Upload the file again"}
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

// CancelImport removes a staged dump upload. If a chunk upload is currently in
// progress, CancelImport signals it to stop and marks the staged files for
// immediate removal when the writer exits instead of failing with upload_busy.
// When owner is empty (admin cancel), any staged upload can be cancelled.
func (r *Runner) CancelImport(importID, owner string) error {
	r.importMu.Lock()
	defer r.importMu.Unlock()
	meta, err := r.readImportMeta(importID)
	if err != nil {
		var ie *ImportError
		if errors.As(err, &ie) && ie.Reason == "import_gone" {
			return nil
		}
		return err
	}
	if owner != "" && meta.Owner != owner {
		return &ImportError{Reason: "import_gone", Message: "that staged dump upload wasn't found or belongs to another user"}
	}
	if r.importInUse[importID] > 0 {
		return &ImportError{Reason: "import_in_use", Message: "this dump is importing now and can't be cancelled"}
	}
	if r.importBusy[importID] {
		if r.importCanceled == nil {
			r.importCanceled = map[string]bool{}
		}
		r.importCanceled[importID] = true
		if cancel := r.importCancel[importID]; cancel != nil {
			cancel()
		}
		return nil
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

// importExpansionLimit returns the maximum expanded SQL byte count allowed for
// an import before it would pass this server's free-space reservation (N6).
func (r *Runner) importExpansionLimit() (uint64, bool) {
	d, err := r.disk()
	if err != nil || d.free <= 0 {
		return 0, false
	}
	avail := d.free - d.reserve
	if avail <= 0 {
		return 0, true
	}
	return uint64(avail), true
}

type expansionLimitReader struct {
	r        io.Reader
	limit    uint64
	read     uint64
	filename string
	onTrip   func()
	err      error
}

func (e *expansionLimitReader) Read(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.r.Read(p)
	if n > 0 {
		e.read += uint64(n)
		if e.read > e.limit {
			e.err = fmt.Errorf("%s expands past %s, which would pass this server's free-space reservation. Free up disk space or trim the dump, then try again",
				e.filename, human.Size(int64(e.limit))) //nolint:gosec // bounded by disk free space
			if e.onTrip != nil {
				e.onTrip()
			}
			return 0, e.err
		}
	}
	return n, err
}

// scanImportFile scans a staged plain or gzipped SQL dump for refused commands
// and statements, stopping if expanded SQL passes limit (when hasLimit is true).
func scanImportFile(engine dockwellv1.DatabaseEngine, path, format, filename string, limit uint64, hasLimit bool) error {
	f, err := os.Open(path) //nolint:gosec // validated staged dump path
	if err != nil {
		return err
	}
	defer f.Close()
	var src io.Reader = f
	if format == ImportFormatSQLGZ {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("can't decompress staged dump: %w", err)
		}
		defer gz.Close()
		src = gz
	}
	if hasLimit {
		src = &expansionLimitReader{r: src, limit: limit, filename: filename}
	}
	return scanSQLDump(engine, src)
}

// scanImportArchive converts a PostgreSQL .dump/.tar archive to SQL in a
// networkless throwaway container and scans the resulting SQL stream, stopping
// if the expanded SQL passes limit (when hasLimit is true).
func (r *Runner) scanImportArchive(ctx context.Context, c *docker.Container, path, format, filename string, limit uint64, hasLimit bool) error {
	f, err := os.Open(path) //nolint:gosec // validated staged dump path
	if err != nil {
		return err
	}
	defer f.Close()
	convCtx, cancelConv := context.WithCancel(ctx)
	defer cancelConv()
	pr, pw := io.Pipe()
	convDone := make(chan error, 1)
	go func() {
		err := engines.Loader{Client: r.Client}.PGArchiveToSQL(convCtx, c, format, true, f, pw)
		_ = pw.CloseWithError(err)
		convDone <- err
	}()
	var src io.Reader = pr
	var limiter *expansionLimitReader
	if hasLimit {
		limiter = &expansionLimitReader{r: pr, limit: limit, filename: filename, onTrip: cancelConv}
		src = limiter
	}
	scanErr := scanSQLDump(dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL, src)
	if scanErr != nil {
		cancelConv()
	}
	_ = pr.CloseWithError(scanErr)
	convErr := <-convDone
	if limiter != nil && limiter.err != nil {
		return limiter.err
	}
	if scanErr != nil {
		return scanErr
	}
	if convErr != nil {
		var le *engines.LoadError
		if errors.As(convErr, &le) {
			return fmt.Errorf("can't read PostgreSQL archive: %s", sanitizeImportError(firstNonEmpty(le.RawSaid, le.Said)))
		}
		return fmt.Errorf("can't read PostgreSQL archive: %w", convErr)
	}
	return nil
}

// judgeImport plans importing a staged dump into one logical database.
func (r *Runner) judgeImport(ctx context.Context, ref resource.Ref, src resource.RestoreSource) (resource.Decision, bool, error) {
	release := r.acquireImport(src.ImportID)
	defer release()
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
	c, err := r.Client.InspectContainer(ctx, db.id)
	if err != nil {
		return resource.Decision{}, false, err
	}
	e := engines.Recognise(c.Config.Image)
	if e == nil {
		return resource.Decision{}, false, fmt.Errorf("%s doesn't run %s from its official image, so %s can't be imported into", db.name, db.kind.name, dbName)
	}
	// Require a client that refuses client commands on its own so the scanner is
	// never the sole guard (N1).
	if err := (engines.Loader{Client: r.Client}).CheckClientRefusals(ctx, c, e); err != nil {
		return resource.Decision{}, false, err
	}
	// Verify ADR 0084 owner-role lockout is possible so imports never run as superuser.
	if l, err := r.readLockout(ref, dbName); err != nil {
		return resource.Decision{}, false, err
	} else if l == nil {
		if _, err := engines.ReadLockout(ctx, r.Client, c, e, dbName); err != nil {
			return resource.Decision{}, false, engines.FormatImportLockoutError(dbName, err)
		}
	}
	// Pre-scan the staged dump before creating a plan, bounding expanded SQL against free space (N6).
	stagedPath := r.importDataPath(meta.ID)
	limit, hasLimit := r.importExpansionLimit()
	if meta.Format == ImportFormatDump || meta.Format == ImportFormatTar {
		if err := r.scanImportArchive(ctx, c, stagedPath, meta.Format, meta.Filename, limit, hasLimit); err != nil {
			return resource.Decision{}, false, err
		}
	} else {
		if err := scanImportFile(engineKind, stagedPath, meta.Format, meta.Filename, limit, hasLimit); err != nil {
			return resource.Decision{}, false, err
		}
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

// applyImport locks out the database's owner (ADR 0084), backs up the database
// as its owner (kept for 7 days for Undo via restoreOne), loads the pre-scanned
// dump strictly as the owner through engines.Loader, and rolls back cleanly by
// emptying and restoring on failure before letting the app back in.
func (r *Runner) applyImport(ctx context.Context, ref resource.Ref, t volume.Target, st *state, src resource.RestoreSource) error {
	release := r.acquireImport(src.ImportID)
	defer release()
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
	c, err := r.Client.InspectContainer(ctx, db.id)
	if err != nil {
		return err
	}
	e := engines.Recognise(c.Config.Image)
	if e == nil {
		return fmt.Errorf("%s doesn't run %s from its official image, so %s can't be imported into", db.name, db.kind.name, dbName)
	}
	if err := (engines.Loader{Client: r.Client}).CheckClientRefusals(ctx, c, e); err != nil {
		return err
	}

	// Re-scan plain/gzipped dumps before locking out the app or touching the database.
	stagedPath := r.importDataPath(meta.ID)
	limit, hasLimit := r.importExpansionLimit()
	if meta.Format == ImportFormatSQL || meta.Format == ImportFormatSQLGZ {
		resource.Progress(ctx, "Checking "+meta.Filename+" before importing")
		if err := scanImportFile(engineKind, stagedPath, meta.Format, meta.Filename, limit, hasLimit); err != nil {
			return err
		}
	}

	// Take up an existing lockout or read and save a new one before changing anything (M1).
	lockout, err := r.readLockout(ref, dbName)
	if err != nil {
		return err
	}
	took := lockout != nil
	if !took {
		if lockout, err = engines.ReadLockout(ctx, r.Client, c, e, dbName); err != nil {
			return fmt.Errorf("can't keep %s's user out, so nothing was changed: %w", dbName, engines.FormatImportLockoutError(dbName, err))
		}
		if err := os.MkdirAll(r.dir(ref), 0o700); err != nil {
			return fmt.Errorf("can't make a place for %s's import state, so nothing was changed: %w", dbName, err)
		}
		if err := r.saveLockout(ref, dbName, lockout); err != nil {
			return fmt.Errorf("can't keep a record of who importing into %s keeps out, so nothing was changed: %w", dbName, err)
		}
	}
	st.One = lockout
	unloaded := func(err error) error {
		if took {
			return fmt.Errorf("%w. %s stays kept out of %s, as an earlier restore left it. Undo this change or try again", err, lockout.Owner, dbName)
		}
		return errors.Join(err, r.letIn(ctx, ref, c, e, dbName, lockout))
	}

	resource.Progress(ctx, "Keeping "+dbName+"'s user out while it imports")
	password, err := engines.LockOut(ctx, r.Client, c, e, dbName, lockout)
	if err != nil {
		return unloaded(fmt.Errorf("can't keep %s's user out, so nothing was imported: %w", dbName, err))
	}
	if err := r.saveState(ref, st, st.Stage); err != nil {
		return unloaded(err)
	}

	if !reached(st.Stage, stageCopied) {
		resource.Progress(ctx, "Backing up "+dbName+" before importing (kept for 7 days)")
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
			return unloaded(fmt.Errorf("can't make a place for the pre-import backup: %w", err))
		}
		var as []string
		if lockout.Loader != "" {
			// Its owner can't sign in now: the loader role dumps it, as the owner (H2).
			as = []string{lockout.Loader, lockout.Owner}
		}
		err := r.write(ref, &st.Manifest, func(w io.Writer) (int64, error) {
			var derr error
			st.Unpacked, derr = r.dumpAs(ctx, ref, db, dbName, as, w)
			return 0, derr
		}, verifyOne(db.kind))
		if err != nil {
			return unloaded(fmt.Errorf("can't back up %s before importing, so nothing was changed: %w", dbName, err))
		}
		if err := r.saveState(ref, st, stageCopied); err != nil {
			return unloaded(err)
		}
	}

	before := filepath.Join(r.dir(ref), st.ID+"."+st.Format)
	if !reached(st.Stage, stageReplaced) {
		if err := r.saveState(ref, st, stageReplacing); err != nil {
			return unloaded(err)
		}
		// Refresh expansion limit after the pre-import backup took disk space (N6).
		applyLimit, hasApplyLimit := r.importExpansionLimit()
		resource.Progress(ctx, fmt.Sprintf("Importing %s into %s", meta.Filename, dbName))
		touched, err := r.runDumpImport(ctx, c, e, dbName, lockout, password, stagedPath, meta.Format, meta.Filename, meta.SizeBytes, applyLimit, hasApplyLimit)
		if err != nil {
			failed := fmt.Errorf("importing %s into %s failed: %w", meta.Filename, dbName, err)
			if !touched {
				// Nothing reached the database yet (e.g. file open/decompress error):
				// let the app back in without emptying and reloading the database (N5).
				return unloaded(failed)
			}
			// Empty and restore the pre-import backup as owner so extra tables added
			// before failure are removed and ownership stays intact (H2, M2).
			putCtx, cancel := resource.Finishing(ctx, putBackWait)
			defer cancel()
			resource.Progress(putCtx, "Putting back what was in "+dbName+" before the import")
			if perr := r.loadInto(putCtx, c, e, dbName, lockout, password, before); perr != nil {
				return fmt.Errorf("%w. Putting back what was there failed too: %w. %s stays kept out of %s, as what's in it may not be whole. "+
					"A dump of what was there is at %s on this server. Run dockwell changes undo %s to try again",
					failed, formatImportLoadError(e.Name, perr), lockout.Owner, dbName, before, st.Change)
			}
			_ = r.saveState(ref, st, stageUndone)
			if lerr := r.letIn(ctx, ref, c, e, dbName, lockout); lerr != nil {
				return fmt.Errorf("%w. What was in %s before the import is back, and %w", failed, dbName, lerr)
			}
			return fmt.Errorf("%w. What was in %s before the import is back, and its user signs in again", failed, dbName)
		}
		if err := r.saveState(ref, st, stageReplaced); err != nil {
			return errors.Join(err, r.letIn(ctx, ref, c, e, dbName, lockout))
		}
	}
	r.removeImportFiles(meta.ID)
	if err := r.letIn(ctx, ref, c, e, dbName, lockout); err != nil {
		return fmt.Errorf("%s holds the import now, and %w. Undo this change or try again", dbName, err)
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
		cmd = []string{"sh", "-c", pgAdminShell + `; exec psql -XAt -U "${POSTGRES_USER:-postgres}" -d postgres`}
		sql = fmt.Sprintf("SELECT 1 FROM pg_database WHERE datname = '%s';\n", dbName)
	} else {
		cmd = []string{"sh", "-c", myAdminShell + `; t=$(command -v mariadb || command -v mysql); exec "$t" -uroot -N -B`}
		sql = fmt.Sprintf("SELECT 1 FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = '%s';\n", dbName)
	}
	code, err := r.Client.Exec(ctx, db.id, db.tool(cmd), docker.Streams{Stdin: strings.NewReader(sql), Stdout: out, Stderr: errs})
	if err != nil {
		return fmt.Errorf("can't check whether %s exists in %s: %w", dbName, db.name, err)
	}
	if code != 0 {
		return fmt.Errorf("can't check whether %s exists in %s: %s", dbName, db.name, scrubbed(errs.String()))
	}
	if strings.TrimSpace(out.String()) != "1" {
		return fmt.Errorf("%s has no database called %s. Create it first, or choose an existing database", db.name, dbName)
	}
	return nil
}

// runDumpImport loads a staged dump into dbName strictly as the database's
// owner through engines.Loader, with lock_timeout, client refusal guards,
// expansion cap, and sanitized tool error output (H1, H2, M1, M4, N1, N6).
// touched reports whether the database load actually started.
func (r *Runner) runDumpImport(ctx context.Context, c *docker.Container, e *engines.Engine, dbName string, lockout *engines.Lockout, password, path, format, filename string, sizeBytes, limit uint64, hasLimit bool) (touched bool, err error) {
	f, err := os.Open(path) //nolint:gosec // validated staged dump path
	if err != nil {
		return false, err
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

	user, prelude := lockout.Signer(password)
	loadReq := engines.Load{
		To:              c,
		Engine:          e,
		User:            user,
		Password:        password,
		Database:        dbName,
		Prelude:         prelude,
		LockTimeout:     importLockTimeout,
		RequireRefusals: true,
	}

	loadCtx, cancelLoad := context.WithCancel(ctx)
	defer cancelLoad()

	if format == ImportFormatDump || format == ImportFormatTar {
		pr, pw := io.Pipe()
		convDone := make(chan error, 1)
		go func() {
			cerr := engines.Loader{Client: r.Client}.PGArchiveToSQL(loadCtx, c, format, true, prog, pw)
			_ = pw.CloseWithError(cerr)
			convDone <- cerr
		}()
		var src io.Reader = pr
		var limiter *expansionLimitReader
		if hasLimit {
			limiter = &expansionLimitReader{r: pr, limit: limit, filename: filename, onTrip: cancelLoad}
			src = limiter
		}
		scanned := newSQLScanReader(dockwellv1.DatabaseEngine_DATABASE_ENGINE_POSTGRESQL, src)
		loadErr := engines.Loader{Client: r.Client}.Load(loadCtx, loadReq, scanned)
		_ = pr.CloseWithError(loadErr)
		convErr := <-convDone
		if limiter != nil && limiter.err != nil {
			return true, limiter.err
		}
		if scanned.err != nil {
			return true, scanned.err
		}
		if convErr != nil && !errors.Is(convErr, io.ErrClosedPipe) {
			return true, formatImportLoadError(e.Name, convErr)
		}
		if loadErr != nil {
			return true, formatImportLoadError(e.Name, loadErr)
		}
		return true, nil
	}

	var sqlStream io.Reader = prog
	if format == ImportFormatSQLGZ {
		gz, err := gzip.NewReader(prog)
		if err != nil {
			return false, fmt.Errorf("can't decompress %s: %w", filename, err)
		}
		defer gz.Close()
		sqlStream = gz
	}
	var limiter *expansionLimitReader
	if hasLimit {
		limiter = &expansionLimitReader{r: sqlStream, limit: limit, filename: filename, onTrip: cancelLoad}
		sqlStream = limiter
	}
	loadErr := (engines.Loader{Client: r.Client}).Load(loadCtx, loadReq, sqlStream)
	if limiter != nil && limiter.err != nil {
		return true, limiter.err
	}
	if loadErr != nil {
		return true, formatImportLoadError(e.Name, loadErr)
	}
	return true, nil
}

func formatImportLoadError(engineName string, err error) error {
	var le *engines.LoadError
	if errors.As(err, &le) {
		said := sanitizeImportError(firstNonEmpty(le.RawSaid, le.Said))
		if said == "" {
			return fmt.Errorf("%s exited with code %d", engineName, le.Code)
		}
		return fmt.Errorf("%s exited with code %d:\n%s", engineName, le.Code, said)
	}
	return err
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
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
		p.read += uint64(n)
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
func (c Calls) BeginImport(owner, importID, serviceID, database, filename, format string, sizeBytes, dbSizeBytes uint64) (*ImportMeta, error) {
	return c.Runner.BeginImport(owner, importID, serviceID, database, filename, format, sizeBytes, dbSizeBytes)
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
