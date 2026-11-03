// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

// TestEveryMethodDeclaresEffect enforces the schema rule the CLI and MCP
// generators rely on: every Dockwell RPC says how it touches managed state,
// and only reads are marked free of side effects.
func TestEveryMethodDeclaresEffect(t *testing.T) {
	var methods int
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if !strings.HasPrefix(string(fd.Package()), "dockwell.") {
			return true
		}
		services := fd.Services()
		for i := range services.Len() {
			ms := services.Get(i).Methods()
			for j := range ms.Len() {
				m := ms.Get(j)
				methods++
				opts, _ := m.Options().(*descriptorpb.MethodOptions)
				effect := dockwellv1.Effect_EFFECT_UNSPECIFIED
				if opts != nil {
					effect, _ = proto.GetExtension(opts, dockwellv1.E_Effect).(dockwellv1.Effect)
				}
				if effect == dockwellv1.Effect_EFFECT_UNSPECIFIED {
					t.Errorf("%s doesn't set (dockwell.v1.effect)", m.FullName())
				}
				noSideEffects := opts.GetIdempotencyLevel() == descriptorpb.MethodOptions_NO_SIDE_EFFECTS
				if noSideEffects && effect != dockwellv1.Effect_EFFECT_READ && effect != dockwellv1.Effect_EFFECT_PLAN {
					t.Errorf("%s is marked NO_SIDE_EFFECTS but its effect is %v", m.FullName(), effect)
				}
			}
		}
		return true
	})
	if methods == 0 {
		t.Fatal("no dockwell.* methods are registered; is the generated package linked in?")
	}
}
