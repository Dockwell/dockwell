// SPDX-License-Identifier: AGPL-3.0-only

package container

// SilentRegistries is how many registries d holds as silent.
func (d *Docker) SilentRegistries() int {
	d.registries.mu.Lock()
	defer d.registries.mu.Unlock()
	return len(d.registries.silent)
}
