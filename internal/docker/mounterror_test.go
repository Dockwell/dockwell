// SPDX-License-Identifier: AGPL-3.0-only

package docker

import "testing"

// What Docker says when a volume won't mount spells out the mount. Only
// the volume and the reason may stay.
func TestAFailedMountKeepsOnlyItsReason(t *testing.T) {
	const at = "error while mounting volume '/var/lib/docker/volumes/shop_share/_data': failed to mount local volume: "
	for name, message := range map[string]string{
		"20.10 prints every option":  at + "mount //nas/share:/var/lib/docker/volumes/shop_share/_data, data: username=svc,password=hunter2: permission denied",
		"later versions miss pass=":  at + "mount //nas/share:/var/lib/docker/volumes/shop_share/_data, data: username=svc,password=********,pass=hunter2: permission denied",
		"the source is never masked": at + "mount //svc:hunter2@nas/share:/var/lib/docker/volumes/shop_share/_data, data: guest: permission denied",
		"flags come before data":     at + "mount //nas/share:/var/lib/docker/volumes/shop_share/_data, flags: 0x1, data: password=hunter2: permission denied",
		"a password with separators": at + "mount //nas/share:/x, data: password=a,b: hunter2: c\nd: permission denied",
	} {
		t.Run(name, func(t *testing.T) {
			want := at + "permission denied"
			if got := scrubMount(message); got != want {
				t.Errorf("scrubMount = %q, want %q", got, want)
			}
		})
	}

	for _, kept := range []string{
		"No such image: nginx:1",
		"driver failed programming external connectivity on endpoint web: Bind for 0.0.0.0:80 failed: port is already allocated",
		"failed to mount local volume",
	} {
		if got := scrubMount(kept); got != kept {
			t.Errorf("scrubMount(%q) = %q, want it as it is", kept, got)
		}
	}
}
