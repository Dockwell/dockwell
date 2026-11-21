// SPDX-License-Identifier: AGPL-3.0-only

package dockerdaemon

import (
	"encoding/json"
	"strings"
	"testing"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

func TestSettingsOverlayLeavesUnrelatedValuesIntact(t *testing.T) {
	original, err := decodeConfig([]byte(`{"iptables":false,"log-opts":{"compress":"true"},"experimental":true}`))
	if err != nil {
		t.Fatal(err)
	}
	draft := &spec.DockerDaemon{Mirrors: &spec.DockerRegistryMirrors{}, Logging: &spec.DockerDaemonLogging{Driver: "json-file", MaxSizeBytes: 10 << 20, MaxFiles: 5}}
	got, err := overlay(original, draft)
	if err != nil {
		t.Fatal(err)
	}
	if string(got["registry-mirrors"]) != "[]" || string(got["iptables"]) != "false" || string(got["experimental"]) != "true" {
		t.Fatal(got)
	}
	var opts map[string]string
	if err := json.Unmarshal(got["log-opts"], &opts); err != nil || opts["compress"] != "true" || opts["max-size"] != "10485760" || opts["max-file"] != "5" {
		t.Fatal(opts, err)
	}
	draft.Logging.Driver = "local"
	if _, err := overlay(original, draft); err == nil {
		t.Fatal("switching drivers silently discarded an option")
	}
}

func TestSettingsRejectCredentialsAndReservedPools(t *testing.T) {
	for _, url := range []string{"http://registry.example", "https://owner:secret@registry.example", "https://registry.example/?token=x", "https://registry.example/#hidden"} {
		if err := validate(&spec.DockerDaemon{Mirrors: &spec.DockerRegistryMirrors{Urls: []string{url}}}); err == nil {
			t.Errorf("accepted unsafe mirror %s", url)
		}
	}
	for _, base := range []string{"10.233.0.0/18", "192.0.2.1/24", "127.0.0.0/24"} {
		if err := validate(&spec.DockerDaemon{AddressPools: &spec.DockerDefaultAddressPools{Pools: []*spec.DockerAddressPool{{Base: base, PrefixLength: 25}}}}); err == nil {
			t.Errorf("accepted invalid pool %s", base)
		}
	}
	if _, err := decodeConfig([]byte(`{"hosts":[],"hosts":[]}`)); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatal(err)
	}
}
