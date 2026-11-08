// SPDX-License-Identifier: AGPL-3.0-only

// Package resourcepb converts between the resource model and the schema's
// messages, for the API and for the agent stream, so both say a step the
// same way.
package resourcepb

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Ref converts a resource's name.
func Ref(r resource.Ref) *dockwellv1.ResourceRef {
	return &dockwellv1.ResourceRef{Kind: string(r.Kind), Scope: r.Scope, Name: r.Name}
}

// FromRef reads a resource's name.
func FromRef(r *dockwellv1.ResourceRef) resource.Ref {
	return resource.Ref{Kind: resource.Kind(r.GetKind()), Scope: r.GetScope(), Name: r.GetName()}
}

var actions = map[resource.Action]dockwellv1.PlanAction{
	resource.ActionNone:    dockwellv1.PlanAction_PLAN_ACTION_NONE,
	resource.ActionCreate:  dockwellv1.PlanAction_PLAN_ACTION_CREATE,
	resource.ActionUpdate:  dockwellv1.PlanAction_PLAN_ACTION_UPDATE,
	resource.ActionReplace: dockwellv1.PlanAction_PLAN_ACTION_REPLACE,
	resource.ActionDelete:  dockwellv1.PlanAction_PLAN_ACTION_DELETE,
}

// Action converts what a step does.
func Action(a resource.Action) dockwellv1.PlanAction { return actions[a] }

// ActionFrom reads an action of the schema as the model's, if this build knows it.
func ActionFrom(p dockwellv1.PlanAction) (resource.Action, bool) { return from(actions, p) }

// Did is what a step did in the end, as the schema has it: unspecified when it
// hasn't said.
func Did(a *resource.Action) dockwellv1.PlanAction {
	if a == nil {
		return dockwellv1.PlanAction_PLAN_ACTION_UNSPECIFIED
	}
	return actions[*a]
}

// DidFrom reads what a step did in the end, as the schema has it.
func DidFrom(p dockwellv1.PlanAction) *resource.Action {
	if a, ok := from(actions, p); ok {
		return &a
	}
	return nil
}

// from reads an enum value of the schema as the model's, and reports
// whether this build knows it.
func from[M comparable, P comparable](m map[M]P, v P) (M, bool) {
	for k, p := range m {
		if p == v {
			return k, true
		}
	}
	var zero M
	return zero, false
}

var effects = map[resource.Effect]dockwellv1.StepEffect{
	resource.EffectNone:     dockwellv1.StepEffect_STEP_EFFECT_NONE,
	resource.EffectRestart:  dockwellv1.StepEffect_STEP_EFFECT_RESTART,
	resource.EffectRecreate: dockwellv1.StepEffect_STEP_EFFECT_RECREATE,
	resource.EffectRoll:     dockwellv1.StepEffect_STEP_EFFECT_ROLL,
}

var warnings = map[resource.Warning]dockwellv1.PlanWarning{
	resource.WarningMayLockOut: dockwellv1.PlanWarning_PLAN_WARNING_MAY_LOCK_OUT,
}

var ops = map[resource.Op]dockwellv1.FieldOp{
	resource.OpAdded:    dockwellv1.FieldOp_FIELD_OP_ADDED,
	resource.OpRemoved:  dockwellv1.FieldOp_FIELD_OP_REMOVED,
	resource.OpModified: dockwellv1.FieldOp_FIELD_OP_MODIFIED,
}

// Changes converts field changes. A sensitive field's values are never
// carried, whatever the change holds.
func Changes(cs []resource.Change) []*dockwellv1.FieldChange {
	out := make([]*dockwellv1.FieldChange, len(cs))
	for i, c := range cs {
		fc := &dockwellv1.FieldChange{Path: c.Path, Op: ops[c.Op], From: c.From, To: c.To, Sensitive: c.Sensitive}
		if c.Sensitive {
			fc.From, fc.To = "", ""
		}
		out[i] = fc
	}
	return out
}

// FromChanges reads field changes.
func FromChanges(fcs []*dockwellv1.FieldChange) []resource.Change {
	out := make([]resource.Change, len(fcs))
	for i, fc := range fcs {
		c := resource.Change{Path: fc.GetPath(), From: fc.GetFrom(), To: fc.GetTo(), Sensitive: fc.GetSensitive()}
		c.Op, _ = from(ops, fc.GetOp())
		if c.Sensitive {
			c.From, c.To = "", ""
		}
		out[i] = c
	}
	return out
}

// Step converts a step. resourceID names the resource, when known.
func Step(s resource.Step, resourceID string) *dockwellv1.PlanStep {
	p := &dockwellv1.PlanStep{
		ResourceId: resourceID, Resource: Ref(s.Ref), Action: Action(s.Action), Effect: effects[s.Effect],
		DestroysData: s.Destroys, Changes: Changes(s.Changes),
	}
	if s.Downtime > 0 {
		p.Downtime = durationpb.New(s.Downtime)
	}
	if s.UntilHealthy > 0 {
		p.UntilHealthy = durationpb.New(s.UntilHealthy)
	}
	for _, w := range s.Warnings {
		p.Warnings = append(p.Warnings, warnings[w])
	}
	p.KeptPaths, p.Restores, p.HostAccess, p.HealthUnknown = s.Kept, s.Restores, HostAccess(s.HostAccess), s.HealthUnknown
	for _, at := range slices.Sorted(maps.Keys(s.Leads)) {
		p.HostAccess = append(p.HostAccess, &dockwellv1.HostAccess{Kind: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PATH, Path: at, LeadsTo: s.Leads[at], LeadsMoved: slices.Contains(s.LeadsMoved, at)})
	}
	p.Pull, p.ImageGone = s.Pull, s.ImageGone
	if i := s.Ingress; i != nil {
		p.Ingress = &dockwellv1.IngressEffect{Change: ingressChanges[i.Change], Proxy: i.Proxy, Ports: i.Ports, PanelDomain: i.PanelDomain}
	}
	p.DomainCheck = DomainCheck(s.Domain)
	p.ReturningContainers = s.Returning
	p.Unencrypted = s.Unencrypted
	if a := s.Answers; a != nil {
		p.Answers = &dockwellv1.RouteAnswers{Reach: routeReaches[a.Reach], Proxies: a.Proxies, Plain: a.Plain}
	}
	if c := s.ServerPort; c != nil {
		p.ServerPort = &dockwellv1.ServerPortCheck{Port: c.Port, Listening: c.Listening, NothingListening: c.Nothing}
	}
	p.HidesLogs = s.HidesLogs
	p.Rollout, p.Restart = rollout(s.Rollout), s.Restart
	p.Adopts, p.Releases = s.Adopts, s.Releases
	for _, n := range s.SharedNames {
		p.SharedNames = append(p.SharedNames, &dockwellv1.SharedName{Name: n.Name, Network: n.Network, Others: n.Others})
	}
	if e := s.Elsewhere; e != nil {
		p.ManagedElsewhere = &dockwellv1.ManagedElsewhere{Manager: e.Manager, Container: e.Container, Project: e.Project, Folder: e.Folder}
	}
	if g := s.Goes; g != nil {
		p.Goes = &dockwellv1.RouteGoes{Where: g.Where, Domain: g.Domain, Find: visitorsFind[g.Find]}
	}
	if m := s.Moves; m != nil {
		p.Moves = &dockwellv1.RouteMove{Where: m.Where, From: m.From, To: m.To, ToPort: m.ToPort}
	}
	if r := s.Restore; r != nil {
		p.Restore = &dockwellv1.RestorePlan{
			BackupId: r.Backup, Undoes: r.Undoes, Method: BackupMethod(r.Method), Database: r.Database,
			TakenAt: timestamppb.New(r.Taken), SizeBytes: r.Size, Stops: r.Stops, WrittenBy: r.WrittenBy, RunsOn: r.RunsOn,
			Reached: r.Reached,
		}
	}
	if b := s.BackupFirst; b != nil {
		p.BackupFirst = &dockwellv1.BackupFirst{BackupId: b.ID, Method: BackupMethod(b.Method), Database: b.Database, Stops: b.Stops, EstimateBytes: b.Estimate, Unmeasured: b.Unmeasured}
	}
	if b := s.Build; b != nil {
		p.Build = &dockwellv1.BuildPlan{
			Url: b.URL, Ref: b.Ref, Commit: b.Commit, Context: b.Context, Dockerfile: b.Dockerfile, Image: b.Image,
			Builder: Builder(b.Builder), Detection: Detection(b.Detection),
		}
	}
	return p
}

// Builder is what builds an image, as the API has it.
func Builder(b string) resourcev1.ImageBuild_Builder {
	switch b {
	case resource.BuilderDockerfile:
		return resourcev1.ImageBuild_BUILDER_DOCKERFILE
	case resource.BuilderRailpack:
		return resourcev1.ImageBuild_BUILDER_RAILPACK
	}
	return resourcev1.ImageBuild_BUILDER_UNSPECIFIED
}

// BuilderFrom is what builds an image, from the API's.
func BuilderFrom(b resourcev1.ImageBuild_Builder) string {
	switch b {
	case resourcev1.ImageBuild_BUILDER_DOCKERFILE:
		return resource.BuilderDockerfile
	case resourcev1.ImageBuild_BUILDER_RAILPACK:
		return resource.BuilderRailpack
	case resourcev1.ImageBuild_BUILDER_UNSPECIFIED:
	}
	return resource.BuilderAuto
}

// Detection is what reading a commit found, as the API has it.
func Detection(d *resource.BuildDetection) *dockwellv1.BuildDetection {
	if d == nil {
		return nil
	}
	p := &dockwellv1.BuildDetection{
		Builder: Builder(d.Builder), Dockerfile: d.Dockerfile, BaseImage: d.BaseImage, RailpackVersion: d.Railpack,
		Providers: d.Providers, StartCommand: d.Start, Warnings: d.Warnings, Error: d.Error,
		Problem: detectionProblems[d.Problem], ProblemTool: d.ProblemTool,
	}
	for _, pkg := range d.Packages {
		p.Packages = append(p.Packages, &dockwellv1.DetectedPackage{Name: pkg.Name, Requested: pkg.Requested, Resolved: pkg.Resolved, Source: pkg.Source})
	}
	for _, st := range d.Steps {
		p.Steps = append(p.Steps, &dockwellv1.DetectedStep{Name: st.Name, Commands: st.Commands})
	}
	return p
}

// DetectionFrom is what reading a commit found, from the API's.
func DetectionFrom(p *dockwellv1.BuildDetection) *resource.BuildDetection {
	if p == nil {
		return nil
	}
	d := &resource.BuildDetection{
		Builder: BuilderFrom(p.GetBuilder()), Dockerfile: p.GetDockerfile(), BaseImage: p.GetBaseImage(), Railpack: p.GetRailpackVersion(),
		Providers: p.GetProviders(), Start: p.GetStartCommand(), Warnings: p.GetWarnings(), Error: p.GetError(),
		ProblemTool: p.GetProblemTool(),
	}
	d.Problem, _ = from(detectionProblems, p.GetProblem())
	for _, pkg := range p.GetPackages() {
		d.Packages = append(d.Packages, resource.DetectedPackage{Name: pkg.GetName(), Requested: pkg.GetRequested(), Resolved: pkg.GetResolved(), Source: pkg.GetSource()})
	}
	for _, st := range p.GetSteps() {
		d.Steps = append(d.Steps, resource.DetectedStep{Name: st.GetName(), Commands: st.GetCommands()})
	}
	return d
}

// detectionProblems are what stops Railpack building a commit, as the API has them.
var detectionProblems = map[string]dockwellv1.DetectionProblem{
	resource.ProblemNoProvider: dockwellv1.DetectionProblem_DETECTION_PROBLEM_NO_PROVIDER,
	resource.ProblemVersion:    dockwellv1.DetectionProblem_DETECTION_PROBLEM_VERSION,
	resource.ProblemNoStart:    dockwellv1.DetectionProblem_DETECTION_PROBLEM_NO_START,
	resource.ProblemNetwork:    dockwellv1.DetectionProblem_DETECTION_PROBLEM_NETWORK,
	resource.ProblemOther:      dockwellv1.DetectionProblem_DETECTION_PROBLEM_OTHER,
}

// backupMethods are how backups are taken, as the API has them.
var backupMethods = map[string]dockwellv1.BackupMethod{
	resource.BackupDump:        dockwellv1.BackupMethod_BACKUP_METHOD_DUMP,
	resource.BackupCopy:        dockwellv1.BackupMethod_BACKUP_METHOD_COPY,
	resource.BackupCopyRunning: dockwellv1.BackupMethod_BACKUP_METHOD_COPY_RUNNING,
}

// BackupMethod says how a backup was taken, as the API has it.
func BackupMethod(m string) dockwellv1.BackupMethod { return backupMethods[m] }

// BackupMethodFrom reads how a backup was taken from the API's word for it.
func BackupMethodFrom(m dockwellv1.BackupMethod) string {
	method, _ := from(backupMethods, m)
	return method
}

var visitorsFind = map[resource.VisitorsFind]dockwellv1.VisitorsFind{
	resource.VisitorsLoseIt:        dockwellv1.VisitorsFind_VISITORS_FIND_UNSPECIFIED,
	resource.VisitorsRefused:       dockwellv1.VisitorsFind_VISITORS_FIND_REFUSED,
	resource.VisitorsTLSError:      dockwellv1.VisitorsFind_VISITORS_FIND_TLS_ERROR,
	resource.VisitorsNotFound:      dockwellv1.VisitorsFind_VISITORS_FIND_NOT_FOUND,
	resource.VisitorsRest:          dockwellv1.VisitorsFind_VISITORS_FIND_REST,
	resource.VisitorsNothingBehind: dockwellv1.VisitorsFind_VISITORS_FIND_NOTHING_BEHIND,
}

// rollout converts how a step replaces a running service; nil stays nil.
func rollout(r *resource.RolloutPlan) *dockwellv1.RolloutPlan {
	if r == nil {
		return nil
	}
	return &dockwellv1.RolloutPlan{
		Rolling: r.Rolling, Ports: r.Ports, Volumes: r.Volumes, Paths: r.Paths, ImageData: r.ImageData, Chosen: r.Chosen,
		Drain: Duration(r.Drain), HealthyWithin: Duration(r.HealthyWithin), StopGrace: Duration(r.StopGrace), Soak: Duration(r.Soak), NoCheck: r.NoCheck,
		Others: r.Others, CantTell: r.CantTell, EditedRoutes: r.EditedRoutes, EarlyNetworks: r.EarlyNetworks, EarlyNames: r.EarlyNames, HostNetwork: r.HostNetwork,
		Dropped: dropped(r.Dropped), RoutePorts: r.RoutePorts,
	}
}

func dropped(list []resource.DroppedMount) []*dockwellv1.DroppedMount {
	out := make([]*dockwellv1.DroppedMount, 0, len(list))
	for _, m := range list {
		out = append(out, &dockwellv1.DroppedMount{Source: m.Source, Target: m.Target, EmptyVolume: m.EmptyVolume})
	}
	return out
}

func fromDropped(list []*dockwellv1.DroppedMount) []resource.DroppedMount {
	var out []resource.DroppedMount
	for _, m := range list {
		out = append(out, resource.DroppedMount{Source: m.GetSource(), Target: m.GetTarget(), EmptyVolume: m.GetEmptyVolume()})
	}
	return out
}

// fromRollout reads how a step replaces a running service; nil stays nil.
func fromRollout(r *dockwellv1.RolloutPlan) *resource.RolloutPlan {
	if r == nil {
		return nil
	}
	d := func(v *durationpb.Duration) time.Duration {
		if v == nil {
			return 0
		}
		return max(v.AsDuration(), 0)
	}
	return &resource.RolloutPlan{
		Rolling: r.GetRolling(), Ports: r.GetPorts(), Volumes: r.GetVolumes(), Paths: r.GetPaths(), ImageData: r.GetImageData(), Chosen: r.GetChosen(),
		Drain: d(r.GetDrain()), HealthyWithin: d(r.GetHealthyWithin()), StopGrace: d(r.GetStopGrace()), Soak: d(r.GetSoak()), NoCheck: r.GetNoCheck(),
		Others: r.GetOthers(), CantTell: r.GetCantTell(), EditedRoutes: r.GetEditedRoutes(), EarlyNetworks: r.GetEarlyNetworks(), EarlyNames: r.GetEarlyNames(), HostNetwork: r.GetHostNetwork(),
		Dropped: fromDropped(r.GetDropped()), RoutePorts: r.GetRoutePorts(),
	}
}

var ingressChanges = map[resource.IngressChange]dockwellv1.IngressChange{
	resource.IngressStarts:     dockwellv1.IngressChange_INGRESS_CHANGE_STARTS,
	resource.IngressStops:      dockwellv1.IngressChange_INGRESS_CHANGE_STOPS,
	resource.IngressRestarts:   dockwellv1.IngressChange_INGRESS_CHANGE_RESTARTS,
	resource.IngressTakesPorts: dockwellv1.IngressChange_INGRESS_CHANGE_TAKES_PORTS,
	resource.IngressFreesPorts: dockwellv1.IngressChange_INGRESS_CHANGE_FREES_PORTS,
}

var routeReaches = map[resource.RouteReach]dockwellv1.RouteReach{
	resource.ReachThisServer: dockwellv1.RouteReach_ROUTE_REACH_THIS_SERVER,
	resource.ReachProxies:    dockwellv1.RouteReach_ROUTE_REACH_PROXIES,
	resource.ReachAnyone:     dockwellv1.RouteReach_ROUTE_REACH_ANYONE,
}

var domainStates = map[resource.DomainState]dockwellv1.DomainCheckState{
	resource.DomainUnknown:   dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_UNSPECIFIED,
	resource.DomainHere:      dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_POINTS_HERE,
	resource.DomainNoRecord:  dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_NO_RECORD,
	resource.DomainElsewhere: dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_ELSEWHERE,
	resource.DomainCantTell:  dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_CANT_TELL,
}

// DomainCheck converts what DNS says of a domain; nil stays nil.
func DomainCheck(d *resource.DomainCheck) *dockwellv1.DomainCheck {
	if d == nil {
		return nil
	}
	out := &dockwellv1.DomainCheck{Domain: d.Domain, State: domainStates[d.State], Found: d.Found, Here: d.Here, PublicAddresses: d.Public, UnprovenAddresses: d.Unproven, CarrierNat: d.Carrier, Proxied: d.Proxied}
	if !d.ReadAt.IsZero() {
		out.ReadTime = timestamppb.New(d.ReadAt)
	}
	return out
}

var accessKinds = map[resource.AccessKind]dockwellv1.HostAccessKind{
	resource.AccessPath:          dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PATH,
	resource.AccessDocker:        dockwellv1.HostAccessKind_HOST_ACCESS_KIND_DOCKER_SOCKET,
	resource.AccessNetwork:       dockwellv1.HostAccessKind_HOST_ACCESS_KIND_HOST_NETWORK,
	resource.AccessMount:         dockwellv1.HostAccessKind_HOST_ACCESS_KIND_VOLUME_MOUNT,
	resource.AccessNetworkDriver: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_NETWORK_DRIVER,
	resource.AccessDomain:        dockwellv1.HostAccessKind_HOST_ACCESS_KIND_DOMAIN,
	resource.AccessPanelDomain:   dockwellv1.HostAccessKind_HOST_ACCESS_KIND_PANEL_DOMAIN,

	resource.AccessExternalNetwork: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_EXTERNAL_NETWORK,
	resource.AccessExternalVolume:  dockwellv1.HostAccessKind_HOST_ACCESS_KIND_EXTERNAL_VOLUME,
	resource.AccessTakesOn:         dockwellv1.HostAccessKind_HOST_ACCESS_KIND_TAKES_ON,
	resource.AccessCapability:      dockwellv1.HostAccessKind_HOST_ACCESS_KIND_CAPABILITY,
	resource.AccessGroup:           dockwellv1.HostAccessKind_HOST_ACCESS_KIND_GROUP,
	resource.AccessPrivileged:      dockwellv1.HostAccessKind_HOST_ACCESS_KIND_PRIVILEGED,

	resource.AccessServerPort:     dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PORT,
	resource.AccessTrustedProxy:   dockwellv1.HostAccessKind_HOST_ACCESS_KIND_TRUSTED_PROXY,
	resource.AccessPlainForAnyone: dockwellv1.HostAccessKind_HOST_ACCESS_KIND_PLAIN_FOR_ANYONE,
	resource.AccessHTTPSOff:       dockwellv1.HostAccessKind_HOST_ACCESS_KIND_HTTPS_OFF,
	resource.AccessBackupOf:       dockwellv1.HostAccessKind_HOST_ACCESS_KIND_BACKUP_OF,
}

// HostAccess converts what a resource reaches on its server.
func HostAccess(list []resource.Access) []*dockwellv1.HostAccess {
	var out []*dockwellv1.HostAccess
	for i := range list {
		a := &list[i]
		out = append(out, &dockwellv1.HostAccess{
			Kind: accessKinds[a.Kind], Path: a.Path, ReadOnly: a.ReadOnly, Type: a.Type,
			Address: a.Address, Relabel: a.Relabel, Driver: a.Driver, Detail: a.Detail, WrittenBy: a.Writer, SameAsRoot: a.AsGoodAsRoot(), LeadsTo: a.LeadsTo, Holds: a.Holds,
			HoldsFor: a.HoldsFor, HeldIn: a.HeldIn, HeldBy: a.HeldBy,
		})
	}
	return out
}

// UnknownError is a step that says something this build doesn't know,
// such as an action that a newer build added. Read as anything else, the
// step would be skipped or misstated: an unknown action as none, or a
// warning dropped from a plan that needs one.
type UnknownError struct {
	What  string // "action", "effect" or "warning"
	Value int32  // as the schema numbers it
}

func (e *UnknownError) Error() string {
	return fmt.Sprintf("its %s, number %d, is one this build doesn't know", e.What, e.Value)
}

// FromStep reads a step. A step with an action, effect or warning this
// build doesn't know is an *UnknownError.
func FromStep(p *dockwellv1.PlanStep) (resource.Step, error) {
	s := resource.Step{
		Ref: FromRef(p.GetResource()), Changes: FromChanges(p.GetChanges()),
		Decision: resource.Decision{Destroys: p.GetDestroysData()},
	}
	var ok bool
	if s.Action, ok = from(actions, p.GetAction()); !ok {
		return resource.Step{}, &UnknownError{What: "action", Value: int32(p.GetAction())}
	}
	if s.Effect, ok = from(effects, p.GetEffect()); !ok {
		return resource.Step{}, &UnknownError{What: "effect", Value: int32(p.GetEffect())}
	}
	if d := p.GetDowntime(); d != nil {
		s.Downtime = max(d.AsDuration(), 0)
	}
	if d := p.GetUntilHealthy(); d != nil {
		s.UntilHealthy = max(d.AsDuration(), 0)
	}
	// An agent's word on host access counts for nothing: the control plane
	// decides it from the specs. All it takes from one is where a path leads,
	// which only an agent can read, and which can only make a plan louder.
	for _, a := range p.GetHostAccess() {
		if a.GetLeadsTo() != "" {
			if s.Leads == nil {
				s.Leads = map[string]string{}
			}
			s.Leads[a.GetPath()] = a.GetLeadsTo()
			if a.GetLeadsMoved() {
				s.LeadsMoved = append(s.LeadsMoved, a.GetPath())
			}
		}
	}
	s.Kept, s.Restores, s.HealthUnknown, s.HidesLogs = p.GetKeptPaths(), p.GetRestores(), p.GetHealthUnknown(), p.GetHidesLogs()
	s.Pull, s.ImageGone = p.GetPull(), p.GetImageGone()
	s.Adopts, s.Releases = p.GetAdopts(), p.GetReleases()
	for _, n := range p.GetSharedNames() {
		s.SharedNames = append(s.SharedNames, resource.SharedName{Name: n.GetName(), Network: n.GetNetwork(), Others: n.GetOthers()})
	}
	if e := p.GetManagedElsewhere(); e != nil {
		s.Elsewhere = &resource.Elsewhere{Manager: e.GetManager(), Container: e.GetContainer(), Project: e.GetProject(), Folder: e.GetFolder()}
	}
	for _, w := range p.GetWarnings() {
		k, ok := from(warnings, w)
		if !ok {
			return resource.Step{}, &UnknownError{What: "warning", Value: int32(w)}
		}
		s.Warnings = append(s.Warnings, k)
	}
	if i := p.GetIngress(); i != nil {
		change, ok := from(ingressChanges, i.GetChange())
		if !ok {
			return resource.Step{}, &UnknownError{What: "ingress change", Value: int32(i.GetChange())}
		}
		s.Ingress = &resource.IngressEffect{Change: change, Proxy: i.GetProxy(), Ports: i.GetPorts(), PanelDomain: i.GetPanelDomain()}
	}
	// A check this build doesn't know reads as not known: it never stops a
	// step, so nothing is misstated by it.
	if d := p.GetDomainCheck(); d != nil {
		state, _ := from(domainStates, d.GetState())
		s.Domain = &resource.DomainCheck{Domain: d.GetDomain(), State: state, Found: d.GetFound(), Here: d.GetHere(), Public: d.GetPublicAddresses(), Unproven: d.GetUnprovenAddresses(), Carrier: d.GetCarrierNat(), Proxied: d.GetProxied()}
		if d.GetReadTime() != nil {
			s.Domain.ReadAt = d.GetReadTime().AsTime()
		}
	}
	s.Returning = p.GetReturningContainers()
	s.Unencrypted = p.GetUnencrypted()
	// Who it answers only informs: the control plane has the spec. One this build
	// doesn't know reads as not known.
	if a := p.GetAnswers(); a != nil {
		reach, _ := from(routeReaches, a.GetReach())
		s.Answers = &resource.RouteAnswers{Reach: reach, Proxies: a.GetProxies(), Plain: a.GetPlain()}
	}
	if c := p.GetServerPort(); c != nil {
		s.ServerPort = &resource.ServerPortCheck{Port: c.GetPort(), Listening: c.GetListening(), Nothing: c.GetNothingListening()}
	}
	// What visitors find only informs: one this build doesn't know reads as not known.
	if g := p.GetGoes(); g != nil {
		find, _ := from(visitorsFind, g.GetFind())
		s.Goes = &resource.RouteGoes{Where: g.GetWhere(), Domain: g.GetDomain(), Find: find}
	}
	s.Rollout, s.Restart = fromRollout(p.GetRollout()), p.GetRestart()
	if m := p.GetMoves(); m != nil {
		s.Moves = &resource.RouteMove{Where: m.GetWhere(), From: m.GetFrom(), To: m.GetTo(), ToPort: m.GetToPort()}
	}
	if r := p.GetRestore(); r != nil {
		s.Restore = &resource.RestorePlan{
			Backup: r.GetBackupId(), Undoes: r.GetUndoes(), Method: BackupMethodFrom(r.GetMethod()), Database: r.GetDatabase(),
			Taken: r.GetTakenAt().AsTime(), Size: r.GetSizeBytes(), Stops: r.GetStops(), WrittenBy: r.GetWrittenBy(), RunsOn: r.GetRunsOn(),
			Reached: r.GetReached(),
		}
	}
	if b := p.GetBackupFirst(); b != nil {
		s.BackupFirst = &resource.BackupFirst{ID: b.GetBackupId(), Method: BackupMethodFrom(b.GetMethod()), Database: b.GetDatabase(), Stops: b.GetStops(), Estimate: b.GetEstimateBytes(), Unmeasured: b.GetUnmeasured()}
	}
	if b := p.GetBuild(); b != nil {
		s.Build = &resource.BuildPlan{
			URL: b.GetUrl(), Ref: b.GetRef(), Commit: b.GetCommit(), Context: b.GetContext(), Dockerfile: b.GetDockerfile(), Image: b.GetImage(),
			Builder: BuilderFrom(b.GetBuilder()), Detection: DetectionFrom(b.GetDetection()),
		}
	}
	return s, nil
}

// Duration converts a duration, unset when zero.
func Duration(d time.Duration) *durationpb.Duration {
	if d <= 0 {
		return nil
	}
	return durationpb.New(d)
}
