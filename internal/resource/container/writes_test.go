// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// Every field a container took on from devices on, and a health check from
// start_interval on, is one an agent before Hello.runs_host_access drops without a
// word, so the one gate the control plane asks names each: a field added here and
// not there fails this (the review of ADR 0097, L8).
func TestTheHostAccessGateNamesEveryNewField(t *testing.T) {
	check := func(m protoreflect.Message, from protoreflect.FieldNumber, wrap func(protoreflect.Message) *resourcev1.Container) {
		fields := m.Descriptor().Fields()
		for i := range fields.Len() {
			fd := fields.Get(i)
			if fd.Number() < from {
				continue
			}
			msg := m.New()
			setSome(msg, fd)
			if !WritesHostAccess(wrap(msg)) {
				t.Errorf("%s isn't one WritesHostAccess names", fd.FullName())
			}
		}
	}
	check((&resourcev1.Container{}).ProtoReflect(), 101, func(m protoreflect.Message) *resourcev1.Container {
		return m.Interface().(*resourcev1.Container)
	})
	check((&resourcev1.Healthcheck{}).ProtoReflect(), 8, func(m protoreflect.Message) *resourcev1.Container {
		return &resourcev1.Container{Healthcheck: m.Interface().(*resourcev1.Healthcheck)}
	})
}

// setSome gives fd a value that isn't its zero.
func setSome(m protoreflect.Message, fd protoreflect.FieldDescriptor) {
	one := func() protoreflect.Value {
		switch fd.Kind() {
		case protoreflect.StringKind:
			return protoreflect.ValueOfString("a=x")
		case protoreflect.BoolKind:
			return protoreflect.ValueOfBool(true)
		case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
			return protoreflect.ValueOfInt32(1)
		case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
			return protoreflect.ValueOfInt64(1)
		case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
			return protoreflect.ValueOfUint32(1)
		case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
			return protoreflect.ValueOfUint64(1)
		case protoreflect.EnumKind:
			return protoreflect.ValueOfEnum(1)
		case protoreflect.FloatKind:
			return protoreflect.ValueOfFloat32(1)
		case protoreflect.DoubleKind:
			return protoreflect.ValueOfFloat64(1)
		case protoreflect.MessageKind, protoreflect.GroupKind:
			return protoreflect.ValueOfMessage(m.NewField(fd).Message().New())
		case protoreflect.BytesKind:
		}
		return protoreflect.ValueOfBytes([]byte("x"))
	}
	switch {
	case fd.IsMap():
		m.Mutable(fd).Map().Set(protoreflect.ValueOfString("a").MapKey(), protoreflect.ValueOfString("x"))
	case fd.IsList():
		l := m.Mutable(fd).List()
		if fd.Kind() == protoreflect.MessageKind {
			l.Append(protoreflect.ValueOfMessage(l.NewElement().Message()))
		} else {
			l.Append(one())
		}
	default:
		m.Set(fd, one())
	}
}
