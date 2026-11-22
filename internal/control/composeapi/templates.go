// SPDX-License-Identifier: AGPL-3.0-only

package composeapi

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/compose"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/paging"
	"github.com/dockwell/dockwell/internal/gitfetch"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

func templateMissing() error {
	return authapi.ReasonError(connect.CodeNotFound, errors.New("there is no Compose template or project by that ID"), authapi.ReasonNotFound)
}

func templateRefusal(err error) *v1.ComposeRefusal {
	var issue *compose.TemplateIssue
	if errors.As(err, &issue) {
		return &v1.ComposeRefusal{Message: issue.Reason, Line: int32(issue.Line)} //nolint:gosec // bounded Compose file
	}
	return &v1.ComposeRefusal{Message: err.Error()}
}

func (h *Handler) readTemplate(ctx context.Context, tx *store.Tx, r store.Revision, archived bool) (*v1.ComposeTemplateSummary, *spec.ComposeTemplate, error) {
	if r.Spec == nil {
		return nil, nil, templateMissing()
	}
	body, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
	if err != nil {
		return nil, nil, err
	}
	t := body.(*spec.ComposeTemplate)
	return &v1.ComposeTemplateSummary{
		Id: r.ID, ProjectId: r.Scope, Name: r.Name, Revision: int64(r.Revision),
		Description: t.GetDescription(), InputCount: int32(len(t.GetInputs())), //nolint:gosec // at most 128 validated inputs
		UpdatedAt: timestamppb.New(r.CreatedAt), Archived: archived,
	}, t, nil
}

func templateProject(ctx context.Context, tx *store.Tx, projectID string) (store.Resource, error) {
	if !strings.HasPrefix(projectID, id.Project+"_") {
		return store.Resource{}, authapi.Invalid(errors.New("select a project by ID"))
	}
	p, err := tx.GetResourceByID(ctx, projectID)
	if errors.Is(err, store.ErrNotFound) || err == nil && (p.Kind != string(scope.ProjectKind) || p.Spec == nil) {
		return store.Resource{}, templateMissing()
	}
	return p, err
}

// ListComposeTemplates lists the current recipes in a project.
func (h *Handler) ListComposeTemplates(ctx context.Context, req *connect.Request[v1.ListComposeTemplatesRequest]) (*connect.Response[v1.ListComposeTemplatesResponse], error) {
	m := req.Msg
	query := strings.ToLower(strings.TrimSpace(m.GetQuery()))
	if len(query) > 100 || len(m.GetPageToken()) > 512 {
		return nil, authapi.Invalid(errors.New("the search or page token is too long"))
	}
	n, err := paging.Size(m.GetPageSize(), 30, 100)
	if err != nil {
		return nil, err
	}
	cursor, hasCursor, err := paging.Read(m.GetPageToken(), "ListComposeTemplates")
	if err != nil {
		return nil, err
	}
	key := m.GetProjectId() + "\x00" + query + "\x00"
	after := ""
	if hasCursor {
		var ok bool
		after, ok = strings.CutPrefix(cursor.ID, key)
		if !ok || (compose.TemplateType{}).CheckName(after) != nil {
			return nil, authapi.Invalid(errors.New("start the template search from the first page"))
		}
	}
	out := &v1.ListComposeTemplatesResponse{}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		if _, err := templateProject(ctx, tx, m.GetProjectId()); err != nil {
			return err
		}
		rows, err := tx.PageProjectResources(ctx, m.GetProjectId(), string(compose.TemplateKind), after, query, n+1)
		if err != nil {
			return err
		}
		for _, r := range rows[:min(len(rows), n)] {
			rev, err := tx.GetRevision(ctx, r.ID, r.Revision)
			if err != nil {
				return err
			}
			summary, _, err := h.readTemplate(ctx, tx, rev, false)
			if err != nil {
				return err
			}
			out.Templates = append(out.Templates, summary)
		}
		if len(rows) > n {
			out.NextPageToken = paging.Token(paging.Cursor{ID: key + rows[n-1].Name})
		}
		return nil
	})
	if err != nil {
		return nil, h.templateError(ctx, err)
	}
	return connect.NewResponse(out), nil
}

func (h *Handler) templateError(ctx context.Context, err error) error {
	var rpc *connect.Error
	switch {
	case errors.As(err, &rpc):
		return rpc
	case errors.Is(err, store.ErrNotFound):
		return templateMissing()
	default:
		return h.internal(ctx, err)
	}
}

func (h *Handler) templateAt(ctx context.Context, tx *store.Tx, templateID string, number int64, live bool) (store.Revision, bool, error) {
	current, err := tx.GetResourceByID(ctx, templateID)
	if err != nil {
		return store.Revision{}, false, err
	}
	if current.Kind != string(compose.TemplateKind) || live && current.Spec == nil {
		return store.Revision{}, false, templateMissing()
	}
	if _, err := templateProject(ctx, tx, current.Scope); err != nil {
		return store.Revision{}, false, err
	}
	if number == 0 {
		number = int64(current.Revision)
	}
	if number < 1 || number > int64(current.Revision) {
		return store.Revision{}, false, templateMissing()
	}
	r, err := tx.GetRevision(ctx, current.ID, int(number))
	return r, current.Spec == nil, err
}

// GetComposeTemplate gets a saved definition and optional revision history.
func (h *Handler) GetComposeTemplate(ctx context.Context, req *connect.Request[v1.GetComposeTemplateRequest]) (*connect.Response[v1.GetComposeTemplateResponse], error) {
	m := req.Msg
	n, err := paging.Size(m.GetHistoryPageSize(), 0, 50)
	if err != nil {
		return nil, err
	}
	before := 0
	if raw := m.GetHistoryPageToken(); raw != "" {
		suffix, ok := strings.CutPrefix(raw, m.GetId()+".")
		before, err = strconv.Atoi(suffix)
		if !ok || err != nil || before < 1 || n == 0 {
			return nil, authapi.Invalid(errors.New("start template history from the first page"))
		}
	}
	out := &v1.GetComposeTemplateResponse{}
	err = h.db.View(ctx, func(tx *store.Tx) error {
		r, archived, err := h.templateAt(ctx, tx, m.GetId(), m.GetRevision(), false)
		if err != nil {
			return err
		}
		out.Template, out.Spec, err = h.readTemplate(ctx, tx, r, archived)
		if err != nil || n == 0 {
			return err
		}
		rows, err := tx.PageRevisions(ctx, r.ID, before, n+1)
		if err != nil {
			return err
		}
		for i := range min(n, len(rows)) {
			r := &rows[i]
			if r.Spec != nil {
				out.History = append(out.History, &v1.ComposeTemplateRevision{Revision: int64(r.Revision), ChangedAt: timestamppb.New(r.CreatedAt), ChangeId: r.ChangeID})
			}
		}
		if len(rows) > n {
			out.NextHistoryPageToken = fmt.Sprintf("%s.%d", r.ID, rows[n-1].Revision)
		}
		return nil
	})
	if err != nil {
		return nil, h.templateError(ctx, err)
	}
	return connect.NewResponse(out), nil
}

// PrepareComposeTemplate reduces one source to a portable definition.
func (h *Handler) PrepareComposeTemplate(ctx context.Context, req *connect.Request[v1.PrepareComposeTemplateRequest]) (*connect.Response[v1.PrepareComposeTemplateResponse], error) {
	m := req.Msg
	project := m.GetProjectId()
	err := h.db.View(ctx, func(tx *store.Tx) error {
		if _, err := templateProject(ctx, tx, project); err != nil {
			return err
		}
		if from := m.GetStack(); from != nil {
			env, err := tx.GetResourceByID(ctx, from.GetEnvironmentId())
			if err != nil {
				return err
			}
			if env.Kind != string(scope.EnvironmentKind) || env.Spec == nil || env.Scope != project {
				return templateMissing()
			}
		}
		return nil
	})
	if err != nil {
		return nil, h.templateError(ctx, err)
	}
	out := &v1.PrepareComposeTemplateResponse{}
	draft := &spec.ComposeTemplate{ComposeYaml: m.GetComposeYaml()}
	var bindings compose.TemplateBindings
	switch src := m.GetSource().(type) {
	case *v1.PrepareComposeTemplateRequest_Stack:
		b, err := h.workspace(ctx, src.Stack.GetEnvironmentId(), true)
		if err != nil {
			if _, ok := errors.AsType[*compose.TemplateIssue](err); ok {
				out.Refusal = templateRefusal(err)
				return connect.NewResponse(out), nil
			}
			return nil, err
		}
		bindings = b.templateBindings
		draft.ComposeYaml = b.info.GetWorkspace().GetYaml()
		out.Omissions = b.info.GetOmissions()
		for _, omit := range out.Omissions {
			switch omit.GetPath() {
			case "build source", "runtime", "workload", "driver_opts":
				out.Refusal = &v1.ComposeRefusal{Message: "this workspace needs a build source, runtime or driver options that a portable template cannot carry"}
				return connect.NewResponse(out), nil
			}
		}
	case *v1.PrepareComposeTemplateRequest_Git:
		draft, err = h.readTemplateGit(ctx, src.Git)
	case *v1.PrepareComposeTemplateRequest_ComposeYaml:
	default:
		return nil, authapi.Invalid(errors.New("choose a Compose file, workspace or Git source"))
	}
	if err == nil {
		err = compose.ValidateTemplate(ctx, draft, bindings)
	}
	if err != nil {
		out.Refusal = templateRefusal(err)
	} else {
		out.Draft = draft
	}
	return connect.NewResponse(out), nil
}

var templateGitReads = make(chan struct{}, 2)

func (h *Handler) readTemplateGit(ctx context.Context, src *v1.ComposeTemplateGitSource) (*spec.ComposeTemplate, error) {
	if len(src.GetRepository()) > 512 || len(src.GetRef()) > 256 {
		return nil, errors.New("enter a short repository URL and ref")
	}
	u, err := gitfetch.CheckURL(src.GetRepository())
	file, ref := src.GetPath(), strings.TrimSpace(src.GetRef())
	if file == "" {
		file = "compose.yaml"
	}
	if err != nil || !compose.TemplateGitPath(file) {
		return nil, errors.New("enter a public HTTPS repository and one repository-relative YAML path")
	}
	select {
	case templateGitReads <- struct{}{}:
		defer func() { <-templateGitReads }()
	default:
		return nil, errors.New("two Git templates are being read. Try again in a moment")
	}
	ctx, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	commit := ref
	if !gitfetch.Commit.MatchString(commit) {
		refs, err := gitfetch.List(ctx, h.gitClient, u.String())
		if err != nil {
			return nil, errors.New("could not read the repository refs over HTTPS")
		}
		if ref == "" {
			ref = refs.Head
		}
		commit = refs.Refs[ref]
		if commit == "" {
			commit = refs.Refs["refs/heads/"+ref]
		}
		if commit == "" {
			commit = refs.Refs["refs/tags/"+ref]
		}
		if commit == "" {
			return nil, errors.New("choose a branch, tag or full commit that exists")
		}
	}
	lim := gitfetch.TreeLimits{Trees: 4 << 20, Pack: 16 << 20, Entries: 4000, Depth: 8, Files: 1, File: compose.MaxFile, Total: compose.MaxFile}
	snap, err := gitfetch.Trees(ctx, h.gitClient, u.String(), commit, lim)
	if err != nil {
		return nil, errors.New("could not read that commit within the Git import limits")
	}
	defer snap.Close()
	dir := path.Dir(file)
	if dir == "." {
		dir = ""
	}
	entries, err := snap.Walk(dir, nil)
	if err != nil {
		return nil, errors.New("the Git directory did not contain a readable Compose file")
	}
	for _, e := range entries {
		if e.Dir || e.Path != file {
			continue
		}
		blobs, _, err := snap.Read(ctx, []gitfetch.Entry{e})
		if err != nil || len(blobs[file]) == 0 {
			break
		}
		return &spec.ComposeTemplate{ComposeYaml: string(blobs[file]), Git: &spec.ComposeTemplateGit{
			Repository: u.String(), Ref: ref, Commit: commit, Path: file,
		}}, nil
	}
	return nil, errors.New("choose an existing regular Compose file up to 256 KiB")
}

// PlanFromComposeTemplate plans a fresh workspace from one exact revision.
func (h *Handler) PlanFromComposeTemplate(ctx context.Context, req *connect.Request[v1.PlanFromComposeTemplateRequest]) (*connect.Response[v1.PlanFromComposeTemplateResponse], error) {
	m := req.Msg
	target := m.GetDestination()
	if m.GetRevision() < 1 || !strings.HasPrefix(target.GetServerId(), id.Server+"_") {
		return nil, authapi.Invalid(errors.New("select an exact template revision and a server"))
	}
	if err := (scope.EnvironmentType{}).CheckName(target.GetEnvironmentName()); err != nil {
		return nil, authapi.Invalid(err)
	}
	var t *spec.ComposeTemplate
	var name string
	var dest destination
	err := h.db.View(ctx, func(tx *store.Tx) error {
		row, _, err := h.templateAt(ctx, tx, m.GetId(), m.GetRevision(), true)
		if err != nil {
			return err
		}
		p, err := templateProject(ctx, tx, row.Scope)
		if err != nil {
			return err
		}
		env := resource.Ref{Kind: scope.EnvironmentKind, Scope: p.ID, Name: target.GetEnvironmentName()}
		if _, err := tx.GetResource(ctx, string(env.Kind), env.Scope, env.Name); err == nil {
			return connect.NewError(connect.CodeAlreadyExists, errors.New("that environment name has been used. Choose a new name"))
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		_, t, err = h.readTemplate(ctx, tx, row, false)
		envID := id.New(id.Environment)
		dest = destination{organization: p.Scope, project: p.Name, environment: envID, made: []revision.Edit{
			{Ref: env, ID: envID, Spec: &spec.Environment{}, MustCreate: true},
		}}
		name = row.Name
		return err
	})
	if err != nil {
		return nil, h.templateError(ctx, err)
	}
	values, secrets, stands, err := templateValues(t, m.GetInputs())
	if err != nil {
		return connect.NewResponse(&v1.PlanFromComposeTemplateResponse{Refusal: templateRefusal(err)}), nil
	}
	stack, err := compose.ParseWorkspace(ctx, []byte(t.GetComposeYaml()), values)
	if err != nil {
		line, _ := compose.Lines(ctx, []byte(t.GetComposeYaml()), err)
		return connect.NewResponse(&v1.PlanFromComposeTemplateResponse{Refusal: templateRefusal(&compose.TemplateIssue{Line: line, Reason: "an input does not fit its Compose field"})}), nil
	}
	useSecrets(stack, stands)
	for _, service := range stack.Services {
		for _, value := range service.Spec.GetApp().GetEnv() {
			if strings.HasPrefix(value, "dockwell-template-secret-") {
				return nil, authapi.Invalid(errors.New("a secret could not be bound to its environment field"))
			}
		}
	}
	notes := []revision.Note{{Reason: "compose_template", Message: fmt.Sprintf("From the %s template, revision %d", name, m.GetRevision())}}
	for key := range secrets {
		delete(values, key)
	}
	c, err := h.plan(ctx, &v1.ImportComposeRequest{Summary: "Use Compose template " + name, Servers: []string{target.GetServerId()}, Variables: values},
		stack, dest, planning{secrets: secrets, notes: notes, createOnly: true})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.PlanFromComposeTemplateResponse{Change: c}), nil
}

func templateValues(t *spec.ComposeTemplate, given []*v1.ComposeTemplateValue) (values, secrets, stands map[string]string, err error) {
	if len(given) > len(t.GetInputs()) {
		return nil, nil, nil, errors.New("supply only this template's inputs")
	}
	byName := make(map[string]*v1.ComposeTemplateValue, len(given))
	for _, in := range given {
		if in.GetChoice() == nil || byName[in.GetName()] != nil {
			return nil, nil, nil, errors.New("give each input one value")
		}
		byName[in.GetName()] = in
	}
	values, secrets, stands = map[string]string{}, map[string]string{}, map[string]string{}
	for _, in := range t.GetInputs() {
		name := in.GetName()
		v := byName[name]
		delete(byName, name)
		secret := in.GetKind() == spec.ComposeTemplateInputKind_COMPOSE_TEMPLATE_INPUT_KIND_SECRET
		value := in.GetDefaultValue()
		switch v.GetChoice().(type) {
		case *v1.ComposeTemplateValue_Value:
			value = v.GetValue()
		case *v1.ComposeTemplateValue_Generate:
			if !secret || !v.GetGenerate() {
				return nil, nil, nil, errors.New("only secret inputs can be generated")
			}
			value = rand.Text() + rand.Text()
		default:
			if in.GetRequired() {
				return nil, nil, nil, fmt.Errorf("enter a value for %s", name)
			}
		}
		if err := compose.CheckTemplateValue(in, value); err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		if secret {
			secrets[name] = value
			value = "dockwell-template-secret-" + rand.Text()
			stands[name] = value
		}
		values[name] = value
	}
	if len(byName) != 0 {
		return nil, nil, nil, errors.New("remove inputs the template does not use")
	}
	return values, secrets, stands, nil
}
