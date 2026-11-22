// SPDX-License-Identifier: AGPL-3.0-only

package resource

import "testing"

// Access kinds are persisted as integers in stored plans.
func TestStoredAccessKindValues(t *testing.T) {
	for kind, want := range map[AccessKind]int{
		AccessPath:               1,
		AccessDocker:             2,
		AccessNetwork:            3,
		AccessMount:              4,
		AccessNetworkDriver:      5,
		AccessDomain:             6,
		AccessPanelDomain:        7,
		AccessExternalNetwork:    8,
		AccessCapability:         9,
		AccessGroup:              10,
		AccessPrivileged:         11,
		AccessServerPort:         12,
		AccessTrustedProxy:       13,
		AccessPlainForAnyone:     14,
		AccessHTTPSOff:           15,
		AccessExternalVolume:     16,
		AccessTakesOn:            17,
		AccessBackupOf:           18,
		AccessServerShell:        19,
		AccessCertificateKey:     20,
		AccessOpened:             21,
		AccessDevice:             22,
		AccessGPUs:               23,
		AccessSecurityOption:     24,
		AccessHostProcesses:      25,
		AccessRootTogether:       26,
		AccessPublicPort:         27,
		AccessTunnelRule:         28,
		AccessCloudflareTakeover: 29,
		AccessZoneMode:           30,
		AccessDockerDaemon:       31,
		AccessDockerImagePush:    32,
	} {
		if int(kind) != want {
			t.Errorf("stored access kind %d was renumbered to %d", want, kind)
		}
	}
}
