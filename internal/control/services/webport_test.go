// SPDX-License-Identifier: AGPL-3.0-only

package services

import (
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

func ports(numbers ...uint32) []*resourcev1.PortMapping {
	out := make([]*resourcev1.PortMapping, len(numbers))
	for i, n := range numbers {
		out[i] = &resourcev1.PortMapping{HostPort: 10000 + n, ContainerPort: n, HostIp: "127.0.0.1"}
	}
	return out
}

func curl(words ...string) *resourcev1.Healthcheck {
	return &resourcev1.Healthcheck{Test: append([]string{"CMD"}, words...)}
}

// Add a domain starts from the port that serves an app's pages, never simply its
// first, which was SSH's 22 for an app that listed it before its web port (#791).
func TestTheWebPortIsTheOneThatServesPages(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		app    *resourcev1.Container
		routed uint32
		want   uint32
	}{
		{
			name: "Gitea with SSH first and its health check on 3000",
			app:  &resourcev1.Container{Ports: ports(22, 3000), Healthcheck: curl("curl", "-fsS", "--max-time", "4", "http://127.0.0.1:3000/api/healthz")},
			want: 3000,
		},
		{name: "Gitea with SSH first and no health check", app: &resourcev1.Container{Ports: ports(22, 3000)}, want: 3000},
		{name: "a database's port beside the web's", app: &resourcev1.Container{Ports: ports(5432, 8080)}, want: 8080},
		{
			name: "a health check that names a port it doesn't publish",
			app:  &resourcev1.Container{Ports: ports(8080, 9090), Healthcheck: &resourcev1.Healthcheck{Test: []string{"CMD-SHELL", "wget -qO- localhost:9090/health || exit 1"}}},
			want: 9090,
		},
		{
			name: "an HTTP check the agent makes",
			app:  &resourcev1.Container{Ports: ports(8080), Healthcheck: &resourcev1.Healthcheck{Http: &resourcev1.HttpCheck{Port: 8081, Path: "/ready"}}},
			want: 8081,
		},
		{name: "a health check on http:// with no port", app: &resourcev1.Container{Ports: ports(22, 8080), Healthcheck: curl("curl", "-f", "http://localhost/")}, want: 80},
		{
			name:   "a route already sends to a port, which outranks the rest",
			app:    &resourcev1.Container{Ports: ports(22, 3000), Healthcheck: curl("curl", "http://127.0.0.1:3000/")},
			routed: 8443,
			want:   8443,
		},
		{
			name: "a check that asks some other way names no port",
			app:  &resourcev1.Container{Ports: ports(22, 5432, 8000), Healthcheck: curl("pg_isready", "-h", "localhost", "-p", "5432")},
			want: 8000,
		},
		{
			name: "a bare address read only beside curl or wget",
			app:  &resourcev1.Container{Ports: ports(8000), Healthcheck: curl("nc", "-z", "127.0.0.1:6379")},
			want: 8000,
		},
		{name: "only SSH published", app: &resourcev1.Container{Ports: ports(22)}, want: 22},
		{
			name: "a UDP port is never the web's",
			app:  &resourcev1.Container{Ports: []*resourcev1.PortMapping{{HostPort: 53, ContainerPort: 53, Protocol: resourcev1.Protocol_PROTOCOL_UDP}, {HostPort: 8053, ContainerPort: 80}}},
			want: 80,
		},
		{name: "on the server's own network", app: &resourcev1.Container{HostNetwork: true, Healthcheck: curl("curl", "http://127.0.0.1:8123/")}, routed: 8123, want: 0},
		{name: "nothing published and nothing named", app: &resourcev1.Container{}, want: 0},
	} {
		if got := WebPort(c.app, c.routed); got != c.want {
			t.Errorf("%s: WebPort = %d, want %d", c.name, got, c.want)
		}
	}
}
