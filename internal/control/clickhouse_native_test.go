// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/store"
)

func TestClickHouseNativeBackupRestoresIntoNewOnRealDocker(t *testing.T) {
	d := dockertest.Real(t)
	const entryID, project = "clickhouse-native-fixture", "clickhouse-source"
	const oldImage = "clickhouse/clickhouse-server:25.8.33.6-alpine@sha256:87e0a5b72f5465b18eacca7c76850e7ff551c9795c50e451f5646299e5e24146"
	dir := t.TempDir()
	useCatalogDir(t, dir)
	manifest, err := os.ReadFile(filepath.Join("..", "catalog", "clickhouse.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	updatedEntry := strings.Replace(string(manifest), "id: clickhouse\n", "id: "+entryID+"\n", 1)
	entry, err := catalog.Parse([]byte(updatedEntry))
	if err != nil {
		t.Fatal(err)
	}
	pins := catalog.PinnedDigests(entry.GetCompose())
	if len(pins) != 1 {
		t.Fatalf("want one ClickHouse image, got %v", pins)
	}
	var newImage string
	for tag, digest := range pins {
		newImage = tag + "@" + digest
	}
	writeEntry := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, entryID+".yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeEntry(strings.NewReplacer(newImage, oldImage, `version: "26.3.62.3"`, `version: "25.8.33.6"`, "release: 2", "release: 1").Replace(updatedEntry))
	helper := filepath.Join(t.TempDir(), "dockwell")
	build := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-tags", "agent", "-o", helper, "./cmd/dockwell")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build backup helper: %v\n%s", err, out)
	}
	f := storageControl(t, store.SQLite, d, helper)
	cat := catalogClient(f.person)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	for _, image := range []string{oldImage, newImage} {
		if err := d.PullImage(ctx, image, nil); err != nil {
			t.Fatalf("pull ClickHouse: %v", err)
		}
	}
	applyNative := func(ch *v1.Change) {
		t.Helper()
		w := watchWithin(t, f.changes, ch.GetId(), 5*time.Minute)
		if _, err := applyChange(t, f.changes, &v1.ApplyChangeRequest{Id: ch.GetId(), Fingerprint: ch.GetPlan().GetFingerprint()}); err != nil {
			t.Fatal(err)
		}
		if got := waitWatchWithin(t, w, 5*time.Minute).last(); got.GetState() != v1.ChangeState_CHANGE_STATE_APPLIED {
			t.Fatalf("%s ended %v: %s (%v)", ch.GetSummary(), got.GetState(), got.GetError(), got.GetSteps())
		}
	}
	original := installApp(t, cat, &v1.InstallCatalogEntryRequest{
		Id: entryID, Project: project, Servers: []string{f.server.id.ServerID},
	})
	source := storageScope(t, original, "service", "db")
	volume := storageScope(t, original, "volume", "data")
	env := source.GetResource().GetScope()
	removeStorageFixture(t, d, env, []string{"default"}, []string{"data"})
	applyNative(original)
	db := databaseClient(f.person, f.person.origin)
	user, err := db.CreateDatabaseUser(t.Context(), connect.NewRequest(&v1.CreateDatabaseUserRequest{
		Id: source.GetId(), User: "fixture_user", Database: "events",
	}))
	if err != nil {
		t.Fatal(err)
	}
	applyNative(user.Msg.GetChange())
	account := storageScope(t, user.Msg.GetChange(), "database.user", "fixture_user")

	containerOf := func(scope string) string {
		t.Helper()
		c, err := d.ServiceContainer(t.Context(), docker.Project(scope), "db")
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	sourceID := containerOf(env)
	query := func(ctx context.Context, id, sql string) (string, error) {
		var out, said bytes.Buffer
		code, err := d.Exec(ctx, id, []string{"clickhouse-client", "--multiquery"}, docker.Streams{
			Stdin: strings.NewReader(sql), Stdout: &out, Stderr: &said,
		})
		if err != nil {
			return "", fmt.Errorf("running ClickHouse client: %w", err)
		}
		if code != 0 {
			return "", fmt.Errorf("query exited %d: %s", code, said.String())
		}
		return strings.TrimSpace(out.String()), nil
	}
	mustQuery := func(id, sql string) string {
		t.Helper()
		out, err := query(t.Context(), id, sql)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	mustQuery(sourceID, "CREATE TABLE events.rows (n UInt64) ENGINE=MergeTree ORDER BY n; INSERT INTO events.rows SELECT number FROM numbers(50000);")
	mustQuery(sourceID, "CREATE DATABASE other_app; CREATE TABLE other_app.rows (n UInt64) ENGINE=MergeTree ORDER BY n; INSERT INTO other_app.rows VALUES (7);")
	vars := variablesClient(f.person, f.person.origin)
	passwordID := storageScope(t, user.Msg.GetChange(), "secret", "FIXTURE_USER_DATABASE_PASSWORD").GetId()
	password := func() string {
		t.Helper()
		res, err := vars.RevealSecret(t.Context(), connect.NewRequest(&v1.RevealSecretRequest{Id: passwordID, Proof: stepUp(t, f.person, passwordID)}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.GetValue()
	}
	appQuery := func(password, sql string) (int, string) {
		t.Helper()
		var out, stderr bytes.Buffer
		cmd := []string{"sh", "-ec", `IFS= read -r CLICKHOUSE_PASSWORD; export CLICKHOUSE_PASSWORD; exec clickhouse-client --user=fixture_user --multiquery`}
		code, err := d.Exec(t.Context(), sourceID, cmd, docker.Streams{
			Stdin: strings.NewReader(password + "\n" + sql), Stdout: &out, Stderr: &stderr,
		})
		if err != nil {
			t.Fatal(err)
		}
		return code, strings.TrimSpace(out.String() + stderr.String())
	}
	before := password()
	if code, out := appQuery(before, "SELECT count() FROM events.rows"); code != 0 || out != "50000" {
		t.Fatalf("the scoped user couldn't read its own rows: %d %s", code, out)
	}
	if code, out := appQuery(before, "SELECT n FROM other_app.rows"); code == 0 || !strings.Contains(out, "ACCESS_DENIED") {
		t.Fatalf("the scoped user could reach another database: %d %s", code, out)
	}
	if code, _ := appQuery(before, "SET max_threads=32"); code == 0 {
		t.Fatal("the scoped user could lift its thread constraint")
	}
	if code, out := appQuery(before, "BACKUP TABLE events.rows TO File('fixture-not-allowed.zip')"); code == 0 || !strings.Contains(out, "ACCESS_DENIED") {
		t.Fatalf("the scoped user could take a native backup: %d %s", code, out)
	}
	rotated, err := db.ChangeDatabasePassword(t.Context(), connect.NewRequest(&v1.ChangeDatabasePasswordRequest{Id: source.GetId(), User: "fixture_user"}))
	if err != nil {
		t.Fatal(err)
	}
	applyNative(rotated.Msg.GetChange())
	if code, out := appQuery(before, "SELECT 1"); code == 0 || !strings.Contains(out, "516") {
		t.Fatalf("the old password was not refused: %d %s", code, out)
	}
	after := password()
	if code, out := appQuery(after, "SELECT count() FROM events.rows"); code != 0 || out != "50000" {
		t.Fatalf("the rotated password didn't open the database: %d %s", code, out)
	}
	const protected = "dockwell-stage/archive-review-fixture.zip"
	mustQuery(sourceID, "BACKUP TABLE other_app.rows TO File('"+protected+"') SETTINGS password='"+rand.Text()+"', compression_method='store'")
	if code, _ := appQuery(after, "RESTORE TABLE other_app.rows AS events.private_restore FROM File('"+protected+"')"); code == 0 {
		t.Fatal("the scoped user could restore a staged archive without its password")
	}
	if got := mustQuery(sourceID, "EXISTS TABLE events.private_restore"); got != "0" {
		t.Fatal("a refused restore created a table")
	}

	bc := backupClient(f.person, f.person.origin)
	writeEntry(updatedEntry)
	upgraded, err := cat.UpgradeCatalogInstall(t.Context(), connect.NewRequest(&v1.UpgradeCatalogInstallRequest{Id: project}))
	if err != nil {
		t.Fatal(err)
	}
	change := upgraded.Msg.GetChange()
	if noteOf(change, "clickhouse_major_upgrade").GetReason() == "" {
		t.Fatal("the ClickHouse upgrade didn't plan a native backup")
	}
	applyNative(change)
	sourceID = containerOf(env)
	if got := mustQuery(sourceID, "SELECT version(); SELECT count() FROM events.rows"); got != "26.3.62.3\n50000" {
		t.Fatalf("the new ClickHouse release didn't retain the rows: %s", got)
	}
	list, err := bc.ListBackups(t.Context(), connect.NewRequest(&v1.ListBackupsRequest{VolumeId: volume.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(list.Msg.GetBackups(), func(b *v1.Backup) bool {
		return b.GetMethod() == v1.BackupMethod_BACKUP_METHOD_DUMP && b.GetDatabase() == "ClickHouse" && b.GetState() == v1.BackupState_BACKUP_STATE_DONE
	}) {
		t.Fatal("the upgrade did not leave a native ClickHouse backup")
	}
	writing, finish := make(chan struct{}), make(chan error, 1)
	stopWriting := make(chan struct{})
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			close(stopWriting)
			if err := <-finish; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
	})
	go func() {
		for next := 50000; ; next += 50000 {
			select {
			case <-stopWriting:
				finish <- nil
				return
			default:
			}
			if _, err := query(t.Context(), sourceID, fmt.Sprintf("INSERT INTO events.rows SELECT %d + number FROM numbers(50000)", next)); err != nil {
				finish <- err
				return
			}
			if next == 50000 {
				close(writing)
			}
		}
	}()
	select {
	case <-writing:
	case err := <-finish:
		stopped = true
		t.Fatal(err)
	case <-time.After(15 * time.Second):
		t.Fatal("the first concurrent insert didn't finish")
	}

	saving, err := bc.CreateBackup(t.Context(), connect.NewRequest(&v1.CreateBackupRequest{VolumeId: volume.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	got := waitBackupWithin(t, bc, saving.Msg.GetBackup().GetId(), 2*time.Minute)
	close(stopWriting)
	stopped = true
	if err := <-finish; err != nil {
		t.Fatal(err)
	}
	if got.GetState() != v1.BackupState_BACKUP_STATE_DONE || got.GetDatabase() != "ClickHouse" || got.GetMethod() != v1.BackupMethod_BACKUP_METHOD_DUMP {
		t.Fatalf("native backup: %v", got)
	}
	last := mustQuery(sourceID, "SELECT max(n) FROM events.rows")
	mustQuery(sourceID, "INSERT INTO events.rows VALUES (999999999)")

	planned, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&v1.RestoreBackupRequest{Id: got.GetId(), Into: "clickhouse-restored"}))
	if err != nil {
		t.Fatal(err)
	}
	restore := planned.Msg.GetChange()
	target := storageScope(t, restore, "service", "db")
	newEnv := target.GetResource().GetScope()
	if env == newEnv {
		t.Fatal("restore selected the source environment")
	}
	removeStorageFixture(t, d, newEnv, []string{"default"}, []string{"data"})
	applyNative(restore)
	targetID := containerOf(newEnv)
	stats := mustQuery(targetID, "SELECT count(), min(n), max(n), sum(n) FROM events.rows")
	var count, minN, maxN, sum, sourceMax uint64
	if _, err := fmt.Sscan(last, &sourceMax); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Sscan(stats, &count, &minN, &maxN, &sum); err != nil ||
		count < 100000 || minN != 0 || maxN+1 != count || maxN > sourceMax || sum != count*(count-1)/2 {
		t.Fatalf("restored snapshot was not a contiguous prefix of the source: %s, source max %s", stats, last)
	}
	if grants := mustQuery(targetID, "SHOW GRANTS FOR fixture_user"); !strings.Contains(grants, "ON events.* TO fixture_user") {
		t.Fatalf("restore dropped the scoped user grant: %s", grants)
	}
	if last := mustQuery(sourceID, "SELECT max(n) FROM events.rows"); last != "999999999" {
		t.Fatalf("restore changed the source: %s", last)
	}
	inPlace, err := bc.RestoreBackup(t.Context(), connect.NewRequest(&v1.RestoreBackupRequest{Id: got.GetId()}))
	if err != nil {
		t.Fatal(err)
	}
	applyNative(inPlace.Msg.GetChange())
	sourceID = containerOf(env)
	if restored := mustQuery(sourceID, "SELECT count(), min(n), max(n), sum(n) FROM events.rows"); restored != stats {
		t.Fatalf("in-place restore: %s, want %s", restored, stats)
	}
	remove := propose(t, f.changes, "Remove the ClickHouse user", &v1.Edit{Resource: account.GetResource(), Delete: true})
	applyNative(remove)
	if last := mustQuery(sourceID, "SELECT max(n) FROM events.rows"); last != strconv.FormatUint(maxN, 10) {
		t.Fatalf("removing the user changed its data: %s", last)
	}
	if _, err := query(t.Context(), sourceID, "SHOW GRANTS FOR fixture_user"); err == nil {
		t.Fatal("the removed account is still in ClickHouse")
	}
	readded, err := db.CreateDatabaseUser(t.Context(), connect.NewRequest(&v1.CreateDatabaseUserRequest{Id: source.GetId(), User: "replacement_user", Database: "events"}))
	if err != nil {
		t.Fatal(err)
	}
	applyNative(readded.Msg.GetChange())
	if grants := mustQuery(sourceID, "SHOW GRANTS FOR replacement_user"); !strings.Contains(grants, "ON events.* TO replacement_user") {
		t.Fatalf("the replacement user didn't receive its kept database: %s", grants)
	}
	if code, err := d.Exec(t.Context(), sourceID, []string{"sh", "-ec", `test -z "$(ls -A /var/lib/clickhouse/backups/dockwell-stage)"`}, docker.Streams{}); err != nil || code != 0 {
		t.Fatalf("staged ClickHouse archive was left behind: %d, %v", code, err)
	}
}
