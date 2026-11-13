// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"go.yaml.in/yaml/v4"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/resource/volume"
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
	// fromFile reads a sensitive field's value from a file, or standard
	// input, which its flag names: never from the command line.
	fromFile bool
	// orFile takes a text field as given, or from a file with @path, or
	// standard input with - (Field.FromFile).
	orFile bool
	env    *Env
}

// MaxSecretInput bounds a value read from a file or standard input: what
// a secret holds, as the control plane checks it.
const MaxSecretInput = variable.MaxValue

// FlagName is the flag for a field's path: "page_size" is "page-size",
// and "app.image" "app-image".
func FlagName(path string) string {
	return strings.NewReplacer("_", "-", ".", "-").Replace(path)
}

// ReservedFlags are the CLI's own flags, which no field's flag may take
// on any API command: --url, --token and --json, which every one has;
// --yes and --plan-only, which mean the same wherever they are; and
// --help. A field whose name would take one names its flag with
// (dockwell.v1.flag), as a claim link's token is --claim-token.
var ReservedFlags = []string{"url", "token", "json", "yes", "plan-only", "help"}

// FieldAnnotation marks a flag that sets a field, with the field's path,
// so tests can tell those flags from the CLI's own.
const FieldAnnotation = "dockwell-field"

// bind adds a flag for each field of root in fields, and finds the one
// that is the argument. The flags appear in the order of the fields. It
// fails when a field's flag would take one of the ReservedFlags, or one
// the command has already, which it would hide.
func bind(fs *pflag.FlagSet, env *Env, root protoreflect.MessageDescriptor, fields []Field) (*binding, error) {
	b := &binding{root: root}
	for _, f := range fields {
		path, err := resolvePath(root, f.Path)
		if err != nil {
			return nil, err
		}
		name := f.Flag
		if name == "" {
			name = FlagName(f.Path)
		}
		leaf := path[len(path)-1]
		// A secret's value never comes from the command line, where shell
		// history and the process list keep it: its flag names a file.
		fromFile := f.Sensitive && !leaf.IsMap() && !leaf.IsList() &&
			(leaf.Kind() == protoreflect.StringKind || leaf.Kind() == protoreflect.BytesKind)
		if fromFile {
			name += "-file"
		}
		orFile := f.FromFile && !fromFile && !leaf.IsList() && !leaf.IsMap() && leaf.Kind() == protoreflect.StringKind
		bf := &boundField{path: path, flag: name, sensitive: f.Sensitive, fromFile: fromFile, orFile: orFile, env: env}
		if f.Arg {
			b.arg = bf
			continue
		}
		if slices.Contains(ReservedFlags, name) || fs.Lookup(name) != nil {
			return nil, fmt.Errorf("%s's field %s would take the flag --%s, which the CLI has already. "+
				"Give it another with the (dockwell.v1.flag) option, then run make generate", root.FullName(), f.Path, name)
		}
		fd := path[len(path)-1]
		flag := fs.VarPF(bf, bf.flag, "", flagHelp(fd, f))
		flag.Annotations = map[string][]string{FieldAnnotation: {f.Path}}
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
			return nil, fmt.Errorf("%s has no field %s; run make generate", root.FullName(), path)
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
		if resource.MayHold(value.Message(), variable.SecretType{}) {
			add("JSON that sets a secret's value, or gives a volume a password, comes from a file or standard input, never the command line.")
		}
	case f.FromFile:
		add("As given, or @file to read it from a file, or - from standard input. A Compose file that gives a volume a password comes from a file or standard input, never the command line.")
	}
	switch {
	case f.Sensitive && !fd.IsMap() && !fd.IsList() && (fd.Kind() == protoreflect.StringKind || fd.Kind() == protoreflect.BytesKind):
		add("Name a file to read it from, or - for standard input, which asks for it without showing it on a terminal. " +
			"It never comes from the command line, so it stays out of your shell's history and the process list. A newline at its end is dropped.")
	case fd.IsMap() && f.Sensitive && isVolumeOptions(fd):
		add("As KEY=VALUE, or KEY alone to take the value from your environment, which keeps it out of your shell's history. A value that gives a password, as a CIFS mount's o does, comes from your environment. Repeat it for each.")
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
	if b.fromFile {
		return "file"
	}
	if b.orFile {
		return "text"
	}
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
	if b.fromFile {
		if s == "" {
			return protoreflect.Value{}, errors.New("the value comes from a file, so it stays off the command line. Name one, or - for standard input")
		}
		return protoreflect.Value{}, nil // read when applied
	}
	if fd.IsMap() {
		key, value, ok := strings.Cut(s, "=")
		if key == "" || !ok && !b.sensitive {
			return protoreflect.Value{}, fmt.Errorf("%q isn't KEY=VALUE", s)
		}
		if !ok {
			return protoreflect.Value{}, nil // read from the environment when applied
		}
		return scalar(fd.MapValue(), value)
	}
	if b.orFile {
		return protoreflect.Value{}, nil // read when applied
	}
	return scalar(fd, s)
}

// inline lists the keys given a value on the command line, in a map that may hold
// secrets, rather than taken from the environment.
func (b *binding) inline() []string {
	var keys []string
	for _, f := range b.fields {
		if !f.sensitive {
			continue
		}
		for _, s := range f.values {
			if key, _, ok := strings.Cut(s, "="); ok {
				keys = append(keys, key)
			}
		}
	}
	return keys
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
			// Here, not as the flag is parsed, which would repeat the value.
			if ok && isVolumeOptions(fd) && volume.HoldsPassword(map[string]string{key: value}) {
				return fmt.Errorf("--%s %s gives a password, so it would stay in your shell's history and the process list. "+
					"Give --%s %s alone to take its value from %s in your environment, or put the volume's edit in a file and pass it to dockwell changes propose with --edits @file",
					b.flag, key, b.flag, key, key)
			}
			if !ok {
				value = b.env.Getenv(key)
				if value == "" {
					return fmt.Errorf("--%s %s takes the value of %s from your environment, but it isn't set. Set it, or write --%s %s=VALUE",
						b.flag, key, key, b.flag, key)
				}
			}
			v, err := scalar(fd.MapValue(), value)
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
				return b.valueError(err)
			}
			l.Append(v)
		}
	case b.orFile:
		from := b.values[len(b.values)-1]
		data, err := readValue(from, b.env)
		if err != nil {
			return fmt.Errorf("--%s: %w", b.flag, err)
		}
		if inline(from) {
			if name := composePassword(data); name != "" {
				return fmt.Errorf("--%s: the file gives the volume %s a password in its driver_opts, so it would stay in your shell's history and the process list. "+
					"Pass the file with --%s @compose.yaml, or pipe it in with --%s -", b.flag, name, b.flag, b.flag)
			}
		}
		parent.Set(fd, protoreflect.ValueOfString(string(data)))
	case b.fromFile:
		text, err := b.readSecret(b.values[len(b.values)-1])
		if err != nil {
			return fmt.Errorf("--%s: %w", b.flag, err)
		}
		if fd.Kind() == protoreflect.BytesKind {
			parent.Set(fd, protoreflect.ValueOfBytes([]byte(text)))
		} else {
			parent.Set(fd, protoreflect.ValueOfString(text))
		}
	default:
		v, err := scalarOrMessage(fd, b.values[len(b.values)-1], b.env, func() protoreflect.Value { return parent.NewField(fd) })
		if err != nil {
			return b.valueError(err)
		}
		parent.Set(fd, v)
	}
	return nil
}

// valueError says which flag's value is wrong, and how to give it.
func (b *boundField) valueError(err error) error {
	if errors.Is(err, errInlineSecret) || errors.Is(err, errInlinePassword) || errors.Is(err, errInlineSealed) {
		return fmt.Errorf("--%s: %w. Put the JSON in a file and pass --%s @file, or pipe it in with --%s -", b.flag, err, b.flag, b.flag)
	}
	return fmt.Errorf("--%s: %w", b.flag, err)
}

// readSecret reads a sensitive value from a file, or from standard input
// for -: on a terminal, through Env.ReadSecret, which doesn't echo it. One
// newline at its end is dropped, as echo and most editors add one.
func (b *boundField) readSecret(from string) (string, error) {
	var data []byte
	switch {
	case from == "-" && b.env.ReadSecret != nil && b.env.Interactive != nil && b.env.Interactive():
		text, err := b.env.ReadSecret("Value (it isn't shown as you type): ")
		if err != nil {
			return "", fmt.Errorf("can't read the value: %w", err)
		}
		return text, nil
	case from == "-":
		var err error
		if data, err = io.ReadAll(io.LimitReader(b.env.In, maxSecretRead)); err != nil {
			return "", fmt.Errorf("can't read standard input: %w", err)
		}
	default:
		f, err := os.Open(from) //nolint:gosec // the person running the command names the file
		if err != nil {
			return "", fmt.Errorf("can't read %s: %w", from, err)
		}
		defer f.Close()
		if data, err = io.ReadAll(io.LimitReader(f, maxSecretRead)); err != nil {
			return "", fmt.Errorf("can't read %s: %w", from, err)
		}
	}
	text := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if len(text) > MaxSecretInput {
		return "", fmt.Errorf("the value is over %d KiB, so no secret can hold it. Check that you named the right file", MaxSecretInput>>10)
	}
	return text, nil
}

// maxSecretRead is how much readSecret reads: the most a value can be, the
// newline it may end with, "\r\n" at most, and a byte to tell a longer one.
const maxSecretRead = MaxSecretInput + 2 + 1

// scalarOrMessage parses a value, reading a message field's JSON into a
// new message from blank. JSON given on the command line itself, rather
// than from a file or standard input, may not set a secret's value: it
// would stay in shell history and the process list. Any other field may,
// as --env KEY=VALUE may set a container's environment.
func scalarOrMessage(fd protoreflect.FieldDescriptor, s string, env *Env, blank func() protoreflect.Value) (protoreflect.Value, error) {
	if fd.Kind() != protoreflect.MessageKind {
		return scalar(fd, s)
	}
	switch fd.Message().FullName() {
	case timestampName, durationName:
		return scalar(fd, s)
	}
	data, err := readValue(s, env)
	if err != nil {
		return protoreflect.Value{}, err
	}
	v := blank()
	if err := protojson.Unmarshal(data, v.Message().Interface()); err != nil {
		return protoreflect.Value{}, fmt.Errorf("can't read it as a %s in JSON: %w", fd.Message().Name(), err)
	}
	if inline(s) && resource.SetsSensitiveOf(variable.SecretType{}, v.Message().Interface()) {
		return protoreflect.Value{}, errInlineSecret
	}
	if inline(s) && givesVolumePassword(v.Message()) {
		return protoreflect.Value{}, errInlinePassword
	}
	if inline(s) && resource.SetsSensitiveIn(v.Message().Interface(), sealedMessages...) {
		return protoreflect.Value{}, errInlineSealed
	}
	return v, nil
}

// sealedMessages are the messages whose sensitive fields sign in to something:
// an alert channel's token, a registry login's password and its ECR key.
var sealedMessages = []protoreflect.FullName{
	(&resourcev1.AlertChannel{}).ProtoReflect().Descriptor().FullName(),
	(&resourcev1.RegistryLogin{}).ProtoReflect().Descriptor().FullName(),
	(&resourcev1.RegistryLoginEcr{}).ProtoReflect().Descriptor().FullName(),
	(&resourcev1.DnsProvider{}).ProtoReflect().Descriptor().FullName(),
}

// errInlineSecret refuses JSON on the command line that sets a secret,
// errInlinePassword JSON that gives a volume a password, and errInlineSealed
// JSON that gives an alert channel what signs in to it.
var (
	errInlineSecret   = errors.New("the JSON sets a secret's value, so it would stay in your shell history and the process list")
	errInlinePassword = errors.New("the JSON gives a volume a password in its options, so it would stay in your shell history and the process list")
	errInlineSealed   = errors.New("the JSON holds what signs in, such as an alert channel's token or a registry login's key, so it would stay in your shell history and the process list")
)

// volumeOptions is the field that holds a volume's options.
var volumeOptions = (&resourcev1.Volume{}).ProtoReflect().Descriptor().Fields().ByName("options")

// isVolumeOptions reports a volume's options, which a CIFS mount's password goes in.
func isVolumeOptions(fd protoreflect.FieldDescriptor) bool {
	return fd.FullName() == volumeOptions.FullName()
}

// givesVolumePassword reports a message that holds a volume, at any depth, whose
// options give a password.
func givesVolumePassword(m protoreflect.Message) bool {
	if v, ok := m.Interface().(*resourcev1.Volume); ok {
		return volume.HoldsPassword(v.GetOptions())
	}
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Message() != nil:
			for i := range v.List().Len() {
				if found = givesVolumePassword(v.List().Get(i).Message()); found {
					break
				}
			}
		case fd.IsMap() && fd.MapValue().Message() != nil:
			v.Map().Range(func(_ protoreflect.MapKey, e protoreflect.Value) bool {
				found = givesVolumePassword(e.Message())
				return !found
			})
		case !fd.IsList() && !fd.IsMap() && fd.Message() != nil:
			found = givesVolumePassword(v.Message())
		}
		return !found
	})
	return found
}

// composePassword names a volume a Compose file gives a password in its
// driver_opts, or nothing. A file that doesn't read is the control plane's to
// refuse, and holds none here.
func composePassword(file []byte) string {
	var doc struct {
		Volumes map[string]struct {
			DriverOpts map[string]any `yaml:"driver_opts"`
		} `yaml:"volumes"`
	}
	if yaml.Unmarshal(file, &doc) != nil {
		return ""
	}
	for _, name := range slices.Sorted(maps.Keys(doc.Volumes)) {
		options := map[string]string{}
		for k, v := range doc.Volumes[name].DriverOpts {
			options[k] = fmt.Sprint(v)
		}
		if volume.HoldsPassword(options) {
			return name
		}
	}
	return ""
}

// inline reports a JSON flag's value given on the command line itself.
func inline(s string) bool { return s != "-" && !strings.HasPrefix(s, "@") }

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
func scalar(fd protoreflect.FieldDescriptor, s string) (protoreflect.Value, error) {
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
		return protoreflect.Value{}, fmt.Errorf("the CLI can't set a field of kind %s, so it has no flag for it. Set it through the API instead", fd.Kind())
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
