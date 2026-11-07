// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// What a container runs with beyond what Docker gives it (ADR 0021). Each is
// named in the plan a person approves, so each is checked against what it can
// be: nothing in a spec gets to write its own words in a plan.

// capabilities are Linux's, without the CAP_ Docker takes them with or without.
var capabilities = map[string]bool{
	"ALL": true, "AUDIT_CONTROL": true, "AUDIT_READ": true, "AUDIT_WRITE": true, "BLOCK_SUSPEND": true, "BPF": true,
	"CHECKPOINT_RESTORE": true, "CHOWN": true, "DAC_OVERRIDE": true, "DAC_READ_SEARCH": true, "FOWNER": true, "FSETID": true,
	"IPC_LOCK": true, "IPC_OWNER": true, "KILL": true, "LEASE": true, "LINUX_IMMUTABLE": true, "MAC_ADMIN": true, "MAC_OVERRIDE": true,
	"MKNOD": true, "NET_ADMIN": true, "NET_BIND_SERVICE": true, "NET_BROADCAST": true, "NET_RAW": true, "PERFMON": true, "SETFCAP": true,
	"SETGID": true, "SETPCAP": true, "SETUID": true, "SYS_ADMIN": true, "SYS_BOOT": true, "SYS_CHROOT": true, "SYS_MODULE": true, "SYS_NICE": true,
	"SYS_PACCT": true, "SYS_PTRACE": true, "SYS_RAWIO": true, "SYS_RESOURCE": true, "SYS_TIME": true, "SYS_TTY_CONFIG": true, "SYSLOG": true,
	"WAKE_ALARM": true,
}

// rootCapabilities reach past what Docker keeps a container in, each by a way
// that is well known: mounting, loading a kernel module, writing to a disk
// directly, or opening any file on the server by its handle.
var rootCapabilities = map[string]bool{"ALL": true, "SYS_ADMIN": true, "SYS_MODULE": true, "SYS_RAWIO": true, "DAC_READ_SEARCH": true}

// rootGroups are the groups that are root's in all but name, or that Docker's
// socket usually belongs to.
var rootGroups = map[string]bool{"0": true, "root": true, "docker": true}

// groupRE is the names and numbers a group can have.
var groupRE = regexp.MustCompile(`^(\d{1,10}|[a-zA-Z_][a-zA-Z0-9_.-]{0,63})$`)

// maxGroups is how many groups a container may join: as many as the kernel lets
// a process have is far more, and no one needs more than this.
const maxGroups = 64

// validatePrivileges checks what a container adds and drops, and puts each list
// in order, in one form.
func validatePrivileges(c *resourcev1.Container) error {
	var err error
	if c.CapAdd, err = validateCapabilities(c.GetCapAdd(), "cap_add"); err != nil {
		return err
	}
	if c.CapDrop, err = validateCapabilities(c.GetCapDrop(), "cap_drop"); err != nil {
		return err
	}
	if n := len(c.GetGroupAdd()); n > maxGroups {
		return fmt.Errorf("the container joins %d groups, and Dockwell takes up to %d. Join fewer", n, maxGroups)
	}
	for _, g := range c.GetGroupAdd() {
		if !groupRE.MatchString(g) {
			return fmt.Errorf("%q isn't a group: give its name or number, such as audio or 999", g)
		}
	}
	slices.Sort(c.GroupAdd)
	c.GroupAdd = slices.Compact(c.GroupAdd)
	return nil
}

// capabilityNames puts capabilities in the form a spec keeps them, and refuses nothing.
func capabilityNames(list []string) []string {
	if list == nil {
		return nil
	}
	out := make([]string, len(list))
	for i, name := range list {
		out[i] = strings.TrimPrefix(strings.ToUpper(name), "CAP_")
	}
	return out
}

// validateCapabilities reads capabilities as Docker does, with or without CAP_,
// in either case. A file's value becomes one of these or is refused.
func validateCapabilities(list []string, field string) ([]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(list))
	for _, name := range list {
		c := strings.TrimPrefix(strings.ToUpper(name), "CAP_")
		if !capabilities[c] {
			return nil, fmt.Errorf("%s has %q, which isn't a Linux capability. Use one such as NET_ADMIN, or ALL", field, name)
		}
		out = append(out, c)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// privilegeAccess is what a container runs with that a person approves. Dropping
// a capability only takes away, so it needs no one.
func privilegeAccess(c *resourcev1.Container) []resource.Access {
	var out []resource.Access
	if c.GetPrivileged() {
		out = append(out, resource.Access{Kind: resource.AccessPrivileged, SameAsRoot: true})
	}
	for _, name := range c.GetCapAdd() {
		out = append(out, resource.Access{Kind: resource.AccessCapability, Path: name, SameAsRoot: rootCapabilities[name]})
	}
	for _, g := range c.GetGroupAdd() {
		out = append(out, resource.Access{Kind: resource.AccessGroup, Path: g, SameAsRoot: rootGroups[g]})
	}
	return out
}
