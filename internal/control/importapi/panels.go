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
	"github.com/dockwell/dockwell/internal/resource/route"
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
		case a.GetSwarm() && a.GetKind() == "stack" && allTasks(a):
			// A stack's services, read from their running tasks, come again in a
			// project of the stack's own, named as the stack named them.
			step.Action = dockwellv1.ImportAction_IMPORT_ACTION_MAKE
			step.Becomes = fmt.Sprintf("its services, %s, made again as they run, on their volumes", human.List(stackServices(a)))
			step.Reason = fmt.Sprintf("%s runs it as a Swarm stack, which Swarm keeps running whatever happens to its containers, so it's made again: %s scales each service to none as Dockwell's copy starts, with a moment down.", w.name, w.name)
		case a.GetSwarm() && a.GetKind() != "stack" && allTasks(a):
			// One Swarm service, read from its running task, comes again in Dockwell
			// as it runs; the project it comes into is the plan's to name.
			step.Action = dockwellv1.ImportAction_IMPORT_ACTION_MAKE
			step.Becomes = fmt.Sprintf("the service %s, made again as it runs, on its volumes", containers.Label(a.GetId()))
			step.Reason = fmt.Sprintf("%s runs it as a Swarm service, which Swarm keeps running whatever happens to its container, so it's made again: %s scales it to none as Dockwell's copy starts, with a moment down.", w.name, w.name)
		case a.GetSwarm():
			leave(step, fmt.Sprintf("%s runs it as a Swarm service, and no task of it runs now, so there's nothing to read it from. Start it in %s, then import again.", w.name, w.name))
		case a.GetComposeProject() == "":
			leave(step, fmt.Sprintf("Docker runs none of its containers through docker compose, so there's nothing to take on as it runs. It stays as it is, in %s.", w.name))
		default:
			step.Reason = domainsReason(w, a)
		}
		steps = append(steps, step)
		apps = append(apps, a)
	}
	// Each domain its proxy serves comes as a route once the proxy hands over ports
	// 80 and 443, a step of its own (PlanPanelDomains), or says why it stops
	// answering then: every domain the proxy serves is down once it stops.
	for _, a := range found.GetApps() {
		for _, rt := range a.GetRoutes() {
			step := &dockwellv1.ImportStep{
				Kind: dockwellv1.ImportKind_IMPORT_KIND_DOMAIN, Name: rt.GetDomain() + rt.GetPath(),
				Summary: fmt.Sprintf("a domain of %s, sent to %s", a.GetName(), rt.GetContainer()),
				Action:  dockwellv1.ImportAction_IMPORT_ACTION_MAKE, WaitsFor: dockwellv1.ImportWaitsFor_IMPORT_WAITS_FOR_PORTS,
			}
			if why := routeLeft(found, a, rt); why != "" {
				leave(step, fmt.Sprintf("It doesn't come as a route, as %s, so it stops answering once %s hands over ports 80 and 443.", why, w.proxy))
			} else {
				step.Becomes = fmt.Sprintf("a route to %s on port %d", rt.GetService(), rt.GetPort())
				step.Reason = fmt.Sprintf("It comes once %s hands over ports 80 and 443, a step of its own, after %s is taken on.", w.proxy, a.GetName())
			}
			steps = append(steps, step)
			apps = append(apps, nil)
		}
		for _, d := range unrouted(a) {
			step := &dockwellv1.ImportStep{Kind: dockwellv1.ImportKind_IMPORT_KIND_DOMAIN, Name: d, Summary: "a domain of " + a.GetName()}
			why := cmp.Or(routeLeft(found, a, &dockwellv1.PanelRoute{Port: 1}), fmt.Sprintf("Dockwell can't read where %s sends it", w.proxy))
			leave(step, fmt.Sprintf("It doesn't come as a route, as %s, so it stops answering once %s hands over ports 80 and 443.", why, w.proxy))
			steps = append(steps, step)
			apps = append(apps, nil)
		}
	}
	for _, d := range found.GetDomains() {
		step := &dockwellv1.ImportStep{Kind: dockwellv1.ImportKind_IMPORT_KIND_DOMAIN, Name: d, Summary: fmt.Sprintf("a domain %s routes from its own files", w.proxy)}
		leave(step, fmt.Sprintf("Its route is %s's own, such as its dashboard's, which doesn't come over, so it stops answering once %s hands over ports 80 and 443.", w.name, w.proxy))
		steps = append(steps, step)
		apps = append(apps, nil)
	}
	holdDomains(steps, w)
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
	remakes, parts, made, err := h.planRemakes(ctx, r, w, steps, apps, claimed)
	if err != nil {
		return nil, err
	}
	takings = append(takings, remakes...)
	if err := h.domainsDone(ctx, r, steps, apps); err != nil {
		return nil, err
	}
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
			Spec:  &resourcev1.ServerHost{},
			Panel: &resource.PanelAsked{Kind: resource.PanelDokploy, Apps: true, Parts: parts},
		})
		also.Notes = append(also.Notes, revision.Note{Reason: "remakes_" + w.from, Message: fmt.Sprintf(
			"Makes again in Dockwell what %s runs as Swarm services: %s, as each runs, on the same volumes. %s scales each to none first, so each is down for a moment until Dockwell's copy starts. Undoing this deletes Dockwell's copies and scales each back",
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

// PlanPanelDomains plans handing over the domains of what was taken on from a
// panel: one change that stops the panel's proxy first, then makes a route to each
// taken-on app's service for each of its domains, so Dockwell's ingress takes
// ports 80 and 443 (ADR 0089). Undoing it deletes the routes, then starts the proxy
// again.
func (h *Handler) PlanPanelDomains(ctx context.Context, req *connect.Request[dockwellv1.PlanPanelDomainsRequest]) (*connect.Response[dockwellv1.PlanPanelDomainsResponse], error) {
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
			fmt.Errorf("%s's agent isn't connected, so Dockwell can't read what %s runs there. Try again once it reconnects", srv.GetName(), w.name), "server_offline")
	}
	if !session.ReadsPanels() || !session.StopsPanels() || !session.ReadsPanelsStrictly() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s's agent is too old to hand over %s's domains, as it doesn't read what could guard them in full. Update the agent on %s, then try again", srv.GetName(), w.name, srv.GetName()), "agent_outdated")
	}
	out, err := session.ReadPanel(ctx, req.Msg.GetPanel())
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if out.GetFound() == nil {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s doesn't run %s: %s. Run this for a server where %s runs", srv.GetName(), w.name, cut(out.GetNotHere(), w.name+" doesn't run here: "), w.name), "panel_not_found")
	}
	r, err := h.taker.ReadRunning(ctx, srv.GetId())
	if err != nil {
		return nil, err
	}
	routes, err := h.panelRoutes(ctx, r, out.GetFound())
	if err != nil {
		return nil, err
	}
	var edits []*dockwellv1.Edit
	var came, stays []string
	waiting := false
	for _, pr := range routes {
		switch {
		case pr.done:
		case pr.left != "":
			stays = append(stays, pr.domain)
			waiting = waiting || pr.waits
		default:
			edits = append(edits, &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: "route", Scope: pr.environment, Name: pr.name},
				Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: pr.route}},
			})
			came = append(came, pr.domain)
		}
	}
	if len(edits) == 0 {
		var why error
		reason := "nothing_to_route"
		switch {
		case waiting:
			why, reason = fmt.Errorf("no domain of %s's would come as a route yet, as the apps they go to aren't taken on. Take them on first, then hand over its domains", w.name), "not_taken_on"
		case len(stays) == 0:
			why, reason = fmt.Errorf("%s's domains come as routes on %s already, so there's nothing to hand over", w.name, srv.GetName()), changes.ReasonAlready
		default:
			why = fmt.Errorf("none of %s's domains can come as a route, so handing over its ports would only take %s down. Each says why in the import", w.name, human.List(stays))
		}
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, why, reason)
	}
	message := fmt.Sprintf("Stops %s, which holds ports 80 and 443 for %s, so Dockwell's ingress takes them and serves %s, each with its own certificate. Each is down from the stop until its certificate comes, a minute or so. Undoing this deletes these routes and starts %s again",
		w.proxy, w.name, human.List(came), w.proxy)
	if len(stays) > 0 {
		message += fmt.Sprintf(". These stop answering, as nothing in Dockwell serves them: %s. The import says why for each", human.List(stays))
	}
	summary := cmp.Or(strings.TrimSpace(req.Msg.GetSummary()), fmt.Sprintf("Hand over %s's domains on %s", w.name, srv.GetName()))
	c, err := h.changes.Propose(ctx, summary, edits, nil, changes.Also{
		Edits: []revision.Edit{{
			Ref:   resource.Ref{Kind: host.Kind, Scope: srv.GetId(), Name: host.Name},
			Spec:  &resourcev1.ServerHost{},
			Panel: &resource.PanelAsked{Kind: kind, Proxy: true},
		}},
		Notes: []revision.Note{
			{Reason: "panel_domains", Message: message},
			// Dockwell read what it reads and found nothing; it can't promise there's
			// nothing else, so the person confirms with that in view.
			{Reason: "panel_domains_unguarded", Message: "No login or address limit found on the old proxy: " + human.List(came), Names: came},
		},
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.PlanPanelDomainsResponse{Change: c}), nil
}

// panelRoute is a domain the panel's proxy serves, and where it goes in Dockwell.
type panelRoute struct {
	domain string
	// environment is where the app it goes to was taken on, "" for one that wasn't.
	environment string
	// name is the route's name there, and route its spec.
	name  string
	route *resourcev1.Route
	// done says the environment routes it already.
	done bool
	// left says why it doesn't come as a route, and waits that its app isn't taken
	// on yet, which would bring it.
	left  string
	waits bool
	// host is its domain without its path.
	host string
}

// panelRoutes reads where each domain the panel's proxy serves goes in Dockwell:
// to the service of the taken-on container its labels send it to, under a name no
// other route of that environment has.
func (h *Handler) panelRoutes(ctx context.Context, r *containers.Running, found *dockwellv1.Panel) ([]panelRoute, error) {
	var out []panelRoute
	have := map[string]map[string]*resourcev1.Route{}
	for _, a := range found.GetApps() {
		for _, rt := range a.GetRoutes() {
			pr := panelRoute{domain: rt.GetDomain() + rt.GetPath(), host: rt.GetDomain()}
			if pr.left = routeLeft(found, a, rt); pr.left != "" {
				out = append(out, pr)
				continue
			}
			runs := rt.GetContainer()
			if a.GetSwarm() {
				// Made again, it runs as Dockwell's container of its appName's service.
				runs = remadeAs(r, containers.Label(rt.GetService()))
			}
			_, env, svc, err := h.taker.TakenOnAs(ctx, r, runs)
			if err != nil {
				return nil, err
			}
			if env == "" || svc == "" {
				pr.left, pr.waits = a.GetName()+" isn't taken on yet", true
				out = append(out, pr)
				continue
			}
			if have[env] == nil {
				if have[env], err = h.taker.RoutesIn(ctx, env); err != nil {
					return nil, err
				}
			}
			pr.environment = env
			pr.route = &resourcev1.Route{Domain: rt.GetDomain(), Path: rt.GetPath(), Service: svc, Port: rt.GetPort()}
			for _, existing := range have[env] {
				pr.done = pr.done || existing.GetDomain() == rt.GetDomain() && existing.GetPath() == rt.GetPath()
			}
			if !pr.done {
				pr.name = routeName(pr.domain, have[env])
				have[env][pr.name] = pr.route
			}
			out = append(out, pr)
		}
		for _, d := range unrouted(a) {
			out = append(out, panelRoute{domain: d, host: d, left: "Dockwell can't read where it goes"})
		}
	}
	for _, d := range found.GetDomains() {
		out = append(out, panelRoute{domain: d, host: d, left: "the panel's own route"})
	}
	// A path of a domain that stays behind keeps the rest of it behind too: a route
	// for the domain's root would serve that path, and what guarded it there.
	for i := range out {
		if out[i].left == "" {
			continue
		}
		for j := range out {
			if out[j].host == out[i].host && out[j].left == "" && !out[j].done {
				out[j].left = "another path of the domain stays behind"
				out[j].environment = ""
			}
		}
	}
	return out, nil
}

// routeName is what to call a route to a domain, as a route's name is a DNS label:
// the domain's letters, digits and dashes, starting with a letter, and a number
// after it where the environment has a route of that name already.
func routeName(domain string, have map[string]*resourcev1.Route) string {
	label := strings.Trim(nonLabel().ReplaceAllString(strings.ToLower(domain), "-"), "-")
	label = strings.TrimLeft(label, "0123456789-")
	if label == "" {
		label = "route"
	}
	const most = 63
	base := strings.TrimRight(label[:min(len(label), most)], "-")
	if _, taken := have[base]; !taken {
		return base
	}
	for i := 2; ; i++ {
		suffix := fmt.Sprintf("-%d", i)
		name := strings.TrimRight(base[:min(len(base), most-len(suffix))], "-") + suffix
		if _, taken := have[name]; !taken {
			return name
		}
	}
}

// nonLabel is what a DNS label can't hold.
var nonLabel = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`[^a-z0-9-]+`) })

// domainsDone marks each domain step done whose domain a route serves already, in
// the environment its app was taken on into: its domains came over.
func (h *Handler) domainsDone(ctx context.Context, r *containers.Running, steps []*dockwellv1.ImportStep, apps []*dockwellv1.PanelApp) error {
	found := &dockwellv1.Panel{Apps: slices.DeleteFunc(slices.Clone(apps), func(a *dockwellv1.PanelApp) bool { return a == nil })}
	routes, err := h.panelRoutes(ctx, r, found)
	if err != nil {
		return err
	}
	for _, pr := range routes {
		if !pr.done {
			continue
		}
		for _, s := range steps {
			if s.GetKind() == dockwellv1.ImportKind_IMPORT_KIND_DOMAIN && s.GetName() == pr.domain {
				s.Action, s.Becomes, s.Reason, s.WaitsFor = dockwellv1.ImportAction_IMPORT_ACTION_DONE, "a route, which Dockwell serves", "", 0
			}
		}
	}
	return nil
}

// routeLeft says why a domain the proxy sends to an app doesn't come as a route,
// or "" where it does once its app is taken on.
func routeLeft(found *dockwellv1.Panel, a *dockwellv1.PanelApp, rt *dockwellv1.PanelRoute) string {
	switch {
	case found.GetProxyLeft() != "":
		// What guards every domain at once couldn't be read in full, or guards one.
		return found.GetProxyLeft()
	case slices.Contains(found.GetDomains(), rt.GetDomain()):
		return "the proxy's own files route it too, which can guard a path of it, and they don't come over"
	case a.GetPreview():
		return "it's a pull request's preview"
	case !a.GetSwarm() && a.GetComposeProject() == "" || a.GetSwarm() && !allTasks(a):
		return a.GetName() + " stays where it is"
	case rt.GetLeft() != "":
		return rt.GetLeft() + ". Add it from " + a.GetName() + "'s page once it's taken on"
	case rt.GetPort() == 0:
		return "the proxy's labels don't say which port it goes to. Add it from " + a.GetName() + "'s page once it's taken on"
	}
	if err := route.CheckPath(rt.GetPath()); rt.GetPath() != "" && err != nil {
		return "its path can't be a route's: " + strings.TrimSuffix(err.Error(), ".")
	}
	return ""
}

// unrouted are an app's domains the proxy's labels or files give no route for.
func unrouted(a *dockwellv1.PanelApp) []string {
	var out []string
	for _, d := range a.GetDomains() {
		if !slices.ContainsFunc(a.GetRoutes(), func(rt *dockwellv1.PanelRoute) bool { return rt.GetDomain() == d }) && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// PlanStartPanel plans starting the panel an import came from again, or its proxy
// alone: the way back where no undo is (ADR 0089).
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
	c, err := h.changes.PlanPanelStart(ctx, srv, kind, req.Msg.GetProxy(), strings.TrimSpace(req.Msg.GetSummary()))
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.PlanStartPanelResponse{Change: c}), nil
}

// planRemakes reads what making each Swarm-run app again would plan, the apps of
// each of the panel's projects into one of Dockwell's named for it, and leaves
// what's in the way, saying why. It returns the takings, the Swarm services the
// panel scales to none as their copies start, and the steps' names.
func (h *Handler) planRemakes(ctx context.Context, r *containers.Running, w panel, steps []*dockwellv1.ImportStep, apps []*dockwellv1.PanelApp, claimed map[string]bool) ([]*composeapi.Taking, []resource.PanelPart, []string, error) {
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

// remadeAs is the container Dockwell runs for a service of that name, which a
// Swarm-run app made again runs as, or "".
func remadeAs(r *containers.Running, service string) string {
	for _, c := range r.All {
		if c.GetComposeService() == service && r.RunsAs(c.GetName()) != "" {
			return c.GetName()
		}
	}
	return ""
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

// holdDomains keeps every path of a domain behind where one stays, in the
// report as in the plan (panelRoutes): a route for the domain's root would serve
// the path that stays, and whatever guarded it.
func holdDomains(steps []*dockwellv1.ImportStep, w panel) {
	domain := func(s *dockwellv1.ImportStep) string { h, _, _ := strings.Cut(s.GetName(), "/"); return h }
	for _, s := range steps {
		if s.GetKind() != dockwellv1.ImportKind_IMPORT_KIND_DOMAIN || s.GetAction() != dockwellv1.ImportAction_IMPORT_ACTION_LEAVE {
			continue
		}
		for _, o := range steps {
			if o.GetKind() == dockwellv1.ImportKind_IMPORT_KIND_DOMAIN && o.GetAction() == dockwellv1.ImportAction_IMPORT_ACTION_MAKE && domain(o) == domain(s) {
				leave(o, fmt.Sprintf("It doesn't come as a route, as another path of the domain stays behind, so it stops answering once %s hands over ports 80 and 443.", w.proxy))
			}
		}
	}
}
