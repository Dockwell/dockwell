// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// Overlay is what a person wrote for one app: fields of its entry, in the entry's own
// format and our own words, and what the app reached when they looked.
type Overlay struct {
	Entry *catalogv1.Entry
	// Reaches is what the app reached on its server when a person looked, each as
	// the report says it. An app that reaches anything else, or less, is left out
	// until a person looks again, as host_access would no longer say what it does.
	Reaches []string
}

// ReadOverlay reads an overlay: an entry's fields, and reaches, a list.
func ReadOverlay(raw []byte) (Overlay, error) {
	var tree map[string]any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return Overlay{}, fmt.Errorf("reading its YAML: %w", err)
	}
	var o Overlay
	if r, ok := tree["reaches"]; ok {
		list, ok := r.([]any)
		if !ok {
			return Overlay{}, fmt.Errorf("its reaches is %T, where the converter reads a list", r)
		}
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return Overlay{}, fmt.Errorf("its reaches has %v, where the converter reads text", item)
			}
			o.Reaches = append(o.Reaches, s)
		}
		delete(tree, "reaches")
	}
	rest, err := yaml.Marshal(tree)
	if err != nil {
		return Overlay{}, err
	}
	if o.Entry, err = catalog.Decode(rest); err != nil {
		return Overlay{}, err
	}
	return o, nil
}

// ReadOverlays reads a folder of overlays, one <id>.yaml each.
func ReadOverlays(dir string) (map[string]Overlay, error) {
	out := map[string]Overlay{}
	if dir == "" {
		return out, nil
	}
	t, err := openTree(dir)
	if err != nil {
		return nil, fmt.Errorf("reading the overlays: %w", err)
	}
	defer t.Close()
	ents, err := t.readDir(".")
	if err != nil {
		return nil, fmt.Errorf("reading the overlays: %w", err)
	}
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".yaml")
		if !ok || e.IsDir() {
			continue
		}
		raw, err := t.readFile(e.Name())
		if err != nil {
			return nil, fmt.Errorf("reading the overlay %s: %w", e.Name(), err)
		}
		o, err := ReadOverlay(raw)
		if err != nil {
			return nil, fmt.Errorf("the overlay %s: %w", e.Name(), err)
		}
		out[id] = o
	}
	return out, nil
}

// applyOverlay takes what a person wrote over what the converter made. Each field
// they set wins. An input of theirs that names a type is the input, in place of the
// converter's, so they can make one a secret, generate it, or take away a default
// everyone would share. One that names no type changes only its words.
func applyOverlay(e *catalogv1.Entry, o Overlay) error {
	w := o.Entry
	for to, from := range map[*string]string{
		&e.Name: w.GetName(), &e.Summary: w.GetSummary(), &e.Category: w.GetCategory(), &e.Homepage: w.GetHomepage(),
		&e.AppLicense: w.GetAppLicense(), &e.FirstSteps: w.GetFirstSteps(), &e.HostAccess: w.GetHostAccess(),
		&e.Description: w.GetDescription(), &e.Icon: w.GetIcon(), &e.Documentation: w.GetDocumentation(), &e.AppSource: w.GetAppSource(),
	} {
		if from != "" {
			*to = from
		}
	}
	if w.GetWeb() != nil {
		e.Web = proto.CloneOf(w.GetWeb())
	}
	if len(w.GetFeatures()) > 0 {
		e.Features = slices.Clone(w.GetFeatures())
	}
	if len(w.GetScreenshots()) > 0 {
		e.Screenshots = make([]*catalogv1.Screenshot, len(w.GetScreenshots()))
		for i, shot := range w.GetScreenshots() {
			e.Screenshots[i] = proto.CloneOf(shot)
		}
	}
	for _, in := range w.GetInputs() {
		i := slices.IndexFunc(e.GetInputs(), func(have *catalogv1.Input) bool { return have.GetName() == in.GetName() })
		if i < 0 {
			return leave(ClassOverlay, "its overlay names the input %s, which it no longer has. Take it out of the overlay", in.GetName())
		}
		if in.GetType() != catalogv1.InputType_INPUT_TYPE_UNSPECIFIED {
			replaced := proto.CloneOf(in)
			replaced.Label = cmp.Or(replaced.GetLabel(), e.GetInputs()[i].GetLabel())
			e.Inputs[i] = replaced
			continue
		}
		have := e.GetInputs()[i]
		have.Label = cmp.Or(in.GetLabel(), have.GetLabel())
		have.Hint = cmp.Or(in.GetHint(), have.GetHint())
		have.DefaultValue = cmp.Or(in.GetDefaultValue(), have.GetDefaultValue())
		have.Optional = have.GetOptional() || in.GetOptional()
	}
	return nil
}
