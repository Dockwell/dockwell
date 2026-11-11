// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	_ "google.golang.org/protobuf/types/known/emptypb" // for the made-up methods' messages

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/authapi"
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
	procedure := "/" + string(sd.FullName()) + "/" + string(md.Name())
	public := slices.Contains(authapi.PublicProcedures(), procedure)
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
	// Before sign-in is the interceptor's list of public calls, and nothing else.
	if (reach == dockwellv1.Reach_REACH_ANYONE) != public {
		say("is reach anyone's %v, but the interceptor's public calls %v it", reach == dockwellv1.Reach_REACH_ANYONE, map[bool]string{true: "hold", false: "don't hold"}[public])
	}
	if (low == dockwellv1.Needs_NEEDS_NOTHING || high == dockwellv1.Needs_NEEDS_NOTHING) && !public && procedure != dockwellv1connect.AuthServiceSignOutProcedure {
		say("needs nothing, which only a call before sign-in, and signing out, may")
	}
	if reach == dockwellv1.Reach_REACH_SELF && sd.FullName() != "dockwell.v1.AuthService" && sd.FullName() != "dockwell.v1.TokenService" {
		say("acts on the caller's own sign-in, which only AuthService and TokenService do")
	}
	if public || reach == dockwellv1.Reach_REACH_SELF {
		return out
	}
	if low == dockwellv1.Needs_NEEDS_VIEWER && effect != dockwellv1.Effect_EFFECT_READ {
		say("a viewer only reads, and this is %v", effect)
	}
	if write && rank[low] < rank[dockwellv1.Needs_NEEDS_DEVELOPER] {
		say("writes, which takes a developer or more")
	}
	if reach == dockwellv1.Reach_REACH_INSTALL && rank[low] < rank[dockwellv1.Needs_NEEDS_ADMIN] {
		say("is the install's own, which is an admin's")
	}
	if reach == dockwellv1.Reach_REACH_NAMED && write {
		say("reads or checks names on the install, so it can't write")
	}
	if reach == dockwellv1.Reach_REACH_SHARED && effect != dockwellv1.Effect_EFFECT_READ {
		say("is the same for everyone, so it only reads")
	}
	if session && rank[high] < rank[dockwellv1.Needs_NEEDS_ADMIN] {
		say("needs a person, so in a protected place it's an admin's")
	}
	return out
}

// rolesTable is every method's options, a line each, checked in, so a change to
// who may call a method shows in review as a changed line.
const rolesTable = "testdata/roles.txt"

var updateRoles = flag.Bool("update-roles", false, "write "+rolesTable+" from the methods' options")

// Each method's options are as the table says, so a method that changes its reach
// or its roles shows in review, and the table is read as the source of who may
// call what.
func TestTheRolesTableIsCurrent(t *testing.T) {
	var lines []string
	protoregistry.GlobalFiles.RangeFilesByPackage("dockwell.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				md := sd.Methods().Get(j)
				opts := md.Options()
				effect, _ := proto.GetExtension(opts, dockwellv1.E_Effect).(dockwellv1.Effect)
				low, _ := proto.GetExtension(opts, dockwellv1.E_MinRole).(dockwellv1.Needs)
				high, _ := proto.GetExtension(opts, dockwellv1.E_MinRoleProtected).(dockwellv1.Needs)
				reach, _ := proto.GetExtension(opts, dockwellv1.E_Reach).(dockwellv1.Reach)
				session, _ := proto.GetExtension(opts, dockwellv1.E_NeedsSession).(bool)
				trim := func(s fmt.Stringer, prefix string) string {
					return strings.ToLower(strings.TrimPrefix(s.String(), prefix))
				}
				line := fmt.Sprintf("%-46s %-12s %-10s %-10s %-8s", string(sd.Name())+"."+string(md.Name()),
					trim(effect, "EFFECT_"), trim(low, "NEEDS_"), trim(high, "NEEDS_"), trim(reach, "REACH_"))
				if session {
					line += " session"
				}
				lines = append(lines, strings.TrimRight(line, " "))
			}
		}
		return true
	})
	slices.Sort(lines)
	got := "# service.method                                effect       min_role   protected  reach    session\n" + strings.Join(lines, "\n") + "\n"
	if *updateRoles {
		if err := os.WriteFile(rolesTable, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(rolesTable)
	if err != nil {
		t.Fatalf("%v. Write it with go test ./internal/control -run TestTheRolesTableIsCurrent -update-roles", err)
	}
	if string(want) != got {
		t.Errorf("%s isn't what the methods say. Check the change is meant, then write it with go test ./internal/control -run TestTheRolesTableIsCurrent -update-roles", rolesTable)
	}
}

// A method can't claim a place in the rules it doesn't have: being a call before
// sign-in, needing nothing, or acting on the caller's own records, outside where
// those are true. These are the review's probes, on made-up methods.
func TestTheRoleRulesHoldForMadeUpMethods(t *testing.T) {
	R, A, D := dockwellv1.Effect_EFFECT_READ, dockwellv1.Effect_EFFECT_APPLY, dockwellv1.Effect_EFFECT_DESTRUCTIVE
	V, N, DEV := dockwellv1.Needs_NEEDS_VIEWER, dockwellv1.Needs_NEEDS_NOTHING, dockwellv1.Needs_NEEDS_DEVELOPER
	for _, c := range []struct {
		svc, name string
		effect    dockwellv1.Effect
		lo, hi    dockwellv1.Needs
		reach     dockwellv1.Reach
		session   bool
	}{
		{"FileService", "WriteBelowDeveloper", A, V, V, dockwellv1.Reach_REACH_PROJECT, false},
		{"FileService", "SessionReadBelowAdmin", R, V, DEV, dockwellv1.Reach_REACH_PROJECT, true},
		{"FileService", "InstallBelowAdmin", R, DEV, DEV, dockwellv1.Reach_REACH_INSTALL, false},
		{"FileService", "ProtectedBelowOpen", R, DEV, V, dockwellv1.Reach_REACH_PROJECT, false},
		{"FileService", "NamedWrites", A, DEV, DEV, dockwellv1.Reach_REACH_NAMED, false},
		{"FileService", "AnyoneReadsOutsideSignIn", R, N, N, dockwellv1.Reach_REACH_ANYONE, false},
		{"FileService", "AnyoneDestroysOutsideSignIn", D, N, N, dockwellv1.Reach_REACH_ANYONE, true},
		{"AuthService", "SelfDestroysNeedingNothing", D, N, N, dockwellv1.Reach_REACH_SELF, false},
		{"OAuthService", "SelfInAnotherService", A, V, V, dockwellv1.Reach_REACH_SELF, false},
	} {
		sd := madeUp(t, c.svc, c.name, func(o *descriptorpb.MethodOptions) {
			proto.SetExtension(o, dockwellv1.E_Effect, c.effect)
			proto.SetExtension(o, dockwellv1.E_MinRole, c.lo)
			proto.SetExtension(o, dockwellv1.E_MinRoleProtected, c.hi)
			proto.SetExtension(o, dockwellv1.E_Reach, c.reach)
			if c.session {
				proto.SetExtension(o, dockwellv1.E_NeedsSession, true)
			}
		})
		if got := roleProblems(sd, sd.Methods().Get(0)); len(got) == 0 {
			t.Errorf("%s.%s passes the rules, and mustn't", c.svc, c.name)
		}
	}
	if got := roleProblems(madeUp(t, "FileService", "NoOptions", func(*descriptorpb.MethodOptions) {}), madeUp(t, "FileService", "NoOptions2", func(*descriptorpb.MethodOptions) {}).Methods().Get(0)); len(got) == 0 {
		t.Error("a method without options passes the rules")
	}
}

// madeUp is a service of one made-up method, with the options set says.
func madeUp(t *testing.T, svc, method string, set func(*descriptorpb.MethodOptions)) protoreflect.ServiceDescriptor {
	t.Helper()
	o := &descriptorpb.MethodOptions{}
	set(o)
	fdp := &descriptorpb.FileDescriptorProto{
		Name: proto.String("madeup/" + svc + method + ".proto"), Package: proto.String("madeup"), Syntax: proto.String("proto3"),
		Dependency: []string{"google/protobuf/empty.proto"},
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: proto.String(svc), Method: []*descriptorpb.MethodDescriptorProto{{
			Name: proto.String(method), InputType: proto.String(".google.protobuf.Empty"), OutputType: proto.String(".google.protobuf.Empty"), Options: o,
		}}}},
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	return fd.Services().Get(0)
}

// A call that carries a secret is never marked as having no side effects. The
// panel's transport sends such calls as GETs, with the request in the address,
// where a claim link's token, an invite's secret, a code or a proof would reach
// a proxy's log and the browser's history. Checking a link is a read, and stays
// a POST for that reason alone (proto/dockwell/v1/auth.proto says so on each).
func TestACallThatCarriesASecretIsNeverAGet(t *testing.T) {
	carriesASecret := map[protoreflect.Name]bool{"token": true, "secret": true, "code": true, "proof": true, "password": true, "credential_json": true}
	seen := 0
	protoregistry.GlobalFiles.RangeFilesByPackage("dockwell.v1", func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			sd := fd.Services().Get(i)
			for j := range sd.Methods().Len() {
				md := sd.Methods().Get(j)
				fields := md.Input().Fields()
				for k := range fields.Len() {
					f := fields.Get(k)
					if !carriesASecret[f.Name()] || f.Kind() != protoreflect.StringKind {
						continue
					}
					seen++
					opts, _ := md.Options().(*descriptorpb.MethodOptions)
					if opts.GetIdempotencyLevel() == descriptorpb.MethodOptions_NO_SIDE_EFFECTS {
						t.Errorf("%s.%s takes %s and is marked NO_SIDE_EFFECTS, so the panel would send it in the address of a GET. Leave idempotency_level off it", sd.Name(), md.Name(), f.Name())
					}
				}
			}
		}
		return true
	})
	for _, must := range []string{"CheckClaim", "CheckInvite", "SignInWithLink"} {
		md := dockwellv1.File_dockwell_v1_auth_proto.Services().ByName("AuthService").Methods().ByName(protoreflect.Name(must))
		if md == nil {
			t.Errorf("AuthService.%s is gone, which this test names", must)
		}
	}
	if seen < 8 {
		t.Errorf("found %d calls that carry a secret, which says the walk missed some", seen)
	}
}
