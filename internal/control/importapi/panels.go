// SPDX-License-Identifier: AGPL-3.0-only

package importapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/composeapi"
	"github.com/dockwell/dockwell/internal/control/containers"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/revision"
)

// panel is what an import says of a panel it reads from Docker and its folders
// (docs/adr/0089-importing-from-coolify-and-dokploy.md).
type panel struct {
	// name is what people call it, and from the word its projects take where
	// Dockwell has one of an app's name, such as blog-coolify.
	name, from string
	// proxy is its proxy's container, which holds ports 80 and 443.
	proxy string
	// known is the version this import was written from.
	known string
	// autoDeploy and basicAuth are its settings for deploying on a push and for a
	// password in front of an app, as its own screens name them.
	autoDeploy, basicAuth string
}

var panels = map[dockwellv1.PanelKind]panel{
	dockwellv1.PanelKind_PANEL_KIND_COOLIFY: {name: "Coolify", from: "coolify", proxy: "coolify-proxy", known: "4.7", autoDeploy: "Auto deploy", basicAuth: "HTTP Basic Authentication"},
	dockwellv1.PanelKind_PANEL_KIND_DOKPLOY: {name: "Dokploy", from: "dokploy", proxy: "dokploy-traefik", known: "0.31", autoDeploy: "Autodeploy", basicAuth: "basic auth"},
}

// ImportCoolify reads what Coolify runs on a server, and says what an import would
// do with each thing. For a person, it plans taking on what runs as it is.
func (h *Handler) ImportCoolify(ctx context.Context, req *connect.Request[dockwellv1.ImportCoolifyRequest]) (*connect.Response[dockwellv1.ImportCoolifyResponse], error) {
	r, err := h.importPanel(ctx, req.Msg.GetServerId(), req.Msg.GetSkipPlan(), dockwellv1.PanelKind_PANEL_KIND_COOLIFY)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.ImportCoolifyResponse{Found: r.found, Steps: r.steps, Change: r.change}), nil
}

// ImportDokploy reads what Dokploy runs on a server, as ImportCoolify does.
func (h *Handler) ImportDokploy(ctx context.Context, req *connect.Request[dockwellv1.ImportDokployRequest]) (*connect.Response[dockwellv1.ImportDokployResponse], error) {
	r, err := h.importPanel(ctx, req.Msg.GetServerId(), req.Msg.GetSkipPlan(), dockwellv1.PanelKind_PANEL_KIND_DOKPLOY)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.ImportDokployResponse{Found: r.found, Steps: r.steps, Change: r.change}), nil
}

// imported is what an import from a panel found, would do, and planned.
type imported struct {
	found  *dockwellv1.Panel
	steps  []*dockwellv1.ImportStep
	change *dockwellv1.Change
}

func (h *Handler) importPanel(ctx context.Context, server string, skipPlan bool, kind dockwellv1.PanelKind) (*imported, error) {
	w := panels[kind]
	srv, err := h.fleet.Get(server)
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no server %s. List them with ListServers", server), "not_found")
	}
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	session, ok := h.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnavailable,
			fmt.Errorf("%s's agent isn't connected, so Dockwell can't read what %s runs there. Try again once it reconnects", srv.GetName(), w.name), "server_offline")
	}
	if !session.ReadsPanels() && !session.Forwards() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s's agent doesn't reach Docker, so it can't read what %s runs, which it reads from Docker. Check that Docker runs on %s, then try again", srv.GetName(), w.name, srv.GetName()), "no_docker")
	}
	if !session.ReadsPanels() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s's agent is too old to read what %s runs. Update the agent on %s, then try again", srv.GetName(), w.name, srv.GetName()), "agent_outdated")
	}
	out, err := session.ReadPanel(ctx, kind)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if out.GetFound() == nil {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s doesn't run %s: %s. Run this for a server where %s runs", srv.GetName(), w.name, cut(out.GetNotHere(), w.name+" doesn't run here: "), w.name), "panel_not_found")
	}
	steps, apps := panelSteps(w, out.GetFound())
	if !session.RemakesSwarmApps() {
		// An agent from before waits for no task to be over, so a copy could start
		// beside the one it replaces.
		for i, s := range steps {
			if apps[i] != nil && apps[i].GetSwarm() && s.GetAction() == dockwellv1.ImportAction_IMPORT_ACTION_MAKE {
				leave(s, fmt.Sprintf("%s's agent is too old to make a Swarm-run app again safely. Update its agent, then import again. It stays as it is, in %s.", srv.GetName(), w.name))
			}
		}
	}
	res := &imported{found: out.GetFound(), steps: steps}
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok || p.Token != nil {
		return res, nil
	}
	if res.change, err = h.planPanel(ctx, srv, w, steps, apps, !skipPlan); err != nil {
		return nil, err
	}
	if !skipPlan {
		h.supersede(ctx, srv.GetId()+"/"+w.from+"/"+p.User.ID, res.change.GetId())
	}
	return res, nil
}

// cut is s without prefix.
func cut(s, prefix string) string {
	return strings.TrimPrefix(s, prefix)
}

// panelSteps are what an import would do with each app the panel runs, and with the
// panel itself, and the app each step is of, nil for the panel's own.
func panelSteps(w panel, found *dockwellv1.Panel) ([]*dockwellv1.ImportStep, []*dockwellv1.PanelApp) {
	names := map[string]int{}
	for _, a := range found.GetApps() {
		names[a.GetName()]++
	}
	var steps []*dockwellv1.ImportStep
	var apps []*dockwellv1.PanelApp
	for _, a := range found.GetApps() {
		name := a.GetName()
		if names[name] > 1 {
			// Two of one name, in two of the panel's projects, say which they are.
			name = fmt.Sprintf("%s (%s)", name, strings.Trim(a.GetProject()+"/"+a.GetEnvironment(), "/"))
		}
		step := &dockwellv1.ImportStep{Kind: dockwellv1.ImportKind_IMPORT_KIND_APP, Name: name, Summary: summaryOf(w, a), Action: dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON}
		if database(a) {
			step.Kind = dockwellv1.ImportKind_IMPORT_KIND_DATABASE_SERVER
		}
		switch {
		case a.GetPreview():
			leave(step, fmt.Sprintf("It's a pull request's preview, which %s makes and removes with its pull request.", w.name))
		case a.GetSwarm() && a.GetSwarmLeft() != "":
			leave(step, fmt.Sprintf("%s runs it as a Swarm service, and %s, so Dockwell can't make it again here. It stays as it is, in %s.", w.name, a.GetSwarmLeft(), w.name))
		case a.GetSwarm() && a.GetKind() == "stack" && allTasks(a):
			// A stack's services, read from their running tasks, come again in a
			// project of the stack's own, named as the stack named them.
			step.Action = dockwellv1.ImportAction_IMPORT_ACTION_MAKE
			step.Becomes = fmt.Sprintf("its services, %s, made again as they run, on their volumes", human.List(stackServices(a)))
			step.Reason = fmt.Sprintf("%s runs it as a Swarm stack, which Swarm keeps running whatever happens to its containers, so it's made again: Dockwell scales each of its services to none in %s, then starts its copy, with a moment down.", w.name, w.name)
		case a.GetSwarm() && a.GetKind() != "stack" && allTasks(a):
			// One Swarm service, read from its running task, comes again in Dockwell
			// as it runs; the project it comes into is the plan's to name.
			step.Action = dockwellv1.ImportAction_IMPORT_ACTION_MAKE
			step.Becomes = fmt.Sprintf("the service %s, made again as it runs, on its volumes", containers.Label(a.GetId()))
			step.Reason = fmt.Sprintf("%s runs it as a Swarm service, which Swarm keeps running whatever happens to its container, so it's made again: Dockwell scales it to none in %s, then starts its copy, with a moment down.", w.name, w.name)
		case a.GetSwarm():
			leave(step, fmt.Sprintf("%s runs it as a Swarm service, and no task of it runs now, so there's nothing to make it again from. It stays as it is, in %s.", w.name, w.name))
		case a.GetComposeProject() == "":
			leave(step, fmt.Sprintf("Docker runs none of its containers through docker compose, so there's nothing to take on as it runs. It stays as it is, in %s.", w.name))
		default:
			step.Reason = domainsReason(w, a)
		}
		steps = append(steps, step)
		apps = append(apps, a)
	}
	own := make([]string, 0, len(found.GetOwn()))
	for _, c := range found.GetOwn() {
		own = append(own, c.GetName())
	}
	summary := w.name + " itself"
	switch {
	case found.GetVersion() == "":
		summary += ", whose version its own containers don't say"
	case !found.GetKnownVersion():
		summary += fmt.Sprintf(" %s, read as %s %s is, which this import was written from", found.GetVersion(), w.name, w.known)
	default:
		summary += " " + found.GetVersion()
	}
	if len(own) > 0 {
		summary += ": " + human.List(own)
	}
	steps = append(steps, &dockwellv1.ImportStep{
		Kind: dockwellv1.ImportKind_IMPORT_KIND_PANEL, Name: w.name, Summary: summary, Action: dockwellv1.ImportAction_IMPORT_ACTION_LEAVE,
		Reason: "It goes on running beside Dockwell, as nothing of it is changed. Which template each app came from, a Git source, scheduled tasks and backups stay with it, as Dockwell reads neither its database nor its API. Once its apps are taken on, stopping it is a step of its own, and starting it again brings it back as it was.",
	})
	apps = append(apps, nil)
	return steps, apps
}

// database reports an app that's a database: one of Coolify's, or a database
// engine Dokploy runs.
func database(a *dockwellv1.PanelApp) bool {
	switch a.GetKind() {
	case "database", "postgres", "mysql", "mariadb", "mongo", "redis", "libsql":
		return true
	}
	return false
}

// summaryOf says what an app is, in a few words.
func summaryOf(w panel, a *dockwellv1.PanelApp) string {
	var what string
	switch a.GetKind() {
	case "application":
		what = "an application"
	case "service":
		what = fmt.Sprintf("a service from %s's templates", w.name)
	case "database":
		what = "a database"
	case "compose":
		what = "a Compose app"
	case "stack":
		what = "a Compose app run as a Swarm stack"
	default:
		what = "a " + a.GetKind() + " database"
	}
	if img := a.GetImage(); img != "" {
		what += ", running " + img
	}
	if where := strings.Trim(a.GetProject()+" / "+a.GetEnvironment(), " /"); where != "" {
		what += ", in " + where
	}
	if n := len(a.GetContainers()); n > 1 {
		what += fmt.Sprintf(", %d containers", n)
	}
	return what
}

// domainsReason says how an app's domains come: as routes, once the panel's proxy
// hands ports 80 and 443 to Dockwell, a step of its own.
func domainsReason(w panel, a *dockwellv1.PanelApp) string {
	switch d := a.GetDomains(); len(d) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("Its domain, %s, comes as a route once %s hands ports 80 and 443 to Dockwell, a step of its own.", d[0], w.proxy)
	default:
		return fmt.Sprintf("Its domains, %s, come as routes once %s hands ports 80 and 443 to Dockwell, a step of its own.", human.List(d), w.proxy)
	}
}

// credentialFix says how to take on an app whose labels hold a credential, which
// a plan would show: a panel puts a password hash on what it runs where its basic
// auth is on, so turning that off clears it. "" where none does.
func credentialFix(w panel, a *dockwellv1.PanelApp, refusals []string) string {
	var labels []string
	for _, r := range refusals {
		if strings.Contains(r, container.CredentialIn) {
			labels = append(labels, r)
		}
	}
	switch {
	case len(labels) == 0:
		return ""
	case slices.ContainsFunc(labels, func(r string) bool { return basicAuth().MatchString(r) }):
		return fmt.Sprintf("Turn off %s for %s in %s first, then import again.", w.basicAuth, a.GetName(), w.name)
	default:
		return fmt.Sprintf("Take that label off %s in %s first, then import again.", a.GetName(), w.name)
	}
}

// basicAuth is a proxy's basic auth in a label's key, as credentialFix reads one.
var basicAuth = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)(basic|digest)_?auth`) })

// autoDeploy says what to do before taking on an app the panel deploys from Git:
// both panels deploy again on a push by default, which replaces what was taken on
// or starts a second copy beside it. "" for one that isn't.
func autoDeploy(w panel, a *dockwellv1.PanelApp) string {
	if !a.GetFromGit() {
		return ""
	}
	return fmt.Sprintf("Turn off %s for %s in %s before you apply, or a push to its repository can replace it or start a second copy", w.autoDeploy, a.GetName(), w.name)
}

// remakeAutoDeploy says what to do before making again an app the panel deploys
// from Git: its auto deploy would scale its Swarm service back up beside its copy.
func remakeAutoDeploy(w panel, a *dockwellv1.PanelApp) string {
	if !a.GetFromGit() {
		return ""
	}
	return fmt.Sprintf("Turn off %s for %s in %s before you apply, or a push to its repository scales its service back up beside its copy", w.autoDeploy, a.GetName(), w.name)
}

// asRoot says what of an app's reach is as good as root on its server, in a few
// words, such as "With Docker's socket, it's as good as root on web-1", or "".
// The plan says it in full, and asks for a passkey; a step says it first.
func asRoot(edits []*dockwellv1.Edit, server string) string {
	var with []string
	for _, e := range edits {
		reach := container.ServiceAccess(e.GetSpec().GetService().GetApp())
		for i := range reach {
			a := &reach[i]
			if !a.AsGoodAsRoot() {
				continue
			}
			switch a.Kind {
			case resource.AccessDocker:
				with = append(with, "Docker's socket")
			case resource.AccessNetwork, resource.AccessNetworkDriver:
				with = append(with, "the server's own network")
			case resource.AccessPrivileged:
				with = append(with, "privileged mode")
			case resource.AccessPath:
				with = append(with, "the server path "+a.Path)
			case resource.AccessCapability:
				with = append(with, "the capability "+a.Path)
			case resource.AccessGroup:
				with = append(with, "the group "+a.Path)
			default:
				with = append(with, resource.AccessWords(*a))
			}
		}
	}
	if len(with) == 0 {
		return ""
	}
	slices.Sort(with)
	return fmt.Sprintf("With %s, it's as good as root on %s", human.List(slices.Compact(with)), server)
}

// planPanel reads what taking on each app that runs as a Compose project would be,
// and plans the ones nothing stands in the way of, in one change, as plan does for
// 1Panel's. Without propose, each step says what the plan would do, and nothing is
// planned.
func (h *Handler) planPanel(ctx context.Context, srv *dockwellv1.Server, w panel, steps []*dockwellv1.ImportStep, apps []*dockwellv1.PanelApp, propose bool) (*dockwellv1.Change, error) {
	r, err := h.taker.ReadRunning(ctx, srv.GetId())
	if err != nil {
		if connect.CodeOf(err) != connect.CodeUnavailable {
			return nil, err
		}
		for i, s := range steps {
			if apps[i] != nil && s.GetAction() == dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON {
				s.Reason = strings.TrimSpace(fmt.Sprintf("Dockwell can't read what runs on %s now, so nothing is planned yet: %s. Try again once it can. %s",
					srv.GetName(), strings.TrimSuffix(message(err), "."), s.GetReason()))
			}
		}
		return nil, nil
	}
	var takings []*composeapi.Taking
	var taken []string
	claimed := map[string]bool{}
	for i, step := range steps {
		a := apps[i]
		if a == nil || step.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_TAKE_ON {
			continue
		}
		if len(a.GetContainers()) > 0 {
			as, err := h.taker.TakenOn(ctx, r, a.GetContainers()[0])
			if err != nil {
				return nil, err
			}
			if as != "" {
				step.Action, step.Becomes, step.WaitsFor = dockwellv1.ImportAction_IMPORT_ACTION_DONE, "the project "+as+", taken on already", 0
				step.Reason = strings.TrimSpace(fmt.Sprintf("Change it from Dockwell now. %s still lists it: leave it alone there, and don't delete it there, as that deletes what it mounts. %s", w.name, domainsReason(w, a)))
				continue
			}
		}
		want := containers.Label(strings.ToLower(a.GetName()))
		project, err := h.taker.FreeProject(ctx, want, w.from, claimed)
		if err != nil {
			return nil, err
		}
		claimed[project] = true
		t, err := h.taker.ReadTakingIn(ctx, r, a.GetComposeProject(), project, "")
		switch {
		case reason(err) == "not_found":
			leave(step, fmt.Sprintf("%s lists it, but Docker runs no container of its Compose project, %s, so there's nothing to take on.", w.name, a.GetComposeProject()))
			continue
		case err != nil:
			leave(step, sentences([]string{message(err)})+fmt.Sprintf(" It stays as it is, in %s.", w.name))
			continue
		case len(t.Refusals) > 0:
			leave(step, strings.TrimSpace(sentences(actionable(t.Refusals))+fmt.Sprintf(" It stays as it is, in %s, until that's dealt with. ", w.name)+credentialFix(w, a, t.Refusals)))
			continue
		}
		step.Becomes = fmt.Sprintf("the project %s, taken on as it runs, with no restart, its volumes where they are", t.Project)
		if t.Project != want {
			step.Becomes += fmt.Sprintf(", as Dockwell has a project %s already", want)
		}
		step.Reason = sentences([]string{asRoot(t.Edits, srv.GetName()), autoDeploy(w, a), step.GetReason()})
		takings = append(takings, t)
		taken = append(taken, step.GetName())
	}
	// What runs as a Swarm service is made again, the apps of each of the panel's
	// projects in one of Dockwell's, so they find each other by the names they had.
	remakes, parts, made, err := h.planRemakes(ctx, r, w, srv.GetName(), steps, apps, claimed)
	if err != nil {
		return nil, err
	}
	takings = append(takings, remakes...)
	if len(takings) == 0 || !propose {
		return nil, nil
	}
	also := changes.Also{Adopts: map[resource.Ref]changes.Adoption{}}
	var edits []*dockwellv1.Edit
	for _, t := range takings {
		edits = append(edits, t.Edits...)
		also.Edits = append(also.Edits, t.Made...)
		also.Notes = append(also.Notes, t.Notes...)
		maps.Copy(also.Adopts, t.Adopts)
	}
	if len(taken) > 0 {
		also.Notes = append(also.Notes, revision.Note{Reason: "imports_" + w.from, Message: fmt.Sprintf(
			"Takes on what %s runs on %s as it runs: %s. Their volumes stay where they are. Their domains come as routes once %s hands over ports 80 and 443, and %s itself goes on running until you stop it.",
			w.name, srv.GetName(), human.List(taken), w.proxy, w.name)})
	}
	if len(parts) > 0 {
		also.Edits = append(also.Edits, revision.Edit{
			Ref:   resource.Ref{Kind: host.Kind, Scope: srv.GetId(), Name: host.Name},
			Panel: &resource.PanelAsked{Kind: resource.PanelDokploy, Apps: true, Parts: parts},
		})
		var deploys []string
		for i, a := range apps {
			if a != nil && a.GetFromGit() && a.GetSwarm() && steps[i].GetAction() == dockwellv1.ImportAction_IMPORT_ACTION_MAKE {
				deploys = append(deploys, a.GetName())
			}
		}
		if len(deploys) > 0 {
			also.Notes = append(also.Notes, revision.Note{Reason: "remakes_autodeploy_" + w.from, Message: fmt.Sprintf(
				"%s deploys %s on a push to its repository: turn off %s for each in %s before you apply, or a push scales its service back up beside its copy",
				w.name, human.List(deploys), w.autoDeploy, w.name), Names: deploys})
		}
		also.Notes = append(also.Notes, revision.Note{Reason: "remakes_" + w.from, Message: fmt.Sprintf(
			"Makes again in Dockwell what %s runs as Swarm services: %s, as each runs, on the same volumes. Dockwell scales each to none in %s first, and takes it off its ports and networks, so each is down for a moment until its copy starts. Undoing this deletes the copies and puts each back as it was",
			w.name, human.List(made), w.name)})
	}
	return h.changes.Propose(ctx, "Take on what "+w.name+" runs on "+srv.GetName(), edits, nil, also)
}

// PlanStopPanel plans stopping the panel an import came from on a server: its own
// containers and services, never what it ran, and never its proxy, which serves
// the domains of what was taken on until those come as routes. Undoing it starts
// each again as it stood.
func (h *Handler) PlanStopPanel(ctx context.Context, req *connect.Request[dockwellv1.PlanStopPanelRequest]) (*connect.Response[dockwellv1.PlanStopPanelResponse], error) {
	kind, err := changes.PanelKindOf(req.Msg.GetPanel())
	if err != nil {
		return nil, err
	}
	w := panels[req.Msg.GetPanel()]
	srv, err := h.fleet.Get(req.Msg.GetServerId())
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no server %s. List them with ListServers", req.Msg.GetServerId()), "not_found")
	}
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	session, ok := h.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnavailable,
			fmt.Errorf("%s's agent isn't connected, so Dockwell can't read how %s stands there. Try again once it reconnects", srv.GetName(), w.name), "server_offline")
	}
	if !session.StopsPanels() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s's agent is too old to stop %s. Update the agent on %s, then try again", srv.GetName(), w.name, srv.GetName()), "agent_outdated")
	}
	c, err := h.changes.PlanPanelStop(ctx, srv, kind, strings.TrimSpace(req.Msg.GetSummary()))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.PlanStopPanelResponse{Change: c}), nil
}

// PlanStartPanel plans starting the panel an import came from again: the way back
// where no undo is (ADR 0089).
func (h *Handler) PlanStartPanel(ctx context.Context, req *connect.Request[dockwellv1.PlanStartPanelRequest]) (*connect.Response[dockwellv1.PlanStartPanelResponse], error) {
	kind, err := changes.PanelKindOf(req.Msg.GetPanel())
	if err != nil {
		return nil, err
	}
	w := panels[req.Msg.GetPanel()]
	srv, err := h.fleet.Get(req.Msg.GetServerId())
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, authapi.ReasonError(connect.CodeNotFound, fmt.Errorf("there's no server %s. List them with ListServers", req.Msg.GetServerId()), "not_found")
	}
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	session, ok := h.agents.SessionPastMove(ctx, srv.GetId(), agentapi.MoveGrace)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnavailable,
			fmt.Errorf("%s's agent isn't connected, so Dockwell can't read how %s stands there. Try again once it reconnects", srv.GetName(), w.name), "server_offline")
	}
	if !session.StopsPanels() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s's agent is too old to start %s. Update the agent on %s, then try again", srv.GetName(), w.name, srv.GetName()), "agent_outdated")
	}
	c, err := h.changes.PlanPanelStart(ctx, srv, kind, strings.TrimSpace(req.Msg.GetSummary()))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.PlanStartPanelResponse{Change: c}), nil
}

// planRemakes reads what making each Swarm-run app again would plan, the apps of
// each of the panel's projects into one of Dockwell's named for it, and leaves
// what's in the way, saying why. It returns the takings, the Swarm services the
// panel scales to none as their copies start, and the steps' names.
func (h *Handler) planRemakes(ctx context.Context, r *containers.Running, w panel, server string, steps []*dockwellv1.ImportStep, apps []*dockwellv1.PanelApp, claimed map[string]bool) ([]*composeapi.Taking, []resource.PanelPart, []string, error) {
	groups := map[string][]int{}
	var order []string
	for i, step := range steps {
		a := apps[i]
		if a == nil || !a.GetSwarm() || step.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_MAKE {
			continue
		}
		key := cmp.Or(a.GetProject(), a.GetId())
		if a.GetKind() == "stack" {
			// A stack's services reach each other by the names it gave them, which
			// another stack may give too: each comes into a project of its own.
			key = "stack:" + a.GetId()
		}
		if groups[key] == nil {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}
	var takings []*composeapi.Taking
	var parts []resource.PanelPart
	var made []string
	for _, key := range order {
		want := strings.TrimPrefix(key, "stack:")
		if strings.HasPrefix(key, "stack:") {
			want = apps[groups[key][0]].GetName()
		}
		project, err := h.taker.FreeProject(ctx, containers.Label(strings.ToLower(want)), w.from, claimed)
		if err != nil {
			return nil, nil, nil, err
		}
		claimed[project] = true
		var list []composeapi.Remake
		for _, i := range groups[key] {
			list = append(list, remakesOf(apps[i])...)
		}
		t, refused, err := h.taker.ReadMakingAgain(ctx, r, list, project)
		if err != nil {
			return nil, nil, nil, err
		}
		took := false
		for _, i := range groups[key] {
			var why []string
			for _, m := range remakesOf(apps[i]) {
				why = append(why, refused[m.Service]...)
			}
			if len(why) > 0 {
				leave(steps[i], sentences(actionable(why))+fmt.Sprintf(" It stays as it is, in %s, until that's dealt with.", w.name))
				t.Edits = slices.DeleteFunc(t.Edits, func(e *dockwellv1.Edit) bool {
					return slices.ContainsFunc(remakesOf(apps[i]), func(m composeapi.Remake) bool { return e.GetResource().GetName() == m.Service })
				})
				continue
			}
			if apps[i].GetKind() == "stack" {
				steps[i].Becomes = fmt.Sprintf("its services, %s, in the project %s, made again as they run, on their volumes", human.List(stackServices(apps[i])), t.Project)
			} else {
				steps[i].Becomes = fmt.Sprintf("the service %s in the project %s, made again as it runs, on its volumes", containers.Label(apps[i].GetId()), t.Project)
			}
			// As for a take-on: what makes a copy as good as root, and a push that
			// would deploy it again in the panel, here scaling its service back up
			// beside its copy.
			own := slices.DeleteFunc(slices.Clone(t.Edits), func(e *dockwellv1.Edit) bool {
				return !slices.ContainsFunc(remakesOf(apps[i]), func(m composeapi.Remake) bool { return e.GetResource().GetName() == m.Service })
			})
			steps[i].Reason = sentences([]string{asRoot(own, server), remakeAutoDeploy(w, apps[i]), steps[i].GetReason()})
			for _, svc := range apps[i].GetContainers() {
				parts = append(parts, resource.PanelPart{Name: svc})
			}
			made = append(made, steps[i].GetName())
			took = true
		}
		if took && len(t.Edits) > 0 {
			takings = append(takings, t)
		}
	}
	return takings, parts, made, nil
}

// allTasks reports a Swarm-run app each of whose services has a task running, to
// read it from.
func allTasks(a *dockwellv1.PanelApp) bool {
	return len(a.GetTasks()) > 0 && len(a.GetTasks()) == len(a.GetContainers()) && !slices.Contains(a.GetTasks(), "")
}

// stackServices are a stack's services, as the stack named them.
func stackServices(a *dockwellv1.PanelApp) []string {
	out := make([]string, 0, len(a.GetContainers()))
	for _, c := range a.GetContainers() {
		out = append(out, containers.Label(strings.TrimPrefix(c, a.GetId()+"_")))
	}
	return out
}

// remakesOf are the containers a Swarm-run app is made again from, each with the
// name its copy takes and the ports its service published: an application's
// appName, or each of a stack's services by the name the stack gave it.
func remakesOf(a *dockwellv1.PanelApp) []composeapi.Remake {
	names := []string{containers.Label(a.GetId())}
	if a.GetKind() == "stack" {
		names = stackServices(a)
	}
	out := make([]composeapi.Remake, 0, len(names))
	for i, name := range names {
		if i >= len(a.GetTasks()) {
			break
		}
		m := composeapi.Remake{Container: a.GetTasks()[i], Service: name}
		if a.GetKind() == "stack" {
			m.Stack = a.GetId()
		}
		if i < len(a.GetRestarts()) {
			m.Restart = a.GetRestarts()[i]
		}
		for _, p := range a.GetPublished() {
			want := p.GetService()
			if a.GetKind() != "stack" {
				want = a.GetId()
			}
			if containers.Label(want) == name {
				protocol := resourcev1.Protocol_PROTOCOL_TCP
				if p.GetProtocol() == "udp" {
					protocol = resourcev1.Protocol_PROTOCOL_UDP
				}
				m.Ports = append(m.Ports, &resourcev1.PortMapping{HostPort: p.GetPublished(), ContainerPort: p.GetPort(), Protocol: protocol})
			}
		}
		out = append(out, m)
	}
	return out
}
