// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"io"
	"slices"
	"testing"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
)

func TestClickHouseFromSpec(t *testing.T) {
	for _, tc := range []struct {
		name, image  string
		command      []string
		native, http uint32
	}{
		{"official", "clickhouse:25.8", nil, 9000, 8123},
		{
			"catalog wrapper", "docker.io/clickhouse/clickhouse-server:24.12.6.70-alpine",
			[]string{"sh", "-c", "printf '%s' \"$SERVER_XML\" >/etc/clickhouse-server/config.d/server.xml && exec /entrypoint.sh"},
			9000, 8123,
		},
		{
			"custom endpoints", "clickhouse/clickhouse-server:25.8",
			[]string{"--", "--tcp_port=19000", "--http_port", "18123"},
			19000, 18123,
		},
		{"client", "clickhouse/clickhouse-server:25.8", []string{"clickhouse-client", "--query=SELECT 1"}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found, ok := FromSpec(&spec.Service{
				Runs: &spec.Service_App{App: &spec.Container{
					Image: tc.image, Command: tc.command,
					Mounts: []*spec.Mount{{Source: &spec.Mount_Volume{Volume: "events"}, Target: "/var/lib/clickhouse"}},
				}},
				Secrets: map[string]string{"CLICKHOUSE_PASSWORD": "EVENTS_PASSWORD"},
			})
			if tc.native == 0 {
				if ok {
					t.Fatal("a ClickHouse client was classified as a server")
				}
				return
			}
			if !ok || found.Engine.Kind != v1.DatabaseEngine_DATABASE_ENGINE_CLICKHOUSE || found.Volume != "events" ||
				found.Port != tc.native || found.HTTPPort != tc.http || found.PasswordSecret != "EVENTS_PASSWORD" {
				t.Fatalf("ClickHouse inventory metadata: %+v", found)
			}
		})
	}
}

func TestClickHouseReadsFileLogBounds(t *testing.T) {
	const rotated = "<size>4M</size><count>2</count>"
	for _, tc := range []struct {
		name, config string
		unbounded    bool
	}{
		{"bounded", "<clickhouse><logger><level>warning</level><log>/var/log/clickhouse.log</log>" + rotated + "</logger></clickhouse>", false},
		{"trace", "<clickhouse><logger><level>trace</level><log>/var/log/clickhouse.log</log>" + rotated + "</logger></clickhouse>", true},
		{"debug", "<clickhouse><logger><level>debug</level><errorlog>/var/log/error.log</errorlog>" + rotated + "</logger></clickhouse>", true},
		{"test", "<clickhouse><logger><level><![CDATA[test]]></level><log>/var/log/log</log>" + rotated + "</logger></clickhouse>", true},
		{"console only", "<clickhouse><logger><level>trace</level><console>true</console><log/></logger></clickhouse>", false},
		{"missing size", "<clickhouse><logger><level>warning</level><log>/var/log/log</log><count>2</count></logger></clickhouse>", true},
		{"empty count", "<clickhouse><logger><level>warning</level><log>/var/log/log</log><size>4M</size><count /></logger></clickhouse>", true},
		{"truncated", "<clickhouse><logger><level>trace</level><log>/var/log/log</log></logger>", false},
		{"examples and nested loggers", `<?xml version="1.0"?>
<!-- <logger><level>trace</level><log>example</log></logger> -->
<clickhouse>
  <keeper_server>
    <logger><level>trace</level><log>keeper</log></logger>
  </keeper_server>
  <logger description="level > warning">
    <!-- <level>trace</level> -->
    <level>warning</level>
    <log>/var/log/log</log>
    <size>4M</size>
    <count>2</count>
  </logger>
</clickhouse>`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const image = "clickhouse:25.8"
			const scope = "env_fixture"
			e := dockertest.New(t)
			e.AddLocalImage(image, dockertest.Image{Exec: func(x dockertest.Exec) int {
				switch {
				case slices.Equal(x.Cmd, []string{"cat", "/var/lib/clickhouse/preprocessed_configs/config.xml"}):
					_, _ = io.WriteString(x.Stdout, tc.config)
					return 0
				case slices.Contains(x.Cmd, "--query=SELECT 1"):
					_, _ = io.WriteString(x.Stderr, "AUTHENTICATION_FAILED")
					return 1
				default:
					_, _ = io.WriteString(x.Stdout, `{"version":"25.8.33.6","user":"default"}`+"\n")
					return 0
				}
			}})
			client := e.Client()
			id, err := client.CreateContainer(t.Context(), "events", &docker.CreateRequest{Config: docker.Config{
				Image: image, Labels: map[string]string{docker.LabelProject: docker.Project(scope), docker.LabelService: "events", docker.LabelNumber: "1"},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := client.StartContainer(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			got, err := (Reader{Client: client}).Read(t.Context(), &agentv1.DatabaseCall{Databases: []*agentv1.DatabaseAsked{{
				Service: &v1.ResourceRef{Scope: scope, Name: "events"}, Engine: v1.DatabaseEngine_DATABASE_ENGINE_CLICKHOUSE,
			}}})
			if err != nil {
				t.Fatal(err)
			}
			reading := got.GetReadings()[0]
			if reading.GetUnread() != "" || !reading.GetClickhousePasswordVerified() || reading.GetClickhouseUnboundedLogs() != tc.unbounded {
				t.Fatalf("ClickHouse reading = %v, want unbounded logs %t", reading, tc.unbounded)
			}
		})
	}
}
