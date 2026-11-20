// SPDX-License-Identifier: AGPL-3.0-only

package service_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/backup"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// realEngine is what a restore into a new database runs on a real engine: the
// image, where it keeps its data, its root password's variable, how a query runs
// over TCP with that password, the SQL that makes the rows, and the line a dump of
// it ends near, which a damaged copy is cut in before.
type realEngine struct {
	image, data, password string
	query                 func(sql string) []string
	setup, add, count     string
	ends                  string
}

// A backup of a real PostgreSQL restores into a new database, which starts on the
// backup's rows and signs in with the original's password, and a restore that
// fails partway leaves the new volume empty; the original's data, revision,
// container and start time are as they were through both (ADR 0106).
func TestARestoreIntoANewPostgresLeavesTheOriginalOnRealDocker(t *testing.T) {
	restoreIntoNewOnRealDocker(t, realEngine{
		image: "postgres:17-alpine", data: "/var/lib/postgresql/data", password: "POSTGRES_PASSWORD",
		query: func(sql string) []string {
			return []string{"sh", "-c", `PGPASSWORD="$POSTGRES_PASSWORD" exec psql -X -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -U postgres -d postgres -tA -c "$1"`, "sh", sql}
		},
		setup: "CREATE TABLE orders (id int); INSERT INTO orders VALUES (1)",
		add:   "INSERT INTO orders VALUES (2)",
		count: "SELECT string_agg(id::text, ',' ORDER BY id) FROM orders",
		ends:  "-- PostgreSQL database cluster dump complete",
	})
}

// The same on a real MariaDB, whose dump carries its users and their passwords.
func TestARestoreIntoANewMariaDBLeavesTheOriginalOnRealDocker(t *testing.T) {
	restoreIntoNewOnRealDocker(t, realEngine{
		image: "mariadb:11.4", data: "/var/lib/mysql", password: "MARIADB_ROOT_PASSWORD",
		query: func(sql string) []string {
			return []string{"sh", "-c", `MYSQL_PWD="$MARIADB_ROOT_PASSWORD" exec mariadb --protocol=TCP -h127.0.0.1 -uroot -N -B -e "$1"`, "sh", sql}
		},
		setup: "CREATE DATABASE shop; CREATE TABLE shop.orders (id int); INSERT INTO shop.orders VALUES (1)",
		add:   "INSERT INTO shop.orders VALUES (2)",
		count: "SELECT GROUP_CONCAT(id ORDER BY id) FROM shop.orders",
		ends:  "-- Dump completed",
	})
}

func restoreIntoNewOnRealDocker(t *testing.T, x realEngine) {
	client := dockertest.Real(t)
	ctx := t.Context()
	if err := client.PullImage(ctx, x.image, nil); err != nil {
		t.Fatal(err)
	}
	binary := agentBuild(t)
	env, restored := dockertest.Scope(t), dockertest.Scope(t)
	for _, scope := range []string{env, restored} {
		dockertest.Cleanup(t, client, docker.Project(scope))
	}
	from := resource.Ref{Kind: volume.Kind, Scope: env, Name: "data"}
	into := resource.Ref{Kind: volume.Kind, Scope: restored, Name: "data"}
	for _, v := range []resource.Ref{from, into} {
		target := volume.TargetOf(v)
		if _, err := client.CreateVolume(ctx, &docker.VolumeCreate{Name: target.Name, Labels: map[string]string{docker.LabelProject: target.Project, docker.LabelVolume: v.Name}}); err != nil {
			t.Fatal(err)
		}
	}
	rt := &container.Docker{Client: client, Settle: 100 * time.Millisecond, Poll: 100 * time.Millisecond, Soak: -1, HealthWait: 2 * time.Minute, ListenWait: time.Minute}
	runner := &backup.Runner{Client: client, Root: t.TempDir(), Binary: binary}
	local := resource.Local{Drivers: resource.NewRegistry(&service.Driver{Containers: container.Driver{Runtime: rt}, Movers: runner})}
	spec := &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: x.image, Env: map[string]string{x.password: "the-original's-" + hex.EncodeToString([]byte(t.Name()))[:8]},
		Mounts: []*resourcev1.Mount{{Source: &resourcev1.Mount_Volume{Volume: "data"}, Target: x.data}},
	}}}
	original := resource.Ref{Kind: service.Kind, Scope: env, Name: "db"}
	steps, err := local.Plan(ctx, []resource.Desired{{Ref: original, Spec: spec}})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Apply(resource.WithDeploy(ctx, resource.Deploy{Revision: 1, Change: "chg_original"}), steps[0], spec); err != nil {
		t.Fatal(err)
	}

	of := func(ref resource.Ref) *docker.Container {
		t.Helper()
		list, err := client.ListContainers(ctx, map[string][]string{"label": {docker.LabelProject + "=" + docker.Project(ref.Scope), docker.LabelService + "=db"}})
		if err != nil || len(list) != 1 {
			t.Fatalf("%s's db: %v, %v", ref.Scope, list, err)
		}
		c, err := client.InspectContainer(ctx, list[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	// query runs SQL in a container over TCP, signing in with its own password, as
	// an app would, waiting through the database's first start.
	query := func(c *docker.Container, sql string) string {
		t.Helper()
		var said string
		for range 120 {
			var out, errs bytes.Buffer
			qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			code, err := client.Exec(qctx, c.ID, x.query(sql), docker.Streams{Stdout: &out, Stderr: &errs})
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			if code == 0 {
				return strings.TrimSpace(out.String())
			}
			said = errs.String()
			time.Sleep(time.Second)
		}
		t.Fatalf("%s: %s", sql, said)
		return ""
	}
	db := of(original)
	query(db, x.setup)
	backupID := id.New(id.Backup)
	if m, err := runner.Take(ctx, backup.Take{ID: backupID, Ref: from}); err != nil || m.Method != resource.BackupDump {
		t.Fatalf("the backup = %+v, %v", m, err)
	}
	query(db, x.add)
	before := of(original)
	unchanged := func(when string) {
		t.Helper()
		now := of(original)
		if now.ID != before.ID || !now.State.StartedAt.Equal(before.State.StartedAt) || !now.State.Running ||
			now.Config.Labels[docker.LabelRevision] != before.Config.Labels[docker.LabelRevision] || now.Config.Labels[docker.LabelConfigHash] != before.Config.Labels[docker.LabelConfigHash] {
			t.Errorf("%s, the original went from %s started %s (%v) to %s started %s (%v)", when, before.ID, before.State.StartedAt, before.Config.Labels, now.ID, now.State.StartedAt, now.Config.Labels)
		}
		if got := query(now, x.count); got != "1,2" {
			t.Errorf("%s, the original holds %q", when, got)
		}
	}

	db2 := resource.Ref{Kind: service.Kind, Scope: restored, Name: "db"}
	seed := func(backupID, change string) error {
		source := &resource.RestoreSource{Backup: backupID, From: from, Change: change}
		steps, err := local.Plan(ctx, []resource.Desired{{Ref: db2, Spec: spec, Restore: source}})
		if err != nil {
			return err
		}
		if r := steps[0].Restore; r == nil || !r.IntoNew || len(r.Stops) != 0 {
			t.Fatalf("the plan = %+v", steps[0].Decision)
		}
		applyCtx := resource.WithRestore(resource.WithDeploy(ctx, resource.Deploy{Revision: 1, Change: change}), *source)
		return local.Apply(applyCtx, steps[0], spec)
	}

	// A copy of the backup that fails near its end, after its rows have loaded, as
	// a disk that fills would leave it: the new volume is emptied again.
	broken := damagedCopy(t, runner.Root, from, backupID, x.ends)
	if err := seed(broken, "chg_broken"); err == nil {
		t.Fatal("a restore whose load failed went on")
	}
	unchanged("after a restore that failed")
	if list, err := client.ListContainers(ctx, map[string][]string{"label": {docker.LabelProject + "=" + docker.Project(restored)}}); err != nil || len(list) != 0 {
		t.Errorf("a failed restore left containers %v, %v", list, err)
	}
	if n := volumeHolds(t, client, x.image, into); n != 0 {
		t.Errorf("a failed restore left %d files in the new volume", n)
	}

	if err := seed(backupID, "chg_restore"); err != nil {
		t.Fatalf("restoring: %v", err)
	}
	made := of(db2)
	if got := query(made, x.count); got != "1" {
		t.Errorf("the new database holds %q, want the backup's row alone", got)
	}
	unchanged("after the restore")
}

// damagedCopy is a copy of a dump that fails as it loads, near its end, after
// everything before has loaded: a statement that fails, ahead of the line it ends
// near. Its manifest says it's whole, as the check before the load reads it, so
// only the load finds it.
func damagedCopy(t *testing.T, root string, from resource.Ref, backupID, ends string) string {
	t.Helper()
	dir := filepath.Join(root, from.Scope, from.Name)
	raw, err := os.ReadFile(filepath.Join(dir, backupID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	format, _ := m["format"].(string)
	f, err := os.Open(filepath.Join(dir, backupID+"."+format))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.LastIndex(plain, []byte(ends))
	if at < 0 {
		t.Fatalf("the dump has no %q", ends)
	}
	damaged := append(append(append([]byte{}, plain[:at]...), "SELECT * FROM dockwell_no_such_table;\n"...), plain[at:]...)
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := w.Write(damaged); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	other := id.New(id.Backup)
	sum := sha256.Sum256(out.Bytes())
	m["id"], m["size"], m["sha256"] = other, out.Len(), hex.EncodeToString(sum[:])
	raw, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, other+"."+format), out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, other+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return other
}

// volumeHolds is how many files and folders a volume holds, as a container of its
// own on no network counts them, with the image the test pulled.
func volumeHolds(t *testing.T, client *docker.Client, image string, ref resource.Ref) int {
	t.Helper()
	target := volume.TargetOf(ref)
	req := &docker.CreateRequest{
		Config:     docker.Config{Image: image, Entrypoint: []string{"sleep", "300"}, Labels: map[string]string{docker.LabelProject: target.Project}},
		HostConfig: docker.HostConfig{NetworkMode: "none", Mounts: []docker.Mount{{Type: "volume", Source: target.Name, Target: "/v", ReadOnly: true}}},
	}
	cid, err := client.CreateContainer(t.Context(), target.Project+"-count", req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.RemoveContainer(context.WithoutCancel(t.Context()), cid, true) }()
	if err := client.StartContainer(t.Context(), cid); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code, err := client.Exec(t.Context(), cid, []string{"sh", "-c", "find /v -mindepth 1 | wc -l"}, docker.Streams{Stdout: &out, Stderr: &errs}); err != nil || code != 0 {
		t.Fatalf("counting what %s holds: %d, %v: %s", target.Name, code, err, errs.String())
	}
	n, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// agentBuild builds the agent, static, as the helper's container runs it.
func agentBuild(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "dockwell")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-tags", "agent", "-o", out, "github.com/dockwell/dockwell/cmd/dockwell")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if said, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the agent: %v\n%s", err, said)
	}
	return out
}
