// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Names of the well-known types a flag takes as a value of its own.
const (
	timestampName = "google.protobuf.Timestamp"
	durationName  = "google.protobuf.Duration"
)

// binding ties a command's flags, and its argument, to the fields of a
// message: a request, or a kind's spec.
type binding struct {
	root   protoreflect.MessageDescriptor
	fields []*boundField
	arg    *boundField
}

// boundField is one flag or argument.
type boundField struct {
	path   []protoreflect.FieldDescriptor
	flag   string
	values []string
	// sensitive takes a map entry's value from the environment when only
	// its key is given.
	sensitive bool
	env       *Env
}

// FlagName is the flag for a field's path: "page_size" is "page-size",
// and "app.image" "app-image".
func FlagName(path string) string {
	return strings.NewReplacer("_", "-", ".", "-").Replace(path)
}

// bind adds a flag for each field of root in fields, and finds the one
// that is the argument. The flags appear in the order of the fields.
func bind(fs *pflag.FlagSet, env *Env, root protoreflect.MessageDescriptor, fields []Field) (*binding, error) {
	b := &binding{root: root}
	for _, f := range fields {
		path, err := resolvePath(root, f.Path)
		if err != nil {
			return nil, err
		}
		bf := &boundField{path: path, flag: FlagName(f.Path), sensitive: f.Sensitive, env: env}
		if f.Arg {
			b.arg = bf
			continue
		}
		fd := path[len(path)-1]
		flag := fs.VarPF(bf, bf.flag, "", flagHelp(fd, f))
		if fd.Kind() == protoreflect.BoolKind && !fd.IsList() {
			flag.NoOptDefVal = "true"
		}
		b.fields = append(b.fields, bf)
	}
	return b, nil
}

// resolvePath finds the fields along a dotted path from root.
func resolvePath(root protoreflect.MessageDescriptor, path string) ([]protoreflect.FieldDescriptor, error) {
	var out []protoreflect.FieldDescriptor
	md := root
	for i, name := range strings.Split(path, ".") {
		if md == nil {
			return nil, fmt.Errorf("%s has no field %s", root.FullName(), path)
		}
		fd := md.Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			return nil, fmt.Errorf("%s has no field %s; run make generate", root.FullName(), path)
		}
		out = append(out, fd)
		md = nil
		if i < len(strings.Split(path, "."))-1 {
			md = fd.Message()
		}
	}
	return out, nil
}

// flagHelp is a field's comment, and how to write its value.
func flagHelp(fd protoreflect.FieldDescriptor, f Field) string {
	help := strings.TrimSpace(f.Help)
	add := func(s string) {
		if help != "" && !strings.HasSuffix(help, ".") {
			help += "."
		}
		if help != "" {
			help += " "
		}
		help += s
	}
	value := fd
	if fd.IsMap() {
		value = fd.MapValue()
	}
	switch {
	case value.Kind() == protoreflect.EnumKind:
		add("One of " + strings.Join(enumNames(value.Enum()), ", ") + ".")
	case value.Message() != nil && value.Message().FullName() == timestampName:
		add("A time such as 2026-12-01T09:00:00Z.")
	case value.Message() != nil && value.Message().FullName() == durationName:
		add("A duration such as 90d, 12h or 30m.")
	case value.Message() != nil:
		add("As JSON, or @file to read it from a file, or - from standard input.")
	}
	switch {
	case fd.IsMap() && f.Sensitive:
		add("As KEY=VALUE, or KEY alone to take the value from your environment, which keeps it out of your shell's history. Repeat it for each.")
	case fd.IsMap():
		add("As KEY=VALUE. Repeat it for each.")
	case fd.IsList():
		add("Repeat it for each.")
	}
	return help
}

// enumNames are an enum's values as flags take them: without the enum's
// prefix, in lowercase, with dashes, and without the unspecified value.
func enumNames(ed protoreflect.EnumDescriptor) []string {
	var out []string
	values := ed.Values()
	for i := range values.Len() {
		if v := values.Get(i); v.Number() != 0 {
			out = append(out, enumName(ed, v))
		}
	}
	return out
}

func enumName(ed protoreflect.EnumDescriptor, v protoreflect.EnumValueDescriptor) string {
	prefix := strings.ToUpper(snake(string(ed.Name()))) + "_"
	return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(string(v.Name()), prefix), "_", "-"))
}

// snake writes a PascalCase name in snake_case: "TokenScope" is
// "token_scope".
func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// String is the flag's default, which help shows: none.
func (*boundField) String() string { return "" }

// Set takes one value, and checks it at once, so a mistake names its flag.
func (b *boundField) Set(s string) error {
	fd := b.path[len(b.path)-1]
	if _, err := b.check(fd, s); err != nil {
		return err
	}
	b.values = append(b.values, s)
	return nil
}

// Type names the kind of value, as help shows it.
func (b *boundField) Type() string {
	fd := b.path[len(b.path)-1]
	if fd.IsMap() {
		return "key=value"
	}
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return "bool"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "int"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "uint"
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return "number"
	case protoreflect.EnumKind:
		return "string"
	case protoreflect.MessageKind:
		switch fd.Message().FullName() {
		case timestampName:
			return "time"
		case durationName:
			return "duration"
		}
		return "json"
	default:
		return "string"
	}
}

// check parses one value for fd, or for a map's entry.
func (b *boundField) check(fd protoreflect.FieldDescriptor, s string) (protoreflect.Value, error) {
	if fd.IsMap() {
		key, value, ok := strings.Cut(s, "=")
		if key == "" {
			return protoreflect.Value{}, errors.New("write it as KEY=VALUE")
		}
		if !ok {
			if !b.sensitive {
				return protoreflect.Value{}, errors.New("write it as KEY=VALUE")
			}
			return protoreflect.Value{}, nil // read from the environment when applied
		}
		return scalar(fd.MapValue(), value, b.env)
	}
	return scalar(fd, s, b.env)
}

// apply sets the field in msg from the values given, if any.
func (b *boundField) apply(msg protoreflect.Message) error {
	if len(b.values) == 0 {
		return nil
	}
	parent := msg
	for _, fd := range b.path[:len(b.path)-1] {
		parent = parent.Mutable(fd).Message()
	}
	fd := b.path[len(b.path)-1]
	switch {
	case fd.IsMap():
		m := parent.Mutable(fd).Map()
		for _, s := range b.values {
			key, value, ok := strings.Cut(s, "=")
			if !ok {
				value = b.env.Getenv(key)
				if value == "" {
					return fmt.Errorf("--%s %s takes the value of %s from your environment, but it isn't set. Set it, or write --%s %s=VALUE",
						b.flag, key, key, b.flag, key)
				}
			}
			v, err := scalar(fd.MapValue(), value, b.env)
			if err != nil {
				return fmt.Errorf("--%s %s: %w", b.flag, key, err)
			}
			m.Set(protoreflect.ValueOfString(key).MapKey(), v)
		}
	case fd.IsList():
		l := parent.Mutable(fd).List()
		for _, s := range b.values {
			v, err := scalarOrMessage(fd, s, b.env, l.NewElement)
			if err != nil {
				return fmt.Errorf("--%s: %w", b.flag, err)
			}
			l.Append(v)
		}
	default:
		v, err := scalarOrMessage(fd, b.values[len(b.values)-1], b.env, func() protoreflect.Value { return parent.NewField(fd) })
		if err != nil {
			return fmt.Errorf("--%s: %w", b.flag, err)
		}
		parent.Set(fd, v)
	}
	return nil
}

// scalarOrMessage parses a value, reading a message field's JSON into a
// new message from blank.
func scalarOrMessage(fd protoreflect.FieldDescriptor, s string, env *Env, blank func() protoreflect.Value) (protoreflect.Value, error) {
	if fd.Kind() != protoreflect.MessageKind {
		return scalar(fd, s, env)
	}
	switch fd.Message().FullName() {
	case timestampName, durationName:
		return scalar(fd, s, env)
	}
	data, err := readValue(s, env)
	if err != nil {
		return protoreflect.Value{}, err
	}
	v := blank()
	if err := protojson.Unmarshal(data, v.Message().Interface()); err != nil {
		return protoreflect.Value{}, fmt.Errorf("can't read it as a %s in JSON: %w", fd.Message().Name(), err)
	}
	return v, nil
}

// readValue is a JSON flag's value: as given, from a file with @path, or
// from standard input with -.
func readValue(s string, env *Env) ([]byte, error) {
	switch {
	case s == "-":
		return io.ReadAll(env.In)
	case strings.HasPrefix(s, "@"):
		b, err := os.ReadFile(s[1:])
		if err != nil {
			return nil, fmt.Errorf("can't read %s: %w", s[1:], err)
		}
		return b, nil
	}
	return []byte(s), nil
}

// scalar parses a value of a field that isn't a message, or of a
// timestamp or duration.
func scalar(fd protoreflect.FieldDescriptor, s string, _ *Env) (protoreflect.Value, error) {
	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(s), nil
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes([]byte(s)), nil
	case protoreflect.BoolKind:
		v, err := strconv.ParseBool(s)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("%q isn't true or false", s)
		}
		return protoreflect.ValueOfBool(v), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		v, err := strconv.ParseInt(s, 10, 32)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("%q isn't a whole number", s)
		}
		return protoreflect.ValueOfInt32(int32(v)), nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("%q isn't a whole number", s)
		}
		return protoreflect.ValueOfInt64(v), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		v, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("%q isn't a whole number from 0", s)
		}
		return protoreflect.ValueOfUint32(uint32(v)), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("%q isn't a whole number from 0", s)
		}
		return protoreflect.ValueOfUint64(v), nil
	case protoreflect.FloatKind:
		v, err := strconv.ParseFloat(s, 32)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("%q isn't a number", s)
		}
		return protoreflect.ValueOfFloat32(float32(v)), nil
	case protoreflect.DoubleKind:
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return protoreflect.Value{}, fmt.Errorf("%q isn't a number", s)
		}
		return protoreflect.ValueOfFloat64(v), nil
	case protoreflect.EnumKind:
		ed := fd.Enum()
		values := ed.Values()
		for i := range values.Len() {
			v := values.Get(i)
			if v.Number() != 0 && (enumName(ed, v) == strings.ToLower(s) || string(v.Name()) == s) {
				return protoreflect.ValueOfEnum(v.Number()), nil
			}
		}
		return protoreflect.Value{}, fmt.Errorf("%q isn't one of %s", s, strings.Join(enumNames(ed), ", "))
	case protoreflect.MessageKind:
		switch fd.Message().FullName() {
		case timestampName:
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return protoreflect.Value{}, fmt.Errorf("%q isn't a time such as 2026-12-01T09:00:00Z", s)
			}
			return protoreflect.ValueOfMessage(timestamppb.New(t).ProtoReflect()), nil
		case durationName:
			d, err := ParseDuration(s)
			if err != nil {
				return protoreflect.Value{}, err
			}
			return protoreflect.ValueOfMessage(durationpb.New(d).ProtoReflect()), nil
		}
		return protoreflect.Value{}, nil // JSON, read when applied
	default:
		return protoreflect.Value{}, fmt.Errorf("the CLI can't set a field of kind %s", fd.Kind())
	}
}

// ParseDuration reads a duration as Go writes one, such as "12h" or
// "30m", or in whole days, such as "90d".
func ParseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err == nil && n >= 0 && n <= 36500 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q isn't a duration such as 90d, 12h or 30m", s)
	}
	return d, nil
}

// apply sets every field given in msg, and the argument from args. It
// refuses two fields of one oneof.
func (b *binding) apply(msg protoreflect.Message, args []string) error {
	if b.arg != nil && len(args) > 0 {
		b.arg.values = []string{args[0]}
	}
	all := b.fields
	if b.arg != nil {
		all = append([]*boundField{b.arg}, all...)
	}
	// Each oneof takes one of its fields: flags for two of them, or for
	// fields inside two of them, can't go together.
	type member struct {
		field protoreflect.FullName
		flag  string
	}
	oneofs := map[protoreflect.FullName]member{}
	for _, f := range all {
		if len(f.values) == 0 {
			continue
		}
		for _, fd := range f.path {
			o := fd.ContainingOneof()
			if o == nil || o.IsSynthetic() {
				continue
			}
			if other, ok := oneofs[o.FullName()]; ok && other.field != fd.FullName() {
				return usage(fmt.Errorf("--%s and --%s can't go together. Give one of them", other.flag, f.flag))
			}
			oneofs[o.FullName()] = member{fd.FullName(), f.flag}
		}
		if err := f.apply(msg); err != nil {
			return usage(err)
		}
	}
	return nil
}
