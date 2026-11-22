// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/distribution/reference"
	"go.yaml.in/yaml/v4"
	"google.golang.org/protobuf/proto"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/gitfetch"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
)

const (
	templateText   = spec.ComposeTemplateInputKind_COMPOSE_TEMPLATE_INPUT_KIND_TEXT
	templateSecret = spec.ComposeTemplateInputKind_COMPOSE_TEMPLATE_INPUT_KIND_SECRET
	templatePort   = spec.ComposeTemplateInputKind_COMPOSE_TEMPLATE_INPUT_KIND_PORT
	templateImage  = spec.ComposeTemplateInputKind_COMPOSE_TEMPLATE_INPUT_KIND_IMAGE
)

var (
	templateVariable   = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)(?:(:-|-)([^$}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)|\$\{`)
	templateName       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
	templateSecretWord = regexp.MustCompile(`(?i)pass(word|wd|phrase)?|secret|token|credential|api.?key|private.?key|auth|cookie|dsn`)
	templateSetting    = regexp.MustCompile(`(?:^|[[:space:]])(--[A-Za-z][A-Za-z0-9_-]*|[A-Za-z_][A-Za-z0-9_.-]*=)`)
)

// TemplateIssue omits source text. Even invalid YAML can contain credentials.
type TemplateIssue struct {
	Line   int
	Reason string
}

func (i *TemplateIssue) Error() string { return i.Reason }

func templateIssue(n *yaml.Node, message string) error {
	return &TemplateIssue{Line: n.Line, Reason: message}
}

type templateInput struct {
	spec            *spec.ComposeTemplateInput
	environmentOnly bool
	imageWhole      bool
}

// TemplateProjection makes a copy for preparing a template without opening the
// full service or volume revision. Environment slots keep only their keys.
func TemplateProjection(source proto.Message) proto.Message {
	switch source := source.(type) {
	case *spec.Service:
		out := proto.CloneOf(source)
		out.ShownEnv = nil
		if out.Build != nil {
			out.Build = &spec.ImageBuild{}
		}
		if app := out.GetApp(); app != nil {
			app.Env = nil
			for _, mount := range app.GetMounts() {
				if file := mount.GetFile(); file != nil {
					file.Content = ""
				}
			}
		}
		if resource.SetsSensitive(out) {
			return nil
		}
		if app := out.GetApp(); app != nil && len(source.GetApp().GetEnv()) > 0 {
			app.Env = make(map[string]string, len(source.GetApp().GetEnv()))
			for name := range source.GetApp().GetEnv() {
				app.Env[name] = ""
			}
		}
		return out
	case *spec.Volume:
		out := proto.CloneOf(source)
		out.Options = nil
		if resource.SetsSensitive(out) {
			return nil
		}
		if len(source.Options) > 0 {
			out.Options = map[string]string{"omitted": ""}
		}
		return out
	}
	return nil
}

// TemplateBindings describes runtime aliases of the same sealed reference, by service.
type TemplateBindings map[string]map[string]string

// ValidateTemplate canonicalizes one portable definition. A non-nil workspace
// binding map also turns the projected stack's environment slots into inputs.
func ValidateTemplate(ctx context.Context, draft *spec.ComposeTemplate, workspace TemplateBindings) error {
	if draft == nil || len(draft.GetComposeYaml()) > MaxFile || len(draft.GetInputs()) > 128 {
		return errors.New("use one Compose file up to 256 KiB with at most 128 inputs")
	}
	if g := draft.GetGit(); g != nil {
		u, err := gitfetch.CheckURL(g.GetRepository())
		if err != nil || !TemplateGitPath(g.GetPath()) || !gitfetch.Commit.MatchString(g.GetCommit()) ||
			len(g.GetRepository()) > 512 || len(g.GetRef()) > 256 || strings.ContainsFunc(g.GetRef(), unicode.IsControl) {
			return errors.New("name a public HTTPS repository, one Compose file and a full Git commit")
		}
		g.Repository = u.String()
	}
	description := strings.TrimSpace(draft.GetDescription())
	if len(description) > 160 || strings.ContainsFunc(description, unicode.IsControl) || service.SecretShape(description) {
		return errors.New("give the template a short public description")
	}
	draft.Description = description
	var doc yaml.Node
	dec := yaml.NewDecoder(strings.NewReader(draft.GetComposeYaml()))
	if err := dec.Decode(&doc); err != nil {
		line := 0
		if !errors.Is(err, io.EOF) {
			if match := yamlLine.FindStringSubmatch(err.Error()); len(match) > 1 {
				line, _ = strconv.Atoi(match[1])
			}
		}
		return &TemplateIssue{Line: line, Reason: "use one valid Compose YAML document"}
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return errors.New("use one Compose YAML document with a services map")
	}
	root := doc.Content[0]
	if err := scrubTemplate(root, 0); err != nil {
		return err
	}
	// The destination supplies the Compose project name.
	for i := 0; i < len(root.Content); i += 2 {
		if key := root.Content[i]; key.Value == "name" {
			root.Content = slices.Delete(root.Content, i, i+2)
			break
		}
	}
	fields := entry(root, "services")
	if fields == nil || fields.Kind != yaml.MappingNode {
		return errors.New("the template needs a services map")
	}
	workspaceKinds := map[string]bool{}
	shared := map[string]string{}
	envNodes := map[*yaml.Node]bool{}
	for i := 0; i < len(fields.Content); i += 2 {
		serviceName := fields.Content[i].Value
		svc := fields.Content[i+1]
		if svc.Kind != yaml.MappingNode {
			return templateIssue(svc, "give the service a Compose mapping")
		}
		env := entry(svc, "environment")
		if env == nil {
			continue
		}
		slots := map[string]bool{}
		clean, err := templateEnvironment(env, serviceName, workspace != nil, workspaceKinds, workspace[serviceName], shared, slots)
		if err != nil {
			return err
		}
		if workspace != nil {
			for j := 0; j < len(svc.Content); j += 2 {
				value := svc.Content[j+1]
				if value != env {
					if hit := templateSlotUse(value, slots); hit != nil {
						return templateIssue(hit, fmt.Sprintf("the %s field uses a saved environment value. Template secrets can only fill environment values", svc.Content[j].Value))
					}
				}
			}
		}
		*env = *clean
		envNodes[env] = true
	}
	overrides := map[string]*spec.ComposeTemplateInput{}
	for _, in := range draft.GetInputs() {
		if !templateName.MatchString(in.GetName()) || overrides[in.GetName()] != nil {
			return errors.New("give each template input a unique variable name")
		}
		overrides[in.GetName()] = in
	}
	inputs := map[string]*templateInput{}
	if err := collectTemplate(root, "", false, inputs, workspaceKinds, envNodes); err != nil {
		return err
	}
	if len(inputs) > 128 {
		return errors.New("a template can have at most 128 inputs")
	}
	draft.Inputs = nil
	samples := map[string]string{}
	for i, name := range slices.Sorted(maps.Keys(inputs)) {
		f := inputs[name]
		if override := overrides[name]; override != nil {
			if override.GetKind() == templateSecret {
				f.spec.Kind = templateSecret
			} else if override.GetKind() != f.spec.GetKind() {
				return errors.New("an input's type must match the way the Compose file uses it")
			}
			if override.DefaultValue != nil {
				f.spec.DefaultValue = override.DefaultValue
				f.spec.Required = false
			}
			delete(overrides, name)
		}
		if f.spec.Kind == templateSecret && (!f.environmentOnly || f.spec.DefaultValue != nil) {
			return errors.New("a secret input must fill a whole environment value and have no default")
		}
		value := fmt.Sprintf("dockwell-template-%d", i)
		switch {
		case f.spec.DefaultValue != nil:
			value = f.spec.GetDefaultValue()
		case f.spec.Kind == templatePort:
			value = strconv.Itoa(40000 + i)
		case f.spec.Kind == templateImage && f.imageWhole:
			value = "example.invalid/template:1"
		}
		if err := CheckTemplateValue(f.spec, value); err != nil {
			return err
		}
		samples[name] = value
		draft.Inputs = append(draft.Inputs, f.spec)
	}
	if len(overrides) != 0 {
		return errors.New("remove inputs that the Compose file does not use")
	}
	data, err := yaml.Marshal(root)
	if err != nil || len(data) > MaxFile {
		return errors.New("the normalized Compose file is too large")
	}
	if _, err := ParseWorkspace(ctx, data, samples); err != nil {
		line, _ := Lines(ctx, []byte(draft.GetComposeYaml()), err)
		return &TemplateIssue{Line: line, Reason: "Dockwell cannot use this Compose field in a template"}
	}
	draft.ComposeYaml = string(data)
	return nil
}

func scrubTemplate(n *yaml.Node, depth int) error {
	if depth > 48 || n.Kind == yaml.AliasNode || n.Anchor != "" {
		return templateIssue(n, "templates use plain YAML without anchors or aliases")
	}
	switch n.Tag {
	case "!!map", "!!seq", "!!str", "!!null", "!!int", "!!float", "!!bool":
	default:
		return templateIssue(n, "use standard Compose YAML values")
	}
	n.HeadComment, n.LineComment, n.FootComment = "", "", ""
	if n.Kind == yaml.MappingNode {
		keys := make(map[string]bool, len(n.Content)/2)
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || keys[key.Value] {
				return templateIssue(key, "give every Compose field one plain name")
			}
			keys[key.Value] = true
			switch key.Value {
			case "include", "build", "env_file", "extends", "driver_opts", "configs", "secrets":
				return templateIssue(key, "templates take one portable file without local files, build sources or driver options")
			}
			if strings.HasPrefix(key.Value, "x-") {
				return templateIssue(key, "remove this extension to save a portable template")
			}
		}
	}
	for _, c := range n.Content {
		if err := scrubTemplate(c, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func templateEnvironment(env *yaml.Node, svc string, fromWorkspace bool, kinds map[string]bool, refs, shared map[string]string, slots map[string]bool) (*yaml.Node, error) {
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Line: env.Line}
	add := func(key string, value *yaml.Node) error {
		if !templateName.MatchString(key) || value.Kind != yaml.ScalarNode {
			return templateIssue(value, "give the environment value a variable name")
		}
		if slots[key] {
			return templateIssue(value, "give each environment slot one value")
		}
		slots[key] = true
		v := value.Value
		if fromWorkspace {
			name := workspaceInputName(svc, key, refs[key], kinds, shared)
			v = "${" + name + "}"
		} else if value.Tag == "!!null" {
			v = "${" + key + "}"
		}
		match := templateVariable.FindStringSubmatch(v)
		if len(match) == 0 || match[0] != v || (match[1] == "" && match[4] == "") || match[2] != "" {
			return templateIssue(value, "replace this environment value with a variable such as ${VALUE}")
		}
		if templateSecretWord.MatchString(key) {
			kinds[match[1]+match[4]] = true
		}
		out.Content = append(out.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Line: value.Line})
		return nil
	}
	switch env.Kind {
	case yaml.MappingNode:
		for i := 0; i < len(env.Content); i += 2 {
			if err := add(env.Content[i].Value, env.Content[i+1]); err != nil {
				return nil, err
			}
		}
	case yaml.SequenceNode:
		for _, n := range env.Content {
			key, value, _ := strings.Cut(n.Value, "=")
			tag := "!!str"
			if !strings.Contains(n.Value, "=") {
				tag = "!!null"
			}
			if err := add(key, &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value, Line: n.Line}); err != nil {
				return nil, err
			}
		}
	default:
		return nil, templateIssue(env, "give environment inputs as a list or map")
	}
	return out, nil
}

func workspaceInputName(serviceName, slot, ref string, kinds map[string]bool, shared map[string]string) string {
	if name := shared[ref]; ref != "" && name != "" {
		return name
	}
	name := strings.ToUpper(strings.ReplaceAll(serviceName, "-", "_")) + "_" + slot
	if !templateName.MatchString(name) || kinds[name] {
		for i := len(kinds); ; i++ {
			name = fmt.Sprintf("ENV_%d", i)
			if !kinds[name] {
				break
			}
		}
	}
	kinds[name] = true
	if ref != "" {
		shared[ref] = name
	}
	return name
}

func templateSlotUse(n *yaml.Node, slots map[string]bool) *yaml.Node {
	if n.Kind == yaml.ScalarNode {
		for _, match := range templateVariable.FindAllStringSubmatch(n.Value, -1) {
			if slots[match[1]+match[4]] {
				return n
			}
		}
		return nil
	}
	start, step := 0, 1
	if n.Kind == yaml.MappingNode {
		start, step = 1, 2
	}
	for i := start; i < len(n.Content); i += step {
		if hit := templateSlotUse(n.Content[i], slots); hit != nil {
			return hit
		}
	}
	return nil
}

func collectTemplate(n *yaml.Node, field string, inEnv bool, inputs map[string]*templateInput, secret map[string]bool, envNodes map[*yaml.Node]bool) error {
	inEnv = inEnv || envNodes[n]
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i].Value
			next := key
			if field == "ports" || inEnv {
				next = field
			}
			if err := collectTemplate(n.Content[i+1], next, inEnv, inputs, secret, envNodes); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if err := collectTemplate(c, field, inEnv, inputs, secret, envNodes); err != nil {
				return err
			}
		}
	case yaml.DocumentNode, yaml.AliasNode, yaml.StreamNode:
		return templateIssue(n, "use a plain Compose field")
	case yaml.ScalarNode:
		if service.SecretShape(n.Value) || !inEnv && n.Value != "" && n.Tag != "!!null" &&
			(templateSecretWord.MatchString(field) || containsTemplateCredentialSetting(n.Value)) {
			return templateIssue(n, "move the credential into an environment input")
		}
		for _, match := range templateVariable.FindAllStringSubmatch(n.Value, -1) {
			if match[0] == "$$" {
				continue
			}
			name := match[1] + match[4]
			if !templateName.MatchString(name) {
				return templateIssue(n, "use a simple variable or a public default in this field")
			}
			kind := templateText
			switch {
			case secret[name] || templateSecretWord.MatchString(name):
				kind = templateSecret
			case field == "ports":
				kind = templatePort
			case field == "image":
				kind = templateImage
			}
			var fallback *string
			if match[2] != "" {
				fallback = &match[3]
			}
			if prev := inputs[name]; prev != nil {
				if prev.spec.Kind != kind || !sameTemplateDefault(prev.spec.DefaultValue, fallback) {
					return templateIssue(n, "use the same input type and default every time this variable appears")
				}
				prev.environmentOnly = prev.environmentOnly && inEnv
				prev.imageWhole = prev.imageWhole && match[0] == n.Value
				continue
			}
			inputs[name] = &templateInput{
				spec:            &spec.ComposeTemplateInput{Name: name, Kind: kind, DefaultValue: fallback, Required: fallback == nil},
				environmentOnly: inEnv && match[0] == n.Value, imageWhole: match[0] == n.Value,
			}
		}
	}
	return nil
}

func containsTemplateCredentialSetting(value string) bool {
	for _, match := range templateSetting.FindAllStringSubmatch(value, -1) {
		if templateSecretWord.MatchString(match[1]) {
			return true
		}
	}
	return false
}

func sameTemplateDefault(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// CheckTemplateValue checks a supplied input without putting its value in an error.
func CheckTemplateValue(in *spec.ComposeTemplateInput, value string) error {
	if len(value) > 4096 || strings.ContainsRune(value, 0) {
		return errors.New("a template input is too large or contains a zero byte")
	}
	switch in.GetKind() {
	case spec.ComposeTemplateInputKind_COMPOSE_TEMPLATE_INPUT_KIND_UNSPECIFIED:
		return errors.New("choose an input type")
	case templateText:
	case templateSecret:
		if value == "" {
			return errors.New("give the secret a value or generate one")
		}
	case templatePort:
		n, err := strconv.ParseUint(value, 10, 16)
		if err != nil || n == 0 {
			return errors.New("enter a port from 1 to 65535")
		}
	case templateImage:
		if _, err := reference.ParseNormalizedNamed(value); err != nil {
			return errors.New("enter a valid image name or tag")
		}
	}
	if in.GetKind() != templateSecret && service.SecretShape(value) {
		return errors.New("this input looks like a credential. Use a secret environment input")
	}
	return nil
}

// TemplateKind is a project-scoped resource with no agent placement.
const TemplateKind resource.Kind = "compose.template"

// TemplateType registers the portable definition in the revision engine.
type TemplateType struct{}

// Kind identifies the revisioned template kind.
func (TemplateType) Kind() resource.Kind { return TemplateKind }

// Level places the definition in a project.
func (TemplateType) Level() resource.Level { return resource.LevelProject }

// IDPrefix identifies template IDs.
func (TemplateType) IDPrefix() string { return id.ComposeTemplate }

// New allocates the template spec.
func (TemplateType) New() proto.Message { return &spec.ComposeTemplate{} }

// CheckName checks that a template name can appear in paths.
func (TemplateType) CheckName(name string) error {
	return resource.CheckLabel("Compose template", name, "example-stack")
}

// Validate checks and normalizes a saved template definition.
func (TemplateType) Validate(message proto.Message) error {
	draft := message.(*spec.ComposeTemplate)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return ValidateTemplate(ctx, draft, nil)
}

// TemplateGitPath permits one regular repository-relative Compose path.
func TemplateGitPath(file string) bool {
	return len(file) <= 512 && fs.ValidPath(file) && file != "." &&
		!strings.Contains(file, "\\") && !strings.ContainsFunc(file, unicode.IsControl) &&
		(path.Ext(file) == ".yaml" || path.Ext(file) == ".yml")
}
