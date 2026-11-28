// SPDX-License-Identifier: AGPL-3.0-only

package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/yeka/zip"

	engines "github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

const (
	clickHouseData   = "/var/lib/clickhouse"
	clickHouseStages = clickHouseData + "/backups/dockwell-stage"
)

var clickHouse = &databaseKind{
	name: "ClickHouse", image: "clickhouse/clickhouse-server", data: clickHouseData, series: 2,
	format: formatClickHouseDump, verify: verifyClickHouse,
	portVar: "DOCKWELL_CLICKHOUSE_PORT",
	ready:   clickHouseCommand("--query=SELECT 1"),
}

func (k *databaseKind) matches(repo string) bool {
	return k.image == repo || k == clickHouse && repo == "clickhouse"
}

func clickHouseCommand(args ...string) []string {
	return append([]string{"sh", "-ec", `
		if [ -z "${CLICKHOUSE_PASSWORD:-}" ] && [ -n "${CLICKHOUSE_PASSWORD_FILE:-}" ]; then
			CLICKHOUSE_PASSWORD="$(cat "$CLICKHOUSE_PASSWORD_FILE")"
			export CLICKHOUSE_PASSWORD
		fi
		exec clickhouse-client --host=127.0.0.1 --port="${DOCKWELL_CLICKHOUSE_PORT:-9000}" "$@"`, "sh"}, args...)
}

// ClickHouse writes one native archive at a time in its data volume. The volume
// lock also covers restoring that archive and cleaning up a stopped run.
func (r *Runner) clickHouseStage(ctx context.Context, db *database) error {
	const config = clickHouseData + "/preprocessed_configs/config.xml"
	check := []string{"sh", "-ec", `
		extract() { clickhouse extract-from-config --config-file="$1" --key="$2"; }
		backup_path="$(extract "$1" backups.allowed_path)"
		data_path="$(extract "$1" path)"
		user_files="$(extract "$1" user_files_path)"
		test "$backup_path" = backups
		test "${data_path%/}" = "$2"
		test "${user_files%/}" = "$2/user_files"
		test ! -L "$2/user_files"
		test ! -L "$2/backups"
		test ! -L "$3"
		for folder in "$2/backups" "$3"; do
			if [ -d "$folder" ]; then test "$(stat -c %d "$folder")" = "$(stat -c %d "$2")"; fi
		done
		`, "sh", config, clickHouseData, clickHouseStages}
	if err := r.clickHouseExec(ctx, db, check, docker.Streams{}, "checking the backup directory"); err != nil {
		return errors.New("ClickHouse's backup staging and user files aren't in separate standard folders, so Dockwell can't stage the archive. Set backups.allowed_path to backups and user_files_path to /var/lib/clickhouse/user_files, then try again")
	}
	// ClickHouse permits a restore into any database the caller can write to.
	// Restrict metadata access and keep the staging filename unpredictable.
	if err := (engines.Reader{Client: r.Client}).RequireClickHouseSystemGrants(ctx, db.id); err != nil {
		return err
	}
	prepare := []string{"sh", "-ec", `
		umask 077
		if [ "$(id -u)" = 0 ]; then owner="$(stat -c %u:%g "$2")"; else owner=; fi
		for folder in "$2/backups" "$1"; do
			if [ ! -d "$folder" ]; then
				mkdir "$folder"
				if [ -n "$owner" ]; then chown "$owner" "$folder"; fi
			fi
		done
		chmod 700 "$1"
		find "$1" -maxdepth 1 -type f \( -name 'archive-*.zip' -o -name 'archive-*.tar' \) -exec rm -f {} +
		`, "sh", clickHouseStages, clickHouseData}
	return r.clickHouseExec(ctx, db, prepare, docker.Streams{}, "preparing the private staging folder")
}

func clickHouseStageName() string { return "archive-" + rand.Text() + ".zip" }

func (r *Runner) clearClickHouseStage(ctx context.Context, db *database, file string) error {
	return r.clickHouseExec(ctx, db, []string{"rm", "-f", path.Join(clickHouseStages, file)}, docker.Streams{}, "removing the staged archive")
}

// roomForClickHouse checks peak space while the native and agent copies coexist.
func (r *Runner) roomForClickHouse(ctx context.Context, t volume.Target) error {
	ctx, cancel := context.WithTimeout(ctx, measureWait)
	defer cancel()
	v, err := r.tool(ctx, t, "measure", docker.Streams{})
	if err != nil {
		return fmt.Errorf("can't measure the disk holding ClickHouse, so the backup didn't start. Check that the data volume is readable: %w", err)
	}
	d, err := r.disk()
	if err != nil {
		return err
	}
	needs := v.Bytes + v.Bytes/margin
	sameDisk := v.Filesystem != "" && v.Filesystem == d.filesystem
	// The encrypted input must support seeking while the verified tar is written.
	agentNeeds := 2 * needs
	if sameDisk {
		agentNeeds += needs
		d.free = min(d.free, v.Free)
	}
	if held := r.drillHeld(d.filesystem); held > 0 && d.free-agentNeeds-held < d.reserve {
		r.yieldDrill(d.filesystem)
		if d, err = r.disk(); err != nil {
			return err
		}
	}
	if !sameDisk && v.Free-needs < min(max(v.Size/20, minReserve), maxReserve) {
		return fmt.Errorf("ClickHouse's data disk has %s free and staging needs about %s plus its free-space reserve, so the backup didn't start. Free space on the data disk and try again",
			human.Size(v.Free), human.Size(needs))
	}
	if d.free-agentNeeds < d.reserve {
		return tooFull(d, agentNeeds)
	}
	return nil
}

func (r *Runner) clickHouseSQL(ctx context.Context, db *database, sql string, out io.Writer, activity string) error {
	return r.clickHouseExec(ctx, db, db.tool(clickHouseCommand("--multiquery", "--format=TabSeparated")), docker.Streams{Stdin: strings.NewReader(sql), Stdout: out}, activity)
}

func (r *Runner) clickHouseRestoreAccess(ctx context.Context, db *database) error {
	if !db.running {
		return errors.New("ClickHouse is stopped, so Dockwell can't verify the administrator can restore its users. Start ClickHouse and plan again, or restore into a new database")
	}
	out := &tail{}
	if err := r.clickHouseSQL(ctx, db, "CHECK GRANT ACCESS MANAGEMENT ON *.*", out, "checking permissions to restore users"); err != nil {
		return err
	}
	if strings.TrimSpace(out.String()) != "1" {
		return errors.New("ClickHouse's administrator can't restore users, so the backup can't replace this database. Give the administrator access_management and plan again")
	}
	return nil
}

func (r *Runner) clickHouseArchiveSQL(ctx context.Context, db *database, sql, password string, out io.Writer, activity string) error {
	// Old releases also use this truncated text in the process list and text log.
	cmd := db.tool(clickHouseCommand("--multiquery", "--format=TabSeparated", "--log_queries=0", "--log_query_threads=0",
		"--log_query_views=0", "--log_query_settings=0", "--log_queries_cut_to_length=16", "--send_logs_level=none"))
	err := r.clickHouseExec(ctx, db, cmd, docker.Streams{Stdin: strings.NewReader(sql), Stdout: out}, activity)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New(strings.ReplaceAll(err.Error(), password, "[hidden]"))
}

func (r *Runner) clickHouseExec(ctx context.Context, db *database, cmd []string, streams docker.Streams, activity string) error {
	said := &tail{}
	streams.Stderr = said
	code, err := r.Client.Exec(ctx, db.id, cmd, streams)
	if err != nil {
		return fmt.Errorf("%s in ClickHouse: %w", activity, err)
	}
	if code != 0 {
		detail := scrubbed(said.String())
		for line := range strings.Lines(said.String()) {
			if strings.HasPrefix(line, "Code: ") {
				detail = scrubbed(line)
				break
			}
		}
		return fmt.Errorf("ClickHouse stopped while %s: %s (exit %d)", activity, detail, code)
	}
	return nil
}

func (r *Runner) clickHouseDump(ctx context.Context, db *database, w io.Writer) (_ int64, err error) {
	file := clickHouseStageName()
	password := rand.Text() + rand.Text()
	defer func() {
		clean, cancel := resource.Finishing(ctx, 30*time.Second)
		defer cancel()
		err = errors.Join(err, r.clearClickHouseStage(clean, db, file))
	}()
	resource.Progress(ctx, "Staging a ClickHouse backup")
	names := &capture{}
	const list = "SELECT name FROM system.databases WHERE lower(name) NOT IN ('system', 'information_schema') ORDER BY name FORMAT JSONEachRow"
	if err := r.clickHouseSQL(ctx, db, list, names, "listing the databases"); err != nil {
		return 0, err
	}
	objects := make([]string, 0)
	for line := range strings.Lines(names.String()) {
		var row struct{ Name string }
		if err := json.Unmarshal([]byte(line), &row); err != nil || row.Name == "" {
			return 0, errors.New("ClickHouse returned an unreadable database name, so the backup stopped. Check the server and try again")
		}
		objects = append(objects, "DATABASE "+clickHouseIdentifier(row.Name))
	}
	// SQL-managed users, their password hashes and the grants that protect each
	// database travel with its data. XML users are part of the server's config.
	objects = append(objects, "TABLE system.users", "TABLE system.roles", "TABLE system.grants", "TABLE system.role_grants",
		"TABLE system.row_policies", "TABLE system.settings_profiles", "TABLE system.quotas")
	taken := &capture{}
	query := "BACKUP " + strings.Join(objects, ", ") + " TO File('dockwell-stage/" + file + "') SETTINGS password='" + password + "', compression_method='store'"
	if err := r.clickHouseArchiveSQL(ctx, db, query, password, taken, "creating the native archive"); err != nil {
		return 0, err
	}
	if !strings.HasSuffix(strings.TrimSpace(taken.String()), "\tBACKUP_CREATED") {
		return 0, errors.New("ClickHouse did not report a completed archive, so no backup was kept. Check the database and try again")
	}
	resource.Progress(ctx, "Copying the ClickHouse archive to backup storage")
	staged, err := os.CreateTemp(r.Root, ".clickhouse-*.zip")
	if err != nil {
		return 0, err
	}
	defer staged.Close()
	// On Linux the open file remains readable, but a killed agent leaves no copy.
	if err := os.Remove(staged.Name()); err != nil {
		return 0, err
	}
	encrypted := &counter{w: staged, check: r.keepRoom}
	cmd := []string{"sh", "-ec", `test -f "$1" && test ! -L "$1" && exec cat "$1"`, "sh", path.Join(clickHouseStages, file)}
	if err := r.clickHouseExec(ctx, db, cmd, docker.Streams{Stdout: encrypted}, "copying the native archive"); err != nil {
		return 0, err
	}
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return 0, err
	}
	plain := &counter{w: gz}
	err = clickHouseZipToTar(ctx, staged, encrypted.n, password, plain)
	return plain.n, errors.Join(err, gz.Close())
}

// The backup keeps its native layout without keeping the stage password.
func clickHouseZipToTar(ctx context.Context, src io.ReaderAt, size int64, password string, dst io.Writer) error {
	archive, err := zip.NewReader(src, size)
	if err != nil {
		return fmt.Errorf("can't open the staged ClickHouse archive: %w", err)
	}
	tw := tar.NewWriter(dst)
	for _, f := range archive.File {
		if f.FileInfo().IsDir() {
			continue
		}
		if !f.IsEncrypted() || !fs.ValidPath(f.Name) || !f.Mode().IsRegular() || f.UncompressedSize64 >= 1<<63 {
			return errors.New("ClickHouse wrote an invalid or unencrypted backup entry")
		}
		if err := tw.WriteHeader(&tar.Header{Name: f.Name, Mode: 0o600, Size: int64(f.UncompressedSize64)}); err != nil { //nolint:gosec // sizes above MaxInt64 were refused
			return err
		}
		f.SetPassword(password)
		f.DeferAuth = true // Each entry is checked before the backup is published.
		entry, err := f.Open()
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, ctxReader{ctx, entry})
		if err := errors.Join(err, entry.Close()); err != nil {
			return err
		}
	}
	return tw.Close()
}

func clickHouseTarToZip(ctx context.Context, src io.Reader, dst io.Writer, password string) error {
	tr := tar.NewReader(ctxReader{ctx, src})
	zw := zip.NewWriter(dst)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return zw.Close()
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg || !fs.ValidPath(h.Name) {
			return errors.New("the ClickHouse backup contains an invalid archive entry")
		}
		entry := &zip.FileHeader{Name: h.Name, Method: zip.Store}
		entry.SetPassword(password)
		entry.SetEncryptionMethod(zip.AES256Encryption)
		out, err := zw.CreateHeader(entry)
		if err != nil {
			return err
		}
		if _, err := io.CopyN(out, tr, h.Size); err != nil {
			return err
		}
	}
}

func clickHouseIdentifier(name string) string {
	return "`" + strings.NewReplacer(`\`, `\\`, "`", "\\`").Replace(name) + "`"
}

func verifyClickHouse(in io.Reader) error {
	gz, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	defer gz.Close()
	t := tar.NewReader(gz)
	for {
		h, err := t.Next()
		switch {
		case errors.Is(err, io.EOF):
			return errors.New("the ClickHouse archive has no .backup metadata")
		case err != nil:
			return err
		case h.Name == ".backup" && h.Typeflag == tar.TypeReg && h.Size > 0:
			// ClickHouse writes this after its data. Read through the gzip footer too.
			if _, err := io.CopyN(io.Discard, t, h.Size); err != nil {
				return err
			}
			_, err := io.CopyN(io.Discard, gz, 1<<20)
			switch {
			case errors.Is(err, io.EOF):
				return nil
			case err != nil:
				return err
			default:
				return errors.New("unexpected data after the ClickHouse archive metadata")
			}
		}
	}
}

func (r *Runner) clickHouseLoad(ctx context.Context, db *database, plain io.Reader) (err error) {
	if err := r.clickHouseStage(ctx, db); err != nil {
		return err
	}
	file := clickHouseStageName()
	password := rand.Text() + rand.Text()
	defer func() {
		clean, cancel := resource.Finishing(ctx, 30*time.Second)
		defer cancel()
		err = errors.Join(err, r.clearClickHouseStage(clean, db, file))
	}()
	cmd := []string{"sh", "-ec", `
		umask 077
		if [ "$(id -u)" = 0 ]; then owner="$(stat -c %u:%g "$2")"; else owner=; fi
		set -C
		cat > "$1/$3"
		if [ -n "$owner" ]; then chown "$owner" "$1/$3"; fi
		`, "sh", clickHouseStages, clickHouseData, file}
	pr, pw := io.Pipe()
	defer pr.Close()
	go func() { _ = pw.CloseWithError(clickHouseTarToZip(ctx, plain, pw, password)) }()
	if err := r.clickHouseExec(ctx, db, cmd, docker.Streams{Stdin: pr}, "staging the archive to restore"); err != nil {
		return err
	}
	out := &capture{}
	query := "RESTORE ALL FROM File('dockwell-stage/" + file + "') SETTINGS password='" + password + "'"
	if err := r.clickHouseArchiveSQL(ctx, db, query, password, out, "restoring the native archive"); err != nil {
		return err
	}
	if !strings.HasSuffix(strings.TrimSpace(out.String()), "\tRESTORED") {
		return errors.New("ClickHouse did not report a completed restore. Check the database and try another backup")
	}
	return nil
}

func clickHouseNativePort(cmd []string) string {
	e := engines.Recognise("clickhouse/clickhouse-server")
	if p := engines.PortOf(e, cmd); p != 0 {
		return strconv.FormatUint(uint64(p), 10)
	}
	return ""
}

// The loader has no network and uses its local default account. The real
// service sets its own hashed administrator credentials when it starts.
func clickHouseLoader(req *docker.CreateRequest) {
	req.Env = []string{"CLICKHOUSE_SKIP_USER_SETUP=1"}
	req.User = ""
	req.Cmd = nil
	req.Entrypoint = []string{"sh", "-ec", `
		cat > /etc/clickhouse-server/users.d/dockwell-restore.xml <<'XML'
<clickhouse><users><default><access_management>1</access_management></default></users></clickhouse>
XML
		{
			printf '<clickhouse><logger><level>warning</level><console>true</console></logger>'
			for name in query query_thread query_views query_metric metric asynchronous_metric trace text part session error iceberg_metadata opentelemetry_span crash processors_profile asynchronous_insert backup blob_storage zookeeper zookeeper_connection s3queue azure_queue filesystem_cache latency transactions_info; do
				printf '<%s_log remove="remove"/>' "$name"
			done
			printf '</clickhouse>\n'
		} > /etc/clickhouse-server/config.d/zz-dockwell-restore.xml
		exec /entrypoint.sh -- --listen_host=127.0.0.1 --background_schedule_pool_size=16 --max_server_memory_usage=335544320 --mark_cache_size=33554432
	`}
	req.HostConfig.Memory = 512 << 20
	req.HostConfig.NanoCPUs = 1e9
}
