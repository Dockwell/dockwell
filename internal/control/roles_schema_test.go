// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// rank orders what a method needs, as the roles do.
var rank = map[dockwellv1.Needs]int{
	dockwellv1.Needs_NEEDS_NOTHING: 0, dockwellv1.Needs_NEEDS_VIEWER: 1, dockwellv1.Needs_NEEDS_DEVELOPER: 2,
	dockwellv1.Needs_NEEDS_ADMIN: 3, dockwellv1.Needs_NEEDS_OWNER: 4,
}

// Every method says who may call it, in open places and protected ones, and what
// its objects are, and what it says holds together: a viewer reads, a write is a
// developer's or more, and what's the install's alone is an admin's (ADR 0049).
// A new method fails here until it says.
func TestEveryMethodSaysWhoMayCallIt(t *testing.T) {
	methods := 0
	protoregistry.GlobalFiles.RangeFilesByPackage("dockwell.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				md := sd.Methods().Get(j)
				methods++
				for _, problem := range roleProblems(sd, md) {
					t.Errorf("%s.%s: %s", sd.Name(), md.Name(), problem)
				}
			}
		}
		return true
	})
	if methods < 80 {
		t.Errorf("found %d methods, which says the walk missed some", methods)
	}
}

func roleProblems(sd protoreflect.ServiceDescriptor, md protoreflect.MethodDescriptor) []string {
	opts := md.Options()
	effect, _ := proto.GetExtension(opts, dockwellv1.E_Effect).(dockwellv1.Effect)
	low, _ := proto.GetExtension(opts, dockwellv1.E_MinRole).(dockwellv1.Needs)
	high, _ := proto.GetExtension(opts, dockwellv1.E_MinRoleProtected).(dockwellv1.Needs)
	reach, _ := proto.GetExtension(opts, dockwellv1.E_Reach).(dockwellv1.Reach)
	session, _ := proto.GetExtension(opts, dockwellv1.E_NeedsSession).(bool)
	var out []string
	say := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	if low == dockwellv1.Needs_NEEDS_UNSPECIFIED || high == dockwellv1.Needs_NEEDS_UNSPECIFIED || reach == dockwellv1.Reach_REACH_UNSPECIFIED {
		say("set (dockwell.v1.min_role), (dockwell.v1.min_role_protected) and (dockwell.v1.reach)")
		return out
	}
	if rank[high] < rank[low] {
		say("min_role_protected %v is below min_role %v", high, low)
	}
	write := effect == dockwellv1.Effect_EFFECT_APPLY || effect == dockwellv1.Effect_EFFECT_DESTRUCTIVE
	self := reach == dockwellv1.Reach_REACH_SELF || reach == dockwellv1.Reach_REACH_ANYONE
	switch {
	case low == dockwellv1.Needs_NEEDS_NOTHING && !self:
		say("needs nothing, which only a call before sign-in or on the caller's own sign-in may")
	case reach == dockwellv1.Reach_REACH_ANYONE && low != dockwellv1.Needs_NEEDS_NOTHING:
		say("is before sign-in, so it can't need a role")
	case reach == dockwellv1.Reach_REACH_SELF && !strings.HasSuffix(string(sd.Name()), "AuthService") && !strings.HasSuffix(string(sd.Name()), "TokenService"):
		say("acts on the caller's own sign-in, which only AuthService and TokenService do")
	}
	if !self {
		if low == dockwellv1.Needs_NEEDS_VIEWER && effect != dockwellv1.Effect_EFFECT_READ &&
			(reach != dockwellv1.Reach_REACH_NAMED || effect != dockwellv1.Effect_EFFECT_PLAN) {
			say("a viewer only reads, and this is %v", effect)
		}
		if write && rank[low] < rank[dockwellv1.Needs_NEEDS_DEVELOPER] {
			say("writes, which takes a developer or more")
		}
		if reach == dockwellv1.Reach_REACH_INSTALL && rank[low] < rank[dockwellv1.Needs_NEEDS_ADMIN] {
			say("is the install's own, which is an admin's")
		}
		if reach == dockwellv1.Reach_REACH_NAMED && effect != dockwellv1.Effect_EFFECT_READ && effect != dockwellv1.Effect_EFFECT_PLAN {
			say("reads or checks names on the install, so it can't write")
		}
		if reach == dockwellv1.Reach_REACH_SHARED && effect != dockwellv1.Effect_EFFECT_READ {
			say("is the same for everyone, so it only reads")
		}
		if session && rank[high] < rank[dockwellv1.Needs_NEEDS_ADMIN] {
			say("needs a person, so in a protected place it's an admin's")
		}
	}
	return out
}
