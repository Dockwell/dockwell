// SPDX-License-Identifier: AGPL-3.0-only

package composeapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/appowner"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/resourcepb"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/volume"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

type workspaceRecord struct {
	revision int
	spec     proto.Message
	servers  []string
}

type workspaceBase struct {
	info        *v1.GetComposeWorkspaceResponse
	records     map[resource.Ref]workspaceRecord
	vars        map[string]string
	projectName string
}

// GetComposeWorkspace reads the portable projection of a managed environment.
func (h *Handler) GetComposeWorkspace(ctx context.Context, req *connect.Request[v1.GetComposeWorkspaceRequest]) (*connect.Response[v1.GetComposeWorkspaceResponse], error) {
	base, err := h.workspace(ctx, req.Msg.GetEnvironmentId())
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(base.info), nil
}

func (h *Handler) workspace(ctx context.Context, scopeID string) (*workspaceBase, error) {
	if !strings.HasPrefix(scopeID, id.Environment+"_") {
		return nil, authapi.Invalid(errors.New("a Compose workspace needs the environment ID"))
	}
	project, env, err := h.changes.Environment(ctx, "", scopeID)
	if err != nil {
		return nil, err
	}
	name := project.DisplayName
	if name == "" {
		name = project.Name
	}
	out := &v1.GetComposeWorkspaceResponse{Workspace: &v1.ComposeWorkspace{
		EnvironmentId: env.ID, ProjectId: project.ID, Name: name, EnvironmentName: env.Name,
	}}
	base := &workspaceBase{
		info: out, records: map[resource.Ref]workspaceRecord{}, vars: map[string]string{}, projectName: project.Name,
	}
	digest := sha256.New()
	fmt.Fprintf(digest, "workspace-v2\x00%s\x00%s\x00%s\x00%s\x00", env.ID, env.Name, name, project.Name)
	stack := &compose.Stack{LogDrivers: map[string]string{}}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		list, err := tx.ListResources(ctx, env.ID, "")
		if err != nil {
			return err
		}
		for _, held := range list {
			fmt.Fprintf(digest, "%s:%d\x00", held.ID, held.Revision)
			kind := resource.Kind(held.Kind)
			switch kind {
			case service.Kind, volume.Kind, network.Kind, route.Kind, appowner.Kind:
			default:
				continue
			}
			spec, err := h.keeper.Decode(ctx, tx, store.InRevision(held.ID, held.Kind, held.Revision), held.Spec)
			if err != nil {
				return err
			}
			ref := resource.Ref{Kind: kind, Scope: env.ID, Name: held.Name}
			record := workspaceRecord{revision: held.Revision, spec: spec}
			if kind == service.Kind || kind == volume.Kind || kind == network.Kind {
				record.servers, err = tx.Placement(ctx, held.ID, held.Revision)
				if err != nil {
					return err
				}
				for _, server := range record.servers {
					fmt.Fprintf(digest, "%s\x00", server)
				}
				out.Workspace.Resources = append(out.Workspace.Resources, &v1.ComposeWorkspaceResource{
					Kind: held.Kind, Name: held.Name, ResourceId: held.ID, ServerIds: record.servers,
				})
			}
			base.records[ref] = record
			base.project(ref, spec, stack)
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	out.Snapshot = hex.EncodeToString(digest.Sum(nil))
	for key := range base.vars {
		// These values are only for parsing placeholders. The merge restores the
		// real values by the original resource and key, and rejects new uses.
		base.vars[key] = base.markerPrefix() + key
	}
	exported, err := compose.ExportWorkspace(project.Name, stack)
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	out.Workspace.Yaml = string(exported.File)
	return base, nil
}

func (b *workspaceBase) markerPrefix() string {
	return "dockwell-kept-" + b.info.GetSnapshot() + "-"
}

func (b *workspaceBase) project(ref resource.Ref, spec proto.Message, stack *compose.Stack) {
	keep := func(path string, sealed bool) {
		b.info.Omissions = append(b.info.Omissions, &v1.ComposeWorkspaceOmission{Kind: string(ref.Kind), Name: ref.Name, Path: path, Sealed: sealed})
	}
	switch s := spec.(type) {
	case *resourcev1.Service:
		if s.GetApp() == nil {
			keep("workload", false)
			return
		}
		for _, name := range binding.ReferencedNames(s.GetApp()) {
			b.vars[name] = ""
		}
		for _, key := range slices.Sorted(maps.Keys(s.GetApp().GetEnv())) {
			if !slices.Contains(s.GetShownEnv(), key) {
				b.vars[key] = ""
				keep("environment."+key, true)
			}
		}
		for _, key := range slices.Sorted(maps.Keys(s.GetSecrets())) {
			b.vars[key] = ""
			keep("environment."+key, true)
		}
		for _, key := range slices.Sorted(maps.Keys(s.GetVariables())) {
			b.vars[key] = ""
			keep("environment."+key, false)
		}
		if s.GetApp().GetDesiredRunState() != 0 {
			keep("run state", false)
		}
		if s.GetBuild() != nil {
			keep("build source", false)
		}
		if s.GetRuntime() != nil {
			keep("runtime", false)
		}
		if s.GetAutoUpdate() != nil {
			keep("automatic updates", false)
		}
		if len(s.GetArchitectures()) > 0 {
			keep("architectures", false)
		}
		// Logging is effective Engine configuration. The workspace edits the
		// saved Compose projection and leaves the existing policy in place.
		stack.LogDrivers[ref.Name] = "kept"
		stack.Services = append(stack.Services, compose.Item[*resourcev1.Service]{Name: ref.Name, Spec: s})
	case *resourcev1.Volume:
		if len(s.GetOptions()) > 0 {
			keep("driver_opts", true)
		}
		stack.Volumes = append(stack.Volumes, compose.Item[*resourcev1.Volume]{Name: ref.Name, Spec: s})
	case *resourcev1.Network:
		if len(s.GetOptions()) > 0 {
			keep("driver_opts", false)
			s = proto.CloneOf(s)
			s.Options = nil
		}
		stack.Networks = append(stack.Networks, compose.Item[*resourcev1.Network]{Name: ref.Name, Spec: s})
	case *resourcev1.Route:
		keep("route", false)
	case *resourcev1.AppOwner:
		keep("app owner", false)
	}
}

// PlanComposeWorkspace plans only the edits the draft makes to its explicit base.
func (h *Handler) PlanComposeWorkspace(ctx context.Context, req *connect.Request[v1.PlanComposeWorkspaceRequest]) (*connect.Response[v1.PlanComposeWorkspaceResponse], error) {
	m := req.Msg
	if len(m.GetYaml()) > compose.MaxFile || len(m.GetExplicitRemovals()) > 256 {
		return nil, authapi.Invalid(errors.New("the Compose draft or its selection is too large"))
	}
	base, err := h.workspace(ctx, m.GetEnvironmentId())
	if err != nil {
		return nil, err
	}
	if base.info.GetSnapshot() != m.GetSnapshot() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New("this stack changed after you opened the draft. Reload the file to review again"), "compose_workspace_stale")
	}
	removals, err := base.removals(m.GetExplicitRemovals())
	if err != nil {
		return nil, err
	}
	var edits []*v1.Edit
	mustCreate := map[resource.Ref]bool{}
	also := changes.Also{MustCreate: mustCreate}
	if m.Yaml != nil {
		draft := []byte(m.GetYaml())
		next, err := compose.ParseWorkspace(ctx, draft, base.vars)
		if err != nil {
			if !errors.Is(err, compose.ErrInvalid) {
				return nil, h.internal(ctx, err)
			}
			from, to := compose.Lines(ctx, draft, err)
			return connect.NewResponse(&v1.PlanComposeWorkspaceResponse{
				Refusal: &v1.ComposeRefusal{Message: compose.Message(err), Line: int32(from), EndLine: int32(to)}, //nolint:gosec // Compose file size limits the number of lines.
			}), nil
		}
		if next.Name != base.projectName {
			return nil, authapi.Invalid(errors.New("keep the Compose project's name. Change its display name in Projects"))
		}
		prior, err := compose.ParseWorkspace(ctx, []byte(base.info.GetWorkspace().GetYaml()), base.vars)
		if err != nil {
			if strings.Contains(base.info.GetWorkspace().GetYaml(), "services: {}") {
				prior = &compose.Stack{}
			} else {
				return nil, h.internal(ctx, err)
			}
		}
		missing := workspaceRemovals(prior, next, m.GetEnvironmentId(), removals)
		if len(missing) > 0 {
			return connect.NewResponse(&v1.PlanComposeWorkspaceResponse{Removals: missing}), nil
		}
		edits, err = base.draftEdits(prior, next, m.GetServerId(), removals, mustCreate)
		if err != nil {
			return nil, err
		}
		also.Notes = append(planNotes(m.GetEnvironmentId(), next.Notices), exposedNotes(m.GetEnvironmentId(), next.Exposed, next.ExposedMore, nil)...)
	}
	for _, ref := range removalOrder(removals) {
		record := base.records[ref]
		if m.Yaml == nil && m.GetServerId() != "" &&
			(ref.Kind == service.Kind || ref.Kind == network.Kind || ref.Kind == volume.Kind) &&
			!slices.Contains(record.servers, m.GetServerId()) {
			return nil, authapi.Invalid(fmt.Errorf("%s is not placed on the selected server", ref))
		}
		edits = append(edits, &v1.Edit{Resource: resourcepb.Ref(ref), Delete: true, BaseRevision: int64(record.revision)})
	}
	if len(edits) == 0 {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New("there are no Compose changes to review"), "compose_workspace_unchanged")
	}
	summary := "Edit Compose stack " + base.info.GetWorkspace().GetName()
	if m.Yaml == nil {
		summary = "Delete Compose workloads from " + base.info.GetWorkspace().GetName()
		message := "Volumes and their data stay."
		for ref := range removals {
			if ref.Kind == volume.Kind {
				message = "The plan removes the listed managed volumes. Other volumes and the environment stay."
				break
			}
		}
		also.Notes = append(also.Notes, revision.Note{Reason: "compose_keeps_data", Message: message})
	}
	change, err := h.changes.Propose(ctx, summary, edits, nil, also)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.PlanComposeWorkspaceResponse{Change: change}), nil
}

func (b *workspaceBase) removals(list []*v1.ComposeWorkspaceRemoval) (map[resource.Ref]bool, error) {
	out := map[resource.Ref]bool{}
	env := b.info.GetWorkspace().GetEnvironmentId()
	for _, item := range list {
		kind := resource.Kind(item.GetKind())
		ref := resource.Ref{Kind: kind, Scope: env, Name: item.GetName()}
		if b.records[ref].spec == nil || kind != service.Kind && kind != network.Kind && kind != volume.Kind {
			return nil, authapi.Invalid(errors.New("select a service, network or managed volume from this Compose base to remove"))
		}
		out[ref] = true
	}
	// Local routes and their ownership are part of removing a workload.
	// The revision engine checks all other consumers, including shared links.
	for ref, record := range b.records {
		var target string
		switch s := record.spec.(type) {
		case *resourcev1.Route:
			target = s.GetService()
		case *resourcev1.AppOwner:
			target = s.GetService()
		}
		if target != "" && out[resource.Ref{Kind: service.Kind, Scope: env, Name: target}] {
			out[ref] = true
		}
	}
	return out, nil
}

func removalOrder(refs map[resource.Ref]bool) []resource.Ref {
	order := map[resource.Kind]int{route.Kind: 0, appowner.Kind: 0, service.Kind: 1, network.Kind: 2, volume.Kind: 3}
	out := slices.Collect(maps.Keys(refs))
	slices.SortFunc(out, func(a, b resource.Ref) int {
		if n := order[a.Kind] - order[b.Kind]; n != 0 {
			return n
		}
		return strings.Compare(a.String(), b.String())
	})
	return out
}

type workspaceDraft struct {
	ordered []resource.Ref
	specs   map[resource.Ref]proto.Message
}

func workspaceSpecs(stack *compose.Stack, env string) workspaceDraft {
	out := workspaceDraft{specs: map[resource.Ref]proto.Message{}}
	add := func(kind resource.Kind, name string, spec proto.Message) {
		ref := resource.Ref{Kind: kind, Scope: env, Name: name}
		out.ordered = append(out.ordered, ref)
		out.specs[ref] = spec
	}
	for _, item := range stack.Networks {
		add(network.Kind, item.Name, item.Spec)
	}
	for _, item := range stack.Volumes {
		add(volume.Kind, item.Name, item.Spec)
	}
	for _, item := range stack.Services {
		add(service.Kind, item.Name, item.Spec)
	}
	return out
}

func workspaceRemovals(prior, next *compose.Stack, env string, selected map[resource.Ref]bool) []*v1.ComposeWorkspaceRemoval {
	old, draft := workspaceSpecs(prior, env), workspaceSpecs(next, env)
	var out []*v1.ComposeWorkspaceRemoval
	for _, ref := range old.ordered {
		if ref.Kind != volume.Kind && draft.specs[ref] == nil && !selected[ref] {
			out = append(out, &v1.ComposeWorkspaceRemoval{Kind: string(ref.Kind), Name: ref.Name})
		}
	}
	return out
}

func (b *workspaceBase) draftEdits(prior, next *compose.Stack, serverID string, removals, mustCreate map[resource.Ref]bool) ([]*v1.Edit, error) {
	env := b.info.GetWorkspace().GetEnvironmentId()
	old, draft := workspaceSpecs(prior, env), workspaceSpecs(next, env)
	servers := map[string]bool{}
	for _, r := range b.records {
		for _, server := range r.servers {
			servers[server] = true
		}
	}
	var edits []*v1.Edit
	for _, ref := range draft.ordered {
		if removals[ref] {
			return nil, authapi.Invalid(fmt.Errorf("%s is both in the draft and selected to remove", ref))
		}
		n := draft.specs[ref]
		r := b.records[ref]
		ed := &v1.Edit{Resource: resourcepb.Ref(ref)}
		if r.spec == nil {
			switch {
			case serverID != "" && servers[serverID]:
				ed.Servers = []string{serverID}
			case serverID == "" && len(servers) == 1:
				ed.Servers = slices.Collect(maps.Keys(servers))
			default:
				return nil, authapi.Invalid(errors.New("select a server in this stack for new Compose resources"))
			}
			mustCreate[ref] = true
		} else {
			prev := old.specs[ref]
			if prev == nil {
				return nil, authapi.Invalid(fmt.Errorf("%s is managed outside the Compose projection. Leave its name out of the draft", ref))
			}
			if proto.Equal(prev, n) {
				continue
			}
			merged := proto.Clone(r.spec)
			if svc, ok := merged.(*resourcev1.Service); ok {
				b.replaceWorkspaceEnvironment(ref, svc, prev.(*resourcev1.Service), n.(*resourcev1.Service))
			}
			if err := mergeWorkspace(merged.ProtoReflect(), prev.ProtoReflect(), n.ProtoReflect()); err != nil {
				return nil, authapi.Invalid(fmt.Errorf("%s: %w", ref, err))
			}
			for _, omission := range b.info.GetOmissions() {
				if omission.GetKind() != string(ref.Kind) || omission.GetName() != ref.Name {
					continue
				}
				if !keepsWorkspaceValue(r.spec, merged) {
					return nil, authapi.Invalid(fmt.Errorf("%s keeps %s outside the Compose file. Edit that setting on its own page", ref, omission.GetPath()))
				}
			}
			ed.BaseRevision = int64(r.revision)
			n = merged
		}
		raw, err := proto.Marshal(n)
		if err != nil {
			return nil, err
		}
		if bytes.Contains(raw, []byte(b.markerPrefix())) {
			return nil, authapi.Invalid(fmt.Errorf("%s uses a kept value in a new location. Set a value or link it from the app's Environment page", ref))
		}
		switch spec := n.(type) {
		case *resourcev1.Service:
			ed.Spec = &resourcev1.Spec{Kind: &resourcev1.Spec_Service{Service: spec}}
		case *resourcev1.Network:
			ed.Spec = &resourcev1.Spec{Kind: &resourcev1.Spec_Network{Network: spec}}
		case *resourcev1.Volume:
			ed.Spec = &resourcev1.Spec{Kind: &resourcev1.Spec_Volume{Volume: spec}}
		}
		edits = append(edits, ed)
	}
	return edits, nil
}

// A literal replaces the saved value or binding, and an omitted key removes it.
// An unchanged marker leaves the saved value alone. Clear only the changed
// slots in the clone so the portable merge can write the new literal there.
func (b *workspaceBase) replaceWorkspaceEnvironment(ref resource.Ref, merged, prior, draft *resourcev1.Service) {
	for _, omission := range b.info.GetOmissions() {
		if omission.GetKind() != string(ref.Kind) || omission.GetName() != ref.Name {
			continue
		}
		key, ok := strings.CutPrefix(omission.GetPath(), "environment.")
		if !ok || equalEntry(key, prior.GetApp().GetEnv(), draft.GetApp().GetEnv()) {
			continue
		}
		delete(merged.GetApp().GetEnv(), key)
		delete(merged.Secrets, key)
		delete(merged.Variables, key)
		merged.ShownEnv = slices.DeleteFunc(merged.ShownEnv, func(shown string) bool { return shown == key })
	}
}

func keepsWorkspaceValue(current, merged proto.Message) bool {
	switch s := current.(type) {
	case *resourcev1.Volume:
		return maps.Equal(s.GetOptions(), merged.(*resourcev1.Volume).GetOptions())
	case *resourcev1.Network:
		return maps.Equal(s.GetOptions(), merged.(*resourcev1.Network).GetOptions())
	}
	return true
}

func equalEntry(key string, before, after map[string]string) bool {
	a, had := before[key]
	b, has := after[key]
	return a == b && had == has
}

// mergeWorkspace applies only differences between the two portable projections.
// The stored spec is cloned first, so unrepresented fields retain their values.
func mergeWorkspace(to, base, next protoreflect.Message) error {
	fields := to.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		b, n := base.Get(fd), next.Get(fd)
		if b.Equal(n) && base.Has(fd) == next.Has(fd) {
			continue
		}
		switch {
		case fd.IsMap():
			if err := mergeWorkspaceMap(to.Mutable(fd).Map(), b.Map(), n.Map(), fd.Name()); err != nil {
				return err
			}
		case fd.IsList() && keyedLists[fd.Name()] != nil:
			if err := mergeWorkspaceList(to.Mutable(fd).List(), b.List(), n.List(), keyedLists[fd.Name()], fd.Name()); err != nil {
				return err
			}
		case fd.Message() != nil && !fd.IsList() && next.Has(fd):
			if err := mergeWorkspace(to.Mutable(fd).Message(), b.Message(), n.Message()); err != nil {
				return err
			}
		case fd.IsList() && !to.Get(fd).Equal(b):
			return fmt.Errorf("%s has settings kept outside Compose. Edit it on its own page", fd.Name())
		default:
			if next.Has(fd) {
				to.Set(fd, n)
			} else {
				to.Clear(fd)
			}
		}
	}
	return nil
}

func mergeWorkspaceMap(to, base, next protoreflect.Map, field protoreflect.Name) error {
	var conflict bool
	base.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		wanted := next.Get(k)
		if wanted.Equal(v) {
			return true
		}
		if held := to.Get(k); held.IsValid() && !held.Equal(v) {
			conflict = true
			return false
		}
		if wanted.IsValid() {
			to.Set(k, wanted)
		} else {
			to.Clear(k)
		}
		return true
	})
	if conflict {
		return fmt.Errorf("%s contains values kept outside Compose. Edit them on their own page", field)
	}
	next.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		if !base.Has(k) {
			to.Set(k, v)
		}
		return true
	})
	return nil
}

func mergeWorkspaceList(to, base, next protoreflect.List, key func(protoreflect.Value) string, field protoreflect.Name) error {
	// Reuse the grouping rules in the catalog merge, including multiple
	// published addresses for a single port.
	type entry = []protoreflect.Value
	collect := func(l protoreflect.List) map[string]entry {
		out := map[string]entry{}
		for i := range l.Len() {
			v := l.Get(i)
			k := key(v)
			out[k] = append(out[k], v)
		}
		return out
	}
	b, held, n := collect(base), collect(to), collect(next)
	for k, before := range b {
		if slices.EqualFunc(before, n[k], protoreflect.Value.Equal) || slices.EqualFunc(before, held[k], protoreflect.Value.Equal) {
			continue
		}
		if len(before) != 1 || len(held[k]) != 1 || len(n[k]) != 1 || !isWorkspaceMessage(before[0]) {
			return fmt.Errorf("%s has settings kept outside Compose. Edit it on its own page", field)
		}
		if err := mergeWorkspace(held[k][0].Message(), before[0].Message(), n[k][0].Message()); err != nil {
			return err
		}
		// The grouping merge now chooses the entry with both sets of fields.
		n[k][0] = held[k][0]
	}
	tmp := make([]protoreflect.Value, to.Len())
	for i := range to.Len() {
		tmp[i] = to.Get(i)
	}
	to.Truncate(0)
	seen := map[string]bool{}
	for _, l := range []protoreflect.List{next, base} {
		for i := range l.Len() {
			k := key(l.Get(i))
			if seen[k] {
				continue
			}
			seen[k] = true
			got := n[k]
			if slices.EqualFunc(b[k], got, protoreflect.Value.Equal) {
				got = held[k]
			}
			for _, v := range got {
				to.Append(v)
			}
		}
	}
	for _, v := range tmp {
		if !seen[key(v)] {
			to.Append(v)
		}
	}
	return nil
}

func isWorkspaceMessage(value protoreflect.Value) bool {
	_, ok := value.Interface().(protoreflect.Message)
	return ok
}
