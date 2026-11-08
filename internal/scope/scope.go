// SPDX-License-Identifier: AGPL-3.0-only

// Package scope is where things live: the organization, its projects, and
// their environments. Projects and environments are kinds in the resource
// model, with specs and revisions, so making or deleting one is a change
// with a plan like any other. They apply on the control plane alone.
//
// It also holds the rule that keeps them out of sight (PRD principle 4):
// an install starts with one organization, a project named "default" and
// its environment "production", and a surface shows a level only once it
// offers a choice. Shown says which levels to show; Resolve says what a
// request that leaves a level out means. See
// docs/adr/0014-object-model-and-store.md.
package scope

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/website"
)

// Kinds.
const (
	ProjectKind     resource.Kind = "project"
	EnvironmentKind resource.Kind = "environment"
)

// The hidden defaults an install starts with.
const (
	DefaultOrganization = "default"
	DefaultProject      = "default"
	DefaultEnvironment  = "production"
)

// Types returns the types of the project and environment kinds, for a
// registry.
func Types() []resource.Type { return []resource.Type{ProjectType{}, EnvironmentType{}} }

// ProjectType is the project kind. Projects live in the organization.
type ProjectType struct{}

// Kind returns "project".
func (ProjectType) Kind() resource.Kind { return ProjectKind }

// Level is the organization.
func (ProjectType) Level() resource.Level { return resource.LevelOrganization }

// IDPrefix is "prj".
func (ProjectType) IDPrefix() string { return id.Project }

// New returns an empty Project.
func (ProjectType) New() proto.Message { return &resourcev1.Project{} }

// Validate trims the display name, and checks what a website serves.
func (ProjectType) Validate(spec proto.Message) error {
	p := spec.(*resourcev1.Project)
	p.DisplayName = strings.TrimSpace(p.GetDisplayName())
	if p.GetWebsite() != nil {
		return website.Check(p.GetWebsite())
	}
	return nil
}

// Keep holds on to what a project is pinned to, and what it serves as a website, as
// an edit replaces a whole spec and no one renaming a project means to forget where
// it came from or what it is.
func (ProjectType) Keep(current, desired proto.Message) (resource.Kept, error) {
	d, c := desired.(*resourcev1.Project), current.(*resourcev1.Project)
	if d.GetCatalog() == nil {
		d.Catalog = c.GetCatalog()
	}
	if d.GetWebsite() == nil {
		d.Website = c.GetWebsite()
	}
	return resource.Kept{}, nil
}

// CheckInChange keeps what a website serves in step with what serves it: a change
// to Project.website comes with the route WebsiteService writes for it, so a plain
// edit of the project can't leave it saying one thing while its web server runs
// another.
func (ProjectType) CheckInChange(current, desired proto.Message, others []resource.Ref) error {
	var was *resourcev1.Website
	if c, ok := current.(*resourcev1.Project); ok {
		was = c.GetWebsite()
	}
	d, _ := desired.(*resourcev1.Project)
	if proto.Equal(was, d.GetWebsite()) {
		return nil
	}
	for _, r := range others {
		if r.Kind == routeKind && r.Name == website.RouteName {
			return nil
		}
	}
	return errors.New("what a website serves changes through Websites, or dockwell websites update, which writes its route and web server with it. Leave website out of the project's edit to keep what it serves")
}

// routeKind is the route kind, which this package can't import.
const routeKind resource.Kind = "route"

// CheckName requires a DNS label, fit for URLs and the CLI.
func (ProjectType) CheckName(name string) error { return resource.CheckLabel("project", name, "shop") }

// EnvironmentType is the environment kind. Environments live in a project.
type EnvironmentType struct{}

// Kind returns "environment".
func (EnvironmentType) Kind() resource.Kind { return EnvironmentKind }

// Level is the project.
func (EnvironmentType) Level() resource.Level { return resource.LevelProject }

// IDPrefix is "env".
func (EnvironmentType) IDPrefix() string { return id.Environment }

// New returns an empty Environment.
func (EnvironmentType) New() proto.Message { return &resourcev1.Environment{} }

// Validate trims the display name.
func (EnvironmentType) Validate(spec proto.Message) error {
	e := spec.(*resourcev1.Environment)
	e.DisplayName = strings.TrimSpace(e.GetDisplayName())
	return nil
}

// CheckName requires a DNS label, fit for URLs and the CLI.
func (EnvironmentType) CheckName(name string) error {
	return resource.CheckLabel("environment", name, "staging")
}

// Tree is the organization with its live projects and their environments.
type Tree struct {
	Organization store.Organization
	Projects     []Project // by name
}

// Project is a live project.
type Project struct {
	ID          string
	Name        string
	DisplayName string
	// Environments are its live environments, by name. A project always
	// has at least one.
	Environments []Environment
}

// Environment is a live environment.
type Environment struct {
	ID          string
	Name        string
	DisplayName string
	Project     string // the project's ID
}

// Load reads the organization's projects and environments.
func Load(ctx context.Context, tx *store.Tx) (Tree, error) {
	org, err := tx.Organization(ctx)
	if err != nil {
		return Tree{}, err
	}
	t := Tree{Organization: org}
	projects, err := tx.ListResources(ctx, org.ID, string(ProjectKind))
	if err != nil {
		return Tree{}, err
	}
	for _, r := range projects {
		var spec resourcev1.Project
		if err := proto.Unmarshal(r.Spec, &spec); err != nil {
			return Tree{}, fmt.Errorf("can't read project %s: %w", r.Name, err)
		}
		p := Project{ID: r.ID, Name: r.Name, DisplayName: spec.GetDisplayName()}
		envs, err := tx.ListResources(ctx, r.ID, string(EnvironmentKind))
		if err != nil {
			return Tree{}, err
		}
		for _, e := range envs {
			var spec resourcev1.Environment
			if err := proto.Unmarshal(e.Spec, &spec); err != nil {
				return Tree{}, fmt.Errorf("can't read environment %s: %w", e.Name, err)
			}
			p.Environments = append(p.Environments, Environment{ID: e.ID, Name: e.Name, DisplayName: spec.GetDisplayName(), Project: r.ID})
		}
		t.Projects = append(t.Projects, p)
	}
	return t, nil
}

// Shown says which levels of the scope a surface shows: the scope bar in
// the panel, columns and filters in lists, and flags and output in the
// CLI. A level shows once it offers a choice, and hides again when it
// stops offering one:
//   - the organization, once a person can reach two (not before
//     federation, so never yet);
//   - the project, once the organization has two live projects;
//   - the environment, once the project in view has two live
//     environments, or, in a view across projects, once any project has.
//
// Deleted projects and environments don't count, and nor do names: a
// renamed default is still the only one. Requests follow the same rule
// (see Resolve), so a level that isn't shown is never asked for.
type Shown struct {
	Organization bool
	Project      bool
	Environment  bool
}

// Shown returns which levels to show for the project named by ID or name,
// or across all projects when project is empty.
func (t Tree) Shown(project string) Shown {
	s := Shown{Project: len(t.Projects) > 1}
	for _, p := range t.Projects {
		if project == "" || p.ID == project || p.Name == project {
			s.Environment = s.Environment || len(p.Environments) > 1
		}
	}
	return s
}

// ErrNotFound means a request named a project, environment or server that
// doesn't exist.
var ErrNotFound = errors.New("no such scope")

// ServerLevel is the level a ChoiceError names when a request leaves out
// which server a host-level resource, such as a firewall rule, lives on,
// and there are several.
const ServerLevel resource.Kind = "server"

// ChoiceError means a request left out a level that offers a choice. It
// names the level and the options, so a surface can ask which.
type ChoiceError struct {
	Level   resource.Kind // ProjectKind, EnvironmentKind or ServerLevel
	Project string        // the project's name, for an environment
	Options []string      // names
}

func (e *ChoiceError) Error() string {
	switch e.Level {
	case EnvironmentKind:
		return fmt.Sprintf("project %s has %d environments, %s, so say which one you mean", e.Project, len(e.Options), human.List(e.Options))
	case ServerLevel:
		return fmt.Sprintf("there are %d servers, %s, so say which one you mean", len(e.Options), human.List(e.Options))
	}
	return fmt.Sprintf("this organization has %d projects, %s, so say which one you mean", len(e.Options), human.List(e.Options))
}

// Resolve finds the environment a request means, from a project and an
// environment, each given by name or ID. A level left empty means the only
// one there is; once there are several, leaving it out is a *ChoiceError.
// An environment given by ID needs no project.
func (t Tree) Resolve(project, environment string) (Project, Environment, error) {
	if strings.HasPrefix(environment, id.Environment+"_") {
		for _, p := range t.Projects {
			for _, e := range p.Environments {
				if e.ID == environment && (project == "" || project == p.ID || project == p.Name) {
					return p, e, nil
				}
			}
		}
		return Project{}, Environment{}, fault.New(ErrNotFound, "there's no environment %s%s", environment, inProject(project))
	}
	p, err := t.Project(project)
	if err != nil {
		return Project{}, Environment{}, err
	}
	if environment == "" {
		if len(p.Environments) == 1 {
			return p, p.Environments[0], nil
		}
		return Project{}, Environment{}, &ChoiceError{Level: EnvironmentKind, Project: p.Name, Options: names(p.Environments)}
	}
	for _, e := range p.Environments {
		if e.ID == environment || e.Name == environment {
			return p, e, nil
		}
	}
	return Project{}, Environment{}, fault.New(ErrNotFound, "project %s has no environment %s. Its environments are %s",
		p.Name, environment, human.List(names(p.Environments)))
}

// Project finds a live project by name or ID. Empty means the only one
// there is; once there are several, leaving it out is a *ChoiceError.
func (t Tree) Project(project string) (Project, error) {
	if project == "" {
		switch len(t.Projects) {
		case 1:
			return t.Projects[0], nil
		case 0:
			return Project{}, fault.New(ErrNotFound, "the organization has no projects yet. Make one first")
		}
		return Project{}, &ChoiceError{Level: ProjectKind, Options: projectNames(t.Projects)}
	}
	for _, p := range t.Projects {
		if p.ID == project || p.Name == project {
			return p, nil
		}
	}
	return Project{}, fault.New(ErrNotFound, "there's no project %s. The projects are %s", project, human.List(projectNames(t.Projects)))
}

func inProject(project string) string {
	if project == "" {
		return ""
	}
	return " in project " + project
}

func names(envs []Environment) []string {
	out := make([]string, len(envs))
	for i, e := range envs {
		out[i] = e.Name
	}
	return out
}

func projectNames(projects []Project) []string {
	out := make([]string, len(projects))
	for i, p := range projects {
		out[i] = p.Name
	}
	return out
}
