// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"testing"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
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
