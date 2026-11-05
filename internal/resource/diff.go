// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

// Op is how a field changed.
type Op int

// Ops.
const (
	OpAdded Op = iota + 1
	OpRemoved
	OpModified
)

// Change is one field that differs between two specs.
type Change struct {
	// Path names the field as the panel shows it, such as "image",
	// "env[API_KEY]" or "ports[0].host_port".
	Path string
	Op   Op
	// From and To are the values, empty when unset. Both are empty for a
	// sensitive field: a plan says a secret changed, never what it is.
	From, To  string
	Sensitive bool
}

// Diff lists the fields that differ from one spec to another, in field
// order. Either may be nil, for a resource being created or deleted. An
// unset field and its zero value are the same.
func Diff(from, to proto.Message) []Change { return diff(from, to, false) }

// DiffRotating is Diff for a spec whose sensitive values are set anew, as
// setting a secret's value does: each sensitive value to sets counts as
// changed, even when from holds the same. So what applying does never
// depends on whether the value is the one already there, and neither a
// plan nor a step can confirm a guess at it (ADR 0022). Its values are
// never compared.
func DiffRotating(from, to proto.Message) []Change { return diff(from, to, true) }

func diff(from, to proto.Message, rotate bool) []Change {
	var a, b protoreflect.Message
	var md protoreflect.MessageDescriptor
	if from != nil {
		a = from.ProtoReflect()
		md = a.Descriptor()
	}
	if to != nil {
		b = to.ProtoReflect()
		md = b.Descriptor()
	}
	if md == nil {
		return nil
	}
	d := differ{rotate: rotate}
	d.message("", md, a, b, false)
	return d.out
}

// SetsSensitive reports whether a spec sets a field marked (sensitive), at
// any depth: a secret's value, or an entry of a container's environment.
func SetsSensitive(m proto.Message) bool {
	if m == nil {
		return false
	}
	return setsSensitive(m.ProtoReflect())
}

func setsSensitive(m protoreflect.Message) bool {
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		found = IsSensitive(fd) || anyMessage(fd, v, setsSensitive)
		return !found
	})
	return found
}

// SetsSensitiveOf reports whether a message holds a spec of t, at any depth,
// that sets a sensitive field: a secret's value in a change's edit, but not
// a container's environment beside it.
func SetsSensitiveOf(t Type, m proto.Message) bool {
	if m == nil {
		return false
	}
	return setsSensitiveOf(t.New().ProtoReflect().Descriptor().FullName(), m.ProtoReflect())
}

func setsSensitiveOf(name protoreflect.FullName, m protoreflect.Message) bool {
	if m.Descriptor().FullName() == name {
		return setsSensitive(m)
	}
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		found = anyMessage(fd, v, func(sub protoreflect.Message) bool { return setsSensitiveOf(name, sub) })
		return !found
	})
	return found
}

// anyMessage reports whether found holds for a message in a field's value,
// alone or in a list or map.
func anyMessage(fd protoreflect.FieldDescriptor, v protoreflect.Value, found func(protoreflect.Message) bool) bool {
	hit := false
	switch {
	case fd.IsMap():
		if fd.MapValue().Message() != nil {
			v.Map().Range(func(_ protoreflect.MapKey, e protoreflect.Value) bool {
				hit = found(e.Message())
				return !hit
			})
		}
	case fd.IsList():
		if fd.Message() != nil {
			for i := 0; i < v.List().Len() && !hit; i++ {
				hit = found(v.List().Get(i).Message())
			}
		}
	case fd.Message() != nil:
		hit = found(v.Message())
	}
	return hit
}

// MayHold reports whether a message may hold a spec of t, at any depth, as
// a change's edit may hold a secret.
func MayHold(md protoreflect.MessageDescriptor, t Type) bool {
	return mayHold(md, t.New().ProtoReflect().Descriptor().FullName(), map[protoreflect.FullName]bool{})
}

func mayHold(md protoreflect.MessageDescriptor, name protoreflect.FullName, seen map[protoreflect.FullName]bool) bool {
	if md.FullName() == name {
		return true
	}
	if seen[md.FullName()] {
		return false
	}
	seen[md.FullName()] = true
	fields := md.Fields()
	for i := range fields.Len() {
		value := fields.Get(i)
		if value.IsMap() {
			value = value.MapValue()
		}
		if value.Message() != nil && mayHold(value.Message(), name, seen) {
			return true
		}
	}
	return false
}

// Equal reports whether two specs mean the same: no field differs.
func Equal(a, b proto.Message) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return proto.Equal(a, b)
}

type differ struct {
	out []Change
	// rotate counts every sensitive value the new spec sets as changed,
	// without comparing it (DiffRotating).
	rotate bool
}

// add records a change for a value that may be absent on either side. A
// singular field is absent when it holds its zero value; a map entry when
// its key is missing, whatever it holds.
func (d *differ) add(path, from, to string, sensitive bool) {
	d.addPresent(path, from, to, from != "", to != "", sensitive)
}

func (d *differ) addPresent(path, from, to string, hasFrom, hasTo, sensitive bool) {
	rotated := sensitive && d.rotate && hasTo
	if !rotated && hasFrom == hasTo && from == to {
		return
	}
	c := Change{Path: path, From: from, To: to, Sensitive: sensitive}
	switch {
	case !hasFrom:
		c.Op = OpAdded
	case !hasTo:
		c.Op = OpRemoved
	default:
		c.Op = OpModified
	}
	if sensitive {
		c.From, c.To = "", ""
	}
	d.out = append(d.out, c)
}

func (d *differ) message(prefix string, md protoreflect.MessageDescriptor, a, b protoreflect.Message, sensitive bool) {
	fields := md.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		path := string(fd.Name())
		if prefix != "" {
			path = prefix + "." + path
		}
		s := sensitive || IsSensitive(fd)
		switch {
		case fd.IsMap():
			d.mapField(path, fd, get(a, fd), get(b, fd), s)
		case fd.IsList():
			d.list(path, fd, get(a, fd), get(b, fd), s)
		case fd.Message() != nil:
			am, bm := sub(a, fd), sub(b, fd)
			if am != nil || bm != nil {
				d.message(path, fd.Message(), am, bm, s)
			}
		default:
			d.add(path, scalar(fd, get(a, fd)), scalar(fd, get(b, fd)), s)
		}
	}
}

func (d *differ) list(path string, fd protoreflect.FieldDescriptor, a, b protoreflect.Value, sensitive bool) {
	la, lb := list(a), list(b)
	if fd.Message() == nil {
		// A list of values reads best whole: "22, 80, 443". When joining
		// hides the difference, as with ["a, b"] and ["a", "b"], it goes
		// item by item.
		ja, jb := joinList(fd, la), joinList(fd, lb)
		// A sensitive list being set anew goes whole, so how it is shown
		// doesn't depend on its values either.
		if sensitive && d.rotate || ja != jb || len(la) == len(lb) && slices.EqualFunc(la, lb, func(x, y protoreflect.Value) bool { return x.Equal(y) }) {
			d.addPresent(path, ja, jb, len(la) > 0, len(lb) > 0, sensitive)
			return
		}
		for i := range max(len(la), len(lb)) {
			var x, y string
			if i < len(la) {
				x = present(fd, la[i])
			}
			if i < len(lb) {
				y = present(fd, lb[i])
			}
			d.addPresent(fmt.Sprintf("%s[%d]", path, i), x, y, i < len(la), i < len(lb), sensitive)
		}
		return
	}
	for i := range max(len(la), len(lb)) {
		var ma, mb protoreflect.Message
		if i < len(la) {
			ma = la[i].Message()
		}
		if i < len(lb) {
			mb = lb[i].Message()
		}
		d.message(fmt.Sprintf("%s[%d]", path, i), fd.Message(), ma, mb, sensitive)
	}
}

func (d *differ) mapField(path string, fd protoreflect.FieldDescriptor, a, b protoreflect.Value, sensitive bool) {
	ma, mb := mapEntries(a), mapEntries(b)
	keys := make([]protoreflect.MapKey, 0, len(ma)+len(mb))
	seen := make(map[string]bool, len(ma)+len(mb))
	for _, m := range []map[string]mapEntry{ma, mb} {
		for k, e := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, e.key)
			}
		}
	}
	slices.SortFunc(keys, func(x, y protoreflect.MapKey) int { return cmp.Compare(x.String(), y.String()) })
	vd := fd.MapValue()
	for _, k := range keys {
		p := fmt.Sprintf("%s[%s]", path, k.String())
		ea, oka := ma[k.String()]
		eb, okb := mb[k.String()]
		if vd.Message() != nil {
			var x, y protoreflect.Message
			if oka {
				x = ea.val.Message()
			}
			if okb {
				y = eb.val.Message()
			}
			d.message(p, vd.Message(), x, y, sensitive)
			continue
		}
		var from, to string
		if oka {
			from = present(vd, ea.val)
		}
		if okb {
			to = present(vd, eb.val)
		}
		d.addPresent(p, from, to, oka, okb, sensitive)
	}
}

// SensitivePath reports whether the field a change's path names, such as
// "env[API_KEY]" or "app.env[API_KEY]", is sensitive or lies within a
// sensitive field of md, as Diff marks it. A path that names a whole
// message, such as "app", or a whole map or list of messages, is sensitive
// when anything within it is: a change there would show the container's
// environment with it. It reads paths as Diff writes them: field names
// joined by dots, a list's index in brackets, and a map's key in brackets,
// which may hold dots and brackets of its own, as the label
// traefik.http.routers.web.rule does. A path md doesn't have is sensitive,
// as nothing can show it's safe. The control plane masks what agents
// report by it, rather than by what they say.
func SensitivePath(md protoreflect.MessageDescriptor, path string) bool {
	rest := path
	for {
		name := rest
		if i := strings.IndexAny(rest, ".["); i >= 0 {
			name = rest[:i]
		}
		rest = rest[len(name):]
		if md == nil || name == "" {
			return true
		}
		fd := md.Fields().ByName(protoreflect.Name(name))
		if fd == nil || IsSensitive(fd) {
			return true
		}
		if rest == "" {
			return holdsSensitive(fd) // the field as a whole
		}
		var ok bool
		switch {
		case fd.IsMap():
			if fd.MapValue().Message() == nil {
				// A scalar's key runs to the end of the path, whatever it
				// holds.
				return rest[0] != '[' || !strings.HasSuffix(rest, "]")
			}
			md = fd.MapValue().Message()
			rest, ok = mapEntryPath(rest)
		case fd.IsList():
			if fd.Message() == nil {
				after, ok := listElementPath(rest)
				return !ok || after != "" // a scalar element has no fields
			}
			md = fd.Message()
			rest, ok = listElementPath(rest)
		default:
			md = fd.Message()
			rest, ok = strings.CutPrefix(rest, ".")
			ok = ok && rest != ""
		}
		if !ok {
			return true
		}
		if rest == "" {
			return sensitiveMessage(md) // a list's element, or a map's entry, as a whole
		}
	}
}

// holdsSensitive reports a field whose values are messages, or a map's
// values are, with a sensitive field within them.
func holdsSensitive(fd protoreflect.FieldDescriptor) bool {
	value := fd
	if fd.IsMap() {
		value = fd.MapValue()
	}
	return value.Message() != nil && sensitiveMessage(value.Message())
}

// listElementPath reads "[3]" or "[3].field" off a path: what follows the
// index, past its dot.
func listElementPath(rest string) (string, bool) {
	inner, after, ok := strings.Cut(strings.TrimPrefix(rest, "["), "]")
	if !ok || !strings.HasPrefix(rest, "[") || inner == "" || strings.Trim(inner, "0123456789") != "" {
		return "", false
	}
	if after == "" {
		return "", true
	}
	field, ok := strings.CutPrefix(after, ".")
	return field, ok && field != ""
}

// mapEntryPath reads "[key]" or "[key].field" off a path, for a map whose
// values are messages: what follows the key, past its dot. A key holding
// "]." is read as ending there; what follows then doesn't name a field,
// and the path counts as sensitive.
func mapEntryPath(rest string) (string, bool) {
	if !strings.HasPrefix(rest, "[") {
		return "", false
	}
	if _, after, ok := strings.Cut(rest, "]."); ok {
		return after, after != ""
	}
	return "", strings.HasSuffix(rest, "]")
}

// Sealed reports whether a kind's specs may hold a secret: whether its
// message has a field marked (sensitive), at any depth. The revision store
// keeps every spec of such a kind encrypted, and digests it with a key for
// fingerprints (ADR 0022).
func Sealed(t Type) bool { return sensitiveMessage(t.New().ProtoReflect().Descriptor()) }

func sensitiveMessage(md protoreflect.MessageDescriptor) bool {
	sensitiveMu.Lock()
	defer sensitiveMu.Unlock()
	v, ok := sensitiveCache[md.FullName()]
	if !ok {
		v = sensitiveIn(md, map[protoreflect.FullName]bool{})
		sensitiveCache[md.FullName()] = v
	}
	return v
}

var (
	sensitiveMu    sync.Mutex
	sensitiveCache = map[protoreflect.FullName]bool{}
)

func sensitiveIn(md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) bool {
	if seen[md.FullName()] {
		return false
	}
	seen[md.FullName()] = true
	fields := md.Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		value := fd
		if fd.IsMap() {
			value = fd.MapValue()
		}
		if IsSensitive(fd) || value.Message() != nil && sensitiveIn(value.Message(), seen) {
			return true
		}
	}
	return false
}

// IsSensitive reports a field that may hold a secret, by its sensitive
// option: plans, history, logs and exports say it is there, never what it
// holds.
func IsSensitive(fd protoreflect.FieldDescriptor) bool {
	opts := fd.Options()
	return opts != nil && proto.GetExtension(opts, resourcev1.E_Sensitive).(bool)
}

func get(m protoreflect.Message, fd protoreflect.FieldDescriptor) protoreflect.Value {
	if m == nil || !m.Has(fd) {
		return protoreflect.Value{}
	}
	return m.Get(fd)
}

func sub(m protoreflect.Message, fd protoreflect.FieldDescriptor) protoreflect.Message {
	if m == nil || !m.Has(fd) {
		return nil
	}
	return m.Get(fd).Message()
}

func list(v protoreflect.Value) []protoreflect.Value {
	if !v.IsValid() {
		return nil
	}
	l := v.List()
	out := make([]protoreflect.Value, l.Len())
	for i := range l.Len() {
		out[i] = l.Get(i)
	}
	return out
}

type mapEntry struct {
	key protoreflect.MapKey
	val protoreflect.Value
}

func mapEntries(v protoreflect.Value) map[string]mapEntry {
	out := map[string]mapEntry{}
	if !v.IsValid() {
		return out
	}
	v.Map().Range(func(k protoreflect.MapKey, val protoreflect.Value) bool {
		out[k.String()] = mapEntry{key: k, val: val}
		return true
	})
	return out
}

func joinList(fd protoreflect.FieldDescriptor, vs []protoreflect.Value) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = present(fd, v)
	}
	return strings.Join(parts, ", ")
}

// scalar formats a singular field, with its zero value as unset.
func scalar(fd protoreflect.FieldDescriptor, v protoreflect.Value) string {
	if !v.IsValid() {
		return ""
	}
	return present(fd, v)
}

// present formats a value that is there, so a map entry or list item that
// holds a zero value still shows it.
func present(fd protoreflect.FieldDescriptor, v protoreflect.Value) string {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return v.String()
	case protoreflect.BoolKind:
		return strconv.FormatBool(v.Bool())
	case protoreflect.EnumKind:
		return enumName(fd.Enum(), v.Enum())
	case protoreflect.BytesKind:
		return fmt.Sprintf("%d bytes", len(v.Bytes()))
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64)
	case protoreflect.Uint32Kind, protoreflect.Uint64Kind, protoreflect.Fixed32Kind, protoreflect.Fixed64Kind:
		return strconv.FormatUint(v.Uint(), 10)
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return string(fd.Message().Name())
	default:
		return strconv.FormatInt(v.Int(), 10)
	}
}

// enumName turns RESTART_POLICY_UNLESS_STOPPED into "unless-stopped".
func enumName(ed protoreflect.EnumDescriptor, n protoreflect.EnumNumber) string {
	ev := ed.Values().ByNumber(n)
	if ev == nil {
		return strconv.Itoa(int(n))
	}
	name := string(ev.Name())
	values := ed.Values()
	if values.Len() > 0 {
		// Strip the prefix all the enum's values share, such as RESTART_POLICY_.
		first := string(values.Get(0).Name())
		if i := strings.LastIndex(first, "_UNSPECIFIED"); i >= 0 {
			name = strings.TrimPrefix(name, first[:i+1])
		}
	}
	return strings.ReplaceAll(strings.ToLower(name), "_", "-")
}
