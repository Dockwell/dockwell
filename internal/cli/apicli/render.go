// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/human"
	"github.com/dockwell/dockwell/internal/resource"
)

// JSON writes msg as the API's JSON, protojson as REST answers it: field
// names in lowerCamelCase, enums by name, unset fields left out. Indented,
// it suits one message; lines puts each message on its own line, for a
// command that prints several, as JSON Lines. Whitespace is normalized,
// as protojson's own varies on purpose.
func JSON(w io.Writer, msg proto.Message, indented bool) error {
	raw, err := protojson.Marshal(msg)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if indented {
		err = json.Indent(&buf, raw, "", "  ")
	} else {
		err = json.Compact(&buf, raw)
	}
	if err != nil {
		return err
	}
	buf.WriteByte('\n')
	_, err = w.Write(buf.Bytes())
	return err
}

// Render writes msg for people.
func Render(w io.Writer, msg proto.Message, now time.Time) {
	switch m := msg.(type) {
	case *dockwellv1.ListServersResponse:
		serversTable(w, m.GetServers(), now)
	case *dockwellv1.GetServerResponse:
		serverDetail(w, m.GetServer(), now)
	case *dockwellv1.WatchServersResponse:
		serverEvent(w, m, now)
	case *dockwellv1.ListChangesResponse:
		changesTable(w, m, now)
	case *dockwellv1.ListDriftResponse:
		driftTable(w, m)
	case *dockwellv1.ListLogSourcesResponse:
		sourcesTable(w, m, now)
	case *dockwellv1.ExportComposeResponse:
		// The file alone, so it pipes into docker compose -f -. Its comments
		// say which variables it leaves out.
		_, _ = io.WriteString(w, m.GetCompose())
	default:
		if c := changeIn(msg); c != nil {
			ChangeView(w, c, now)
			return
		}
		generic(w, msg.ProtoReflect(), now)
	}
}

// changeIn finds the change a response carries, if it carries one.
func changeIn(msg proto.Message) *dockwellv1.Change {
	m := msg.ProtoReflect()
	fields := m.Descriptor().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		if fd.Message() != nil && !fd.IsList() && fd.Message().FullName() == "dockwell.v1.Change" && m.Has(fd) {
			c, _ := m.Get(fd).Message().Interface().(*dockwellv1.Change)
			return c
		}
	}
	return nil
}

func newTable(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

func row(tw io.Writer, cells ...string) { fmt.Fprintln(tw, strings.Join(cells, "\t")) }

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func when(t *timestamppb.Timestamp, now time.Time) string {
	if t == nil {
		return ""
	}
	if at := t.AsTime(); at.After(now.Add(time.Second)) {
		return In(at, now)
	}
	return Ago(t.AsTime(), now)
}

// enumWord writes an enum value as a word: CONNECTION_CONNECTED is
// "Connected", TOKEN_SCOPE_READ_ONLY "Read only", and a value with a word of its
// own that, as HEALTH_STATE_NO_CHECK is "Running".
func enumWord(ed protoreflect.EnumDescriptor, n protoreflect.EnumNumber) string {
	v := ed.Values().ByNumber(n)
	if v == nil {
		return strconv.Itoa(int(n))
	}
	if n == 0 {
		return ""
	}
	if w, _ := proto.GetExtension(v.Options(), dockwellv1.E_Word).(string); w != "" {
		return w
	}
	return capitalize(strings.ReplaceAll(enumName(ed, v), "-", " "))
}

func word(e interface {
	Descriptor() protoreflect.EnumDescriptor
	Number() protoreflect.EnumNumber
},
) string {
	return enumWord(e.Descriptor(), e.Number())
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func serversTable(w io.Writer, servers []*dockwellv1.Server, now time.Time) {
	if len(servers) == 0 {
		fmt.Fprintln(w, "No servers yet. Make a join ticket with dockwell token create on the control server, then run dockwell join on the server you're adding.")
		return
	}
	tw := newTable(w)
	row(tw, "Name", "ID", "Connection", "OS", "CPU", "Memory", "Last seen")
	for _, s := range servers {
		name := s.GetName()
		if s.GetControlServer() {
			name += " (control)"
		}
		cpu, memory := "-", "-"
		if st := s.GetStats(); st != nil {
			cpu = Percent(st.GetCpuPercent())
			memory = BytesOf(st.GetMemoryUsedBytes(), st.GetMemoryTotalBytes())
		}
		seen := "never"
		if s.GetLastSeenTime() != nil {
			seen = when(s.GetLastSeenTime(), now)
		}
		row(tw, name, s.GetId(), word(s.GetConnection()), orDash(s.GetInventory().GetOsName()), cpu, memory, seen)
	}
	_ = tw.Flush()
}

func serverDetail(w io.Writer, s *dockwellv1.Server, now time.Time) {
	fmt.Fprintf(w, "%s (%s)\n", s.GetName(), s.GetId())
	tw := newTable(w)
	connection := word(s.GetConnection())
	if s.GetLastSeenTime() != nil {
		connection += ", last seen " + when(s.GetLastSeenTime(), now)
	}
	row(tw, "Connection", connection)
	if s.GetControlServer() {
		row(tw, "Control server", "Yes, it runs the control plane")
	}
	inv := s.GetInventory()
	if inv != nil {
		os := inv.GetOsName()
		if inv.GetKernel() != "" {
			os += ", kernel " + inv.GetKernel()
		}
		if inv.GetArchitecture() != "" {
			os += ", " + inv.GetArchitecture()
		}
		row(tw, "OS", os)
		cpu := inv.GetCpuModel()
		if inv.GetCpuCount() > 0 {
			cpu = strings.TrimPrefix(cpu+fmt.Sprintf(", %d cores", inv.GetCpuCount()), ", ")
		}
		if st := s.GetStats(); st != nil {
			cpu += ", " + Percent(st.GetCpuPercent()) + " busy"
		}
		row(tw, "CPU", cpu)
	}
	if st := s.GetStats(); st != nil {
		row(tw, "Memory", BytesOf(st.GetMemoryUsedBytes(), st.GetMemoryTotalBytes()))
		row(tw, "Load", fmt.Sprintf("%.2f, %.2f, %.2f", st.GetLoad1(), st.GetLoad5(), st.GetLoad15()))
		for _, d := range st.GetDisks() {
			row(tw, "Disk "+d.GetMountPoint(), BytesOf(d.GetUsedBytes(), d.GetSizeBytes()))
		}
		if st.GetUptime() != nil {
			row(tw, "Up for", Span(st.GetUptime().AsDuration()))
		}
	} else if inv != nil {
		row(tw, "Memory", Bytes(inv.GetMemoryBytes()))
	}
	if inv != nil {
		docker := inv.GetDockerVersion()
		if docker == "" {
			docker = "Not running"
		}
		row(tw, "Docker", docker)
		if len(inv.GetAddresses()) > 0 {
			row(tw, "Addresses", strings.Join(inv.GetAddresses(), ", "))
		}
	}
	if b := s.GetAgentBuild(); b != nil {
		row(tw, "Agent", b.GetVersion())
	}
	if s.GetCreateTime() != nil {
		row(tw, "Joined", when(s.GetCreateTime(), now))
	}
	_ = tw.Flush()
}

func serverEvent(w io.Writer, e *dockwellv1.WatchServersResponse, now time.Time) {
	switch {
	case e.GetHeartbeat():
		// It says only that the watch is alive, which a terminal shows by going on.
	case e.GetSnapshotComplete():
		fmt.Fprintln(w, "Watching for changes. Press Ctrl+C to stop.")
	case e.GetRemoved():
		fmt.Fprintf(w, "%s was removed.\n", e.GetServer().GetName())
	default:
		s := e.GetServer()
		line := fmt.Sprintf("%s  %s", s.GetName(), word(s.GetConnection()))
		if st := s.GetStats(); st != nil {
			line += fmt.Sprintf("  CPU %s  memory %s", Percent(st.GetCpuPercent()), BytesOf(st.GetMemoryUsedBytes(), st.GetMemoryTotalBytes()))
		}
		if s.GetLastSeenTime() != nil {
			line += "  " + when(s.GetLastSeenTime(), now)
		}
		fmt.Fprintln(w, line)
	}
}

// Counts writes a plan's summary as the panel does: "1 to create · 0 to
// change · 0 to delete", with replacements apart, only when there are
// some.
func Counts(s *dockwellv1.PlanSummary) string {
	parts := []string{fmt.Sprintf("%d to create", s.GetCreate()), fmt.Sprintf("%d to change", s.GetChange())}
	if s.GetReplace() > 0 {
		parts = append(parts, fmt.Sprintf("%d to replace", s.GetReplace()))
	}
	parts = append(parts, fmt.Sprintf("%d to delete", s.GetDestroy()))
	return strings.Join(parts, " · ")
}

// planCounts is Counts of a plan, saying what it replaces only if a pull finds a
// newer image.
func planCounts(p *dockwellv1.Plan) string {
	s := p.GetSummary()
	maybe := len(ifNewerIn(p))
	if maybe == 0 {
		return Counts(s)
	}
	parts := []string{fmt.Sprintf("%d to create", s.GetCreate()), fmt.Sprintf("%d to change", s.GetChange())}
	if certain := int(s.GetReplace()) - maybe; certain > 0 {
		parts = append(parts, fmt.Sprintf("%d to replace", certain))
	}
	parts = append(parts, fmt.Sprintf("%d to replace if newer", maybe), fmt.Sprintf("%d to destroy", s.GetDestroy()))
	return strings.Join(parts, " · ")
}

// onlyIfNewer says all a plan changes depends on its pull finding a newer image,
// so it may change nothing.
func onlyIfNewer(p *dockwellv1.Plan) bool {
	s := p.GetSummary()
	maybe := int32(len(ifNewerIn(p))) //nolint:gosec // as many as a plan's resources
	return maybe > 0 && s.GetCreate() == 0 && s.GetChange() == 0 && s.GetDestroy() == 0 && s.GetReplace() == maybe
}

// ifNewerIn are the resources a plan replaces only if a pull finds a newer image,
// on every server it replaces them.
func ifNewerIn(p *dockwellv1.Plan) []string {
	maybe, certain := map[string]bool{}, map[string]bool{}
	for _, srv := range p.GetServers() {
		for _, st := range srv.GetSteps() {
			if replacesIfNewer(st) {
				maybe[ref(st.GetResource())] = true
			} else if st.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_REPLACE {
				certain[ref(st.GetResource())] = true
			}
		}
	}
	var out []string
	for r := range maybe {
		if !certain[r] {
			out = append(out, r)
		}
	}
	return out
}

// replacesIfNewer says a step replaces what runs only if its pull finds a newer
// image: it has nothing to change that a container can't change in place, which is
// its restart policy and its limits, but for lifting a limit, as the driver reads
// a limit of zero as leaving it as it is.
func replacesIfNewer(st *dockwellv1.PlanStep) bool {
	if !st.GetPull() || st.GetAction() != dockwellv1.PlanAction_PLAN_ACTION_REPLACE {
		return false
	}
	for _, c := range st.GetChanges() {
		switch strings.TrimPrefix(c.GetPath(), "app.") {
		case "restart", "restart_retries":
		case "resources.cpus", "resources.memory", "resources.memory_reservation":
			if c.GetOp() == dockwellv1.FieldOp_FIELD_OP_REMOVED {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// shortCounts is a plan's summary in a table: "+1 ~0 -0".
func shortCounts(s *dockwellv1.PlanSummary) string {
	out := fmt.Sprintf("+%d ~%d", s.GetCreate(), s.GetChange())
	if s.GetReplace() > 0 {
		out += fmt.Sprintf(" ±%d", s.GetReplace())
	}
	return out + fmt.Sprintf(" -%d", s.GetDestroy())
}

func changesTable(w io.Writer, res *dockwellv1.ListChangesResponse, now time.Time) {
	if len(res.GetChanges()) == 0 {
		fmt.Fprintln(w, "No changes yet. Make one with a command such as dockwell projects create.")
		return
	}
	tw := newTable(w)
	row(tw, "ID", "Summary", "State", "Plan", "By", "Proposed")
	for _, c := range res.GetChanges() {
		row(tw, c.GetId(), c.GetSummary(), word(c.GetState()), shortCounts(c.GetPlan().GetSummary()), orDash(c.GetProposedBy().GetName()), when(c.GetCreateTime(), now))
	}
	_ = tw.Flush()
	if t := res.GetNextPageToken(); t != "" {
		fmt.Fprintf(w, "\nThere are more. See them with dockwell changes list --page-token %s\n", t)
	}
}

func driftTable(w io.Writer, res *dockwellv1.ListDriftResponse) {
	if len(res.GetDrift()) == 0 && len(res.GetUncheckedServers()) == 0 {
		fmt.Fprintln(w, "No drift: what every server has matches its latest revisions.")
		return
	}
	if len(res.GetDrift()) > 0 {
		tw := newTable(w)
		row(tw, "Server", "Resource", "ID", "Revision", "What differs")
		for _, d := range res.GetDrift() {
			what := "Missing from the server"
			if !d.GetMissing() {
				var paths []string
				for _, c := range d.GetChanges() {
					paths = append(paths, c.GetPath())
				}
				what = strings.Join(paths, ", ")
			}
			row(tw, d.GetServerId(), ref(d.GetResource()), d.GetResourceId(), fmt.Sprintf("r%d", d.GetRevision()), what)
		}
		_ = tw.Flush()
		fmt.Fprintln(w, "\nPut one back with dockwell drift revert, or keep what the server has with dockwell drift adopt, each with --server-id and --resource-id.")
	}
	for _, u := range res.GetUncheckedServers() {
		fmt.Fprintf(w, "%s wasn't checked: %s\n", orDash(u.GetServerName()), u.GetMessage())
	}
}

// ref names a resource as "kind name".
func ref(r *dockwellv1.ResourceRef) string {
	if r.GetKind() == "" {
		return r.GetName()
	}
	return r.GetKind() + " " + r.GetName()
}

// The panel's symbols and words for each action.
var actions = map[dockwellv1.PlanAction]struct{ symbol, word string }{
	dockwellv1.PlanAction_PLAN_ACTION_NONE:    {" ", "No change"},
	dockwellv1.PlanAction_PLAN_ACTION_CREATE:  {"+", "Create"},
	dockwellv1.PlanAction_PLAN_ACTION_UPDATE:  {"~", "Update in place"},
	dockwellv1.PlanAction_PLAN_ACTION_REPLACE: {"±", "Replace"},
	dockwellv1.PlanAction_PLAN_ACTION_DELETE:  {"-", "Delete"},
}

var surfaces = map[dockwellv1.Surface]string{
	dockwellv1.Surface_SURFACE_PANEL:     "in the panel",
	dockwellv1.Surface_SURFACE_API:       "through the API",
	dockwellv1.Surface_SURFACE_AUTOMATIC: "on its own",
	dockwellv1.Surface_SURFACE_CLI:       "from the CLI",
}

// ChangeView writes a change: what it is, who proposed and applied it,
// its plan, and how its steps went.
func ChangeView(w io.Writer, c *dockwellv1.Change, now time.Time) {
	fmt.Fprintf(w, "%s  %s\n", c.GetId(), c.GetSummary())
	proposed := fmt.Sprintf("%s by %s %s", when(c.GetCreateTime(), now), actor(c.GetProposedBy()), surfaces[c.GetProposedVia()])
	applied := fmt.Sprintf("%s by %s %s", when(c.GetApplyTime(), now), actor(c.GetAppliedBy()), surfaces[c.GetAppliedVia()])
	switch c.GetState() {
	case dockwellv1.ChangeState_CHANGE_STATE_PLANNED:
		fmt.Fprintf(w, "Planned %s.\n", proposed)
	case dockwellv1.ChangeState_CHANGE_STATE_DISCARDED:
		fmt.Fprintf(w, "Discarded. Proposed %s.\n", proposed)
	case dockwellv1.ChangeState_CHANGE_STATE_APPLYING:
		fmt.Fprintf(w, "Applying. Applied %s.\n", applied)
	case dockwellv1.ChangeState_CHANGE_STATE_APPLIED:
		fmt.Fprintf(w, "Applied %s.\n", applied)
	case dockwellv1.ChangeState_CHANGE_STATE_FAILED:
		fmt.Fprintf(w, "Failed. Applied %s.\n", applied)
	case dockwellv1.ChangeState_CHANGE_STATE_UNSPECIFIED:
	}
	switch {
	case c.GetRollback():
		fmt.Fprintf(w, "Dockwell made it to roll back %s, which failed.\n", c.GetReverts())
	case c.GetReverts() != "":
		fmt.Fprintf(w, "It undoes %s.\n", c.GetReverts())
	}
	if c.GetError() != "" {
		fmt.Fprintf(w, "It failed: %s\n", sentence(c.GetError()))
	}
	if words := rollbackWords(c); words != "" {
		fmt.Fprintln(w, words)
	}
	if c.GetBecause() != "" {
		fmt.Fprintf(w, "Dockwell made this change because of what failed: %s\n", sentence(c.GetBecause()))
	}
	if c.GetUndoneBy() != "" {
		fmt.Fprintf(w, "%s undid it.\n", c.GetUndoneBy())
	}
	fmt.Fprintln(w)
	PlanView(w, c.GetPlan())
	if len(c.GetSteps()) > 0 {
		fmt.Fprintln(w, "\nSteps:")
		tw := newTable(w)
		for _, s := range c.GetSteps() {
			row(tw, "  "+word(s.GetState()), stepText(s))
		}
		_ = tw.Flush()
		for _, s := range c.GetSteps() {
			if s.GetDetail() != "" {
				fmt.Fprintf(w, "\nWhat the server said: %s\n", sentence(s.GetDetail()))
			}
		}
	}
	switch {
	case c.GetUndoDeadline() != nil && c.GetUndoneBy() == "" && c.GetUndoDeadline().AsTime().After(now):
		fmt.Fprintf(w, "\nUndo it %s with dockwell changes undo %s.\n", untilWord(c.GetUndoDeadline().AsTime(), now), c.GetId())
	case c.GetState() == dockwellv1.ChangeState_CHANGE_STATE_PLANNED && c.GetPlan().GetUndoWindow() != nil && !c.GetPlan().GetDestructive():
		when := "once applied"
		if onlyIfNewer(c.GetPlan()) {
			when = "if this replaces anything"
		}
		fmt.Fprintf(w, "\nUndo for %s %s.\n", Span(c.GetPlan().GetUndoWindow().AsDuration()), when)
	}
}

// rollbackWords says how a failed change's rollback went, from what the API
// says of it. It runs on after the change has failed, so it may not be over,
// and may have failed too: it never says what ran before is back unless it is,
// or that anything ran before a first deploy.
func rollbackWords(c *dockwellv1.Change) string {
	s := c.GetRollbackStatus()
	id := c.GetRolledBackBy()
	first := onlyCreates(c.GetPlan())
	follow := fmt.Sprintf("Follow it with dockwell changes watch %s.", id)
	switch s.GetState() {
	case dockwellv1.RollbackState_ROLLBACK_STATE_APPLIED:
		switch {
		case s.GetSteps() == 0 && first:
			return fmt.Sprintf("It stopped before it changed anything on the servers (%s).", id)
		case s.GetSteps() == 0:
			return fmt.Sprintf("It stopped before it changed anything on the servers, so what was there before still is (%s).", id)
		case first:
			return fmt.Sprintf("Dockwell cleared away what it had made, in %s.", id)
		case len(s.GetRestored()) == 0:
			return fmt.Sprintf("What ran never stopped. Dockwell removed what this made, in %s.", id)
		case restoresWhatRuns(c.GetPlan(), s.GetRestored()) && rolled(c.GetPlan()):
			return fmt.Sprintf("Dockwell put back what ran before, in %s.", id)
		case restoresWhatRuns(c.GetPlan(), s.GetRestored()):
			return fmt.Sprintf("Dockwell put back what ran before, in %s, and it runs again. It was down while that happened.", id)
		}
		return fmt.Sprintf("Dockwell put back what was there before, in %s.", id)
	case dockwellv1.RollbackState_ROLLBACK_STATE_APPLYING:
		if first {
			return fmt.Sprintf("Dockwell is clearing away what it made, in %s. %s", id, follow)
		}
		return fmt.Sprintf("Dockwell is putting back what was there before, in %s. %s", id, follow)
	case dockwellv1.RollbackState_ROLLBACK_STATE_WAITING:
		servers := strings.Join(s.GetWaitingFor(), ", ")
		if first {
			return fmt.Sprintf("Dockwell clears away what it made, in %s, when it can reach %s. It can't tell what is there until then. %s", id, servers, follow)
		}
		return fmt.Sprintf("Dockwell puts back what was there before, in %s, when it can reach %s. It can't tell what is running there until then. %s", id, servers, follow)
	case dockwellv1.RollbackState_ROLLBACK_STATE_FAILED:
		what, left := "Putting back what was there before", "The servers may be left between the two, so check what runs, then deploy what should, or fix it by hand."
		if first {
			what, left = "Clearing away what it made", "Parts of it may still be on the servers, so check what is there, then try again, or remove them by hand."
		}
		out := fmt.Sprintf("%s failed too, in %s: %s", what, id, sentence(s.GetError()))
		if d := s.GetDetail(); d != "" {
			out += " What the server said: " + sentence(d)
		}
		return out + " " + left
	case dockwellv1.RollbackState_ROLLBACK_STATE_UNSPECIFIED:
	}
	if id != "" {
		return fmt.Sprintf("Dockwell made %s to roll it back. See how that went with dockwell changes get %s.", id, id)
	}
	return ""
}

// onlyCreates reports whether a plan only makes what wasn't there, so a
// rollback of it has nothing to put back.
func onlyCreates(p *dockwellv1.Plan) bool {
	s := p.GetSummary()
	return s.GetCreate() > 0 && s.GetChange() == 0 && s.GetReplace() == 0 && s.GetDestroy() == 0
}

// restoresWhatRuns reports whether a rollback put back something that serves,
// which stopped if it did, rather than something like a firewall rule.
func restoresWhatRuns(p *dockwellv1.Plan, restored []string) bool {
	for _, r := range p.GetResources() {
		kind := r.GetResource().GetKind()
		if (kind == "service" || kind == "container") && slices.Contains(restored, r.GetResource().GetName()) {
			return true
		}
	}
	return false
}

// rolled says what a plan set out to do rolls, so what ran goes on serving until the
// new one is healthy.
func rolled(p *dockwellv1.Plan) bool {
	some := false
	for _, server := range p.GetServers() {
		for _, st := range server.GetSteps() {
			if kind := st.GetResource().GetKind(); kind != "service" && kind != "container" {
				continue
			}
			if !st.GetRollout().GetRolling() || startsStopped(st) {
				return false
			}
			some = true
		}
	}
	return some
}

// untilWord says how long is left: "within 7 days", "within 3 hours",
// to the nearest minute, so a deadline a moment under 7 days away reads 7.
func untilWord(t, now time.Time) string { return "within " + Span(t.Sub(now).Round(time.Minute)) }

func actor(a *dockwellv1.Actor) string {
	switch {
	case a == nil:
		return "someone"
	case a.GetKind() == dockwellv1.ActorKind_ACTOR_KIND_TOKEN:
		return "the API token " + a.GetName()
	case a.GetName() != "":
		return a.GetName()
	}
	return a.GetId()
}

func stepText(s *dockwellv1.StepProgress) string {
	if s.GetMessage() != "" {
		return s.GetMessage()
	}
	return actions[s.GetAction()].word + " " + ref(s.GetResource())
}

// sentence ends a message with a full stop.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s != "" && !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// placeWords says where a plan lands, naming what offers a choice.
func placeWords(p *dockwellv1.PlanPlace) string {
	switch {
	case p.GetProject() != "" && p.GetEnvironment() != "":
		return fmt.Sprintf("In project %s, environment %s.", p.GetProject(), p.GetEnvironment())
	case p.GetProject() != "":
		return fmt.Sprintf("In project %s.", p.GetProject())
	}
	return fmt.Sprintf("In environment %s.", p.GetEnvironment())
}

// PlanView writes a plan as the panel's plan sheet shows it: what happens
// to each resource that runs on no server, then each server's steps with
// their downtime, data loss and warnings, then the counts.
func PlanView(w io.Writer, p *dockwellv1.Plan) {
	if p == nil {
		return
	}
	for _, place := range p.GetPlaces() {
		fmt.Fprintln(w, placeWords(place))
	}
	if len(p.GetPlaces()) > 0 {
		fmt.Fprintln(w)
	}
	onServers := map[string]bool{}
	for _, s := range p.GetServers() {
		for _, st := range s.GetSteps() {
			onServers[st.GetResourceId()] = true
		}
	}
	byID := map[string]*dockwellv1.PlannedResource{}
	tw := newTable(w)
	wrote := false
	for _, r := range p.GetResources() {
		byID[r.GetId()] = r
		if onServers[r.GetId()] || r.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_NONE {
			continue
		}
		a := actions[r.GetAction()]
		row(tw, "  "+a.symbol+" "+ref(r.GetResource()), a.word)
		fieldChanges(tw, r.GetChanges(), "      ")
		wrote = true
	}
	_ = tw.Flush()
	for _, s := range p.GetServers() {
		if wrote {
			fmt.Fprintln(w)
		}
		wrote = true
		head := s.GetServerName()
		switch {
		case s.GetRemoved():
			head += ", removed from the fleet: what the change takes off it is only recorded"
		case s.GetOffline():
			head += ", offline: its steps are planned from the state it last reported, and the change can't apply until it reconnects"
		}
		if d := s.GetDowntime(); d.AsDuration() > 0 {
			head += " · about " + downtime(d) + " of downtime"
			if h := s.GetUntilHealthy(); h.AsDuration() > d.AsDuration() {
				head += ", and up to " + upTo(h.AsDuration()) + " until healthy"
			}
		}
		fmt.Fprintf(w, "On %s\n", head)
		tw := newTable(w)
		for _, st := range s.GetSteps() {
			a := actions[st.GetAction()]
			details := []string{a.word}
			ifNewer := replacesIfNewer(st)
			switch {
			case ifNewer && len(st.GetChanges()) == 0:
				details[0] = "Replace if newer"
			case ifNewer:
				details[0] = "Update, and replace if newer"
			}
			switch st.GetEffect() {
			case dockwellv1.StepEffect_STEP_EFFECT_RESTART:
				details = append(details, "restarts")
			case dockwellv1.StepEffect_STEP_EFFECT_ROLL:
				details = append(details, "rolling, with nothing stopped")
			case dockwellv1.StepEffect_STEP_EFFECT_RECREATE:
				if !ifNewer {
					details = append(details, "recreated")
				}
			default:
			}
			if d := st.GetDowntime(); d.AsDuration() > 0 {
				what := "about " + downtime(d) + " downtime"
				if h := st.GetUntilHealthy(); h.AsDuration() > d.AsDuration() {
					what += ", and up to " + upTo(h.AsDuration()) + " until healthy"
				}
				details = append(details, what)
			}
			row(tw, "  "+a.symbol+" "+ref(st.GetResource()), strings.Join(details, " · "))
			changes := st.GetChanges()
			if len(changes) == 0 {
				changes = byID[st.GetResourceId()].GetChanges()
			}
			fieldChanges(tw, changes, "      ")
			// Lines without a tab don't widen the columns.
			switch {
			case len(st.GetKeptPaths()) > 0:
				fmt.Fprintf(tw, "      Takes out of use the data its image keeps at %s. The server keeps it for 7 days, and undoing this mounts it again.\n",
					human.List(st.GetKeptPaths()))
			case st.GetDestroysData():
				fmt.Fprintln(tw, "      Destroys data its spec can't bring back.")
			}
			if st.GetRestores() {
				fmt.Fprintln(tw, "      Brings back the data the server kept. If the server no longer has it, applying fails, and nothing is made empty.")
			}
			for _, line := range restoreWords(st.GetRestore()) {
				fmt.Fprintln(tw, "      "+line)
			}
			if st.GetHidesLogs() {
				fmt.Fprintln(tw, "      Its kept logs can't be read from then on, by any name, and go in 7 days. Undoing this brings them back.")
			}
			if access := st.GetHostAccess(); len(access) > 0 {
				fmt.Fprintln(tw, "      "+reachWords(access, s.GetServerName()))
			}
			if st.GetHealthUnknown() {
				fmt.Fprintln(tw, "      Its image isn't on the server yet, so its own health check isn't known, and it may take longer to be healthy.")
			}
			for _, line := range rolloutWords(st.GetRollout(), st.GetRestart() != "", startsStopped(st)) {
				fmt.Fprintln(tw, "      "+line)
			}
			for _, line := range ingressWords(st.GetIngress(), s.GetServerName()) {
				fmt.Fprintln(tw, "      "+line)
			}
			if reloads(st) {
				fmt.Fprintf(tw, "      This reloads the proxy on %s, which can reset a connection on its way in to any site there, and lose a POST on a kept HTTP/1.1 connection. A browser sends such a request again, and an app's own client may not.\n", s.GetServerName())
			}
			if line := returningWords(st.GetReturningContainers(), s.GetServerName()); line != "" {
				fmt.Fprintln(tw, "      "+line)
			}
			if line := domainWords(st.GetDomainCheck()); line != "" {
				fmt.Fprintln(tw, "      "+line)
			}
			if st.GetUnencrypted() {
				fmt.Fprintln(tw, "      "+unencryptedWords(s.GetServerName()))
			}
			for _, line := range answersWords(st.GetAnswers(), st.GetServerPort(), s.GetServerName()) {
				fmt.Fprintln(tw, "      "+line)
			}
			if line := goesWords(st.GetGoes()); line != "" {
				fmt.Fprintln(tw, "      "+line)
			}
			if line := movesWords(st.GetMoves()); line != "" {
				fmt.Fprintln(tw, "      "+line)
			}
			for _, warning := range st.GetWarnings() {
				if warning == dockwellv1.PlanWarning_PLAN_WARNING_MAY_LOCK_OUT {
					fmt.Fprintln(tw, "      May cut off remote access to "+s.GetServerName()+", such as SSH.")
				}
			}
		}
		_ = tw.Flush()
	}
	if wrote {
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, planCounts(p))
	if p.GetDestructive() {
		fmt.Fprintln(w, "It destroys data, may cut off access, or lets two containers write to one place, which undo may not bring back.")
	}
	if p.GetHostAccess() {
		fmt.Fprintln(w, "It reaches more of a server, or what isn't its own yet, which a person approves:")
		// Here as well as on the steps, as a plan that only writes down
		// what a server has, such as adopting drift, has none.
		for _, r := range p.GetResources() {
			if access := r.GetHostAccess(); len(access) > 0 {
				fmt.Fprintf(w, "  %s: %s\n", ref(r.GetResource()), human.List(accessWords(access)))
			}
		}
	}
	for _, n := range p.GetNotes() {
		// A value anyone can read stops nobody, and is the most a note can say.
		label := "Note"
		switch {
		case strings.HasPrefix(n.GetReason(), "exposed_"), n.GetReason() == "no_backup", n.GetReason() == "undo_no_backup", n.GetReason() == "managed_elsewhere":
			label = "Warning"
		case n.GetReason() == "first_steps":
			label = "First steps"
		}
		message := strings.TrimSuffix(n.GetMessage(), ".")
		if names := n.GetNames(); n.GetReason() == "named_as_text" && len(names) > 0 {
			flag := "--app-references"
			if n.GetResource().GetKind() == "container" {
				flag = "--references"
			}
			message += fmt.Sprintf(", with %s %s on the command line", flag, names[0])
		}
		fmt.Fprintf(w, "%s: %s.\n", label, message)
	}
}

// restoreWords says what a step that restores a volume does: what it replaces, what
// stops, and what stays.
func restoreWords(r *dockwellv1.RestorePlan) []string {
	if r == nil {
		return nil
	}
	taken := r.GetTakenAt().AsTime().Local().Format("2 Jan 2006 15:04")
	size := human.Size(r.GetSizeBytes())
	var out []string
	switch {
	case r.GetUndoes() != "":
		out = append(out, fmt.Sprintf("Replaces what's in the volume with what %s replaced on %s (%s).", r.GetUndoes(), taken, size))
	case r.GetMethod() == dockwellv1.BackupMethod_BACKUP_METHOD_DUMP:
		out = append(out, fmt.Sprintf("Replaces what's in the volume with a new %s holding the dump from %s (%s). What was made since goes.", r.GetDatabase(), taken, size))
	case r.GetMethod() == dockwellv1.BackupMethod_BACKUP_METHOD_COPY_RUNNING:
		out = append(out, fmt.Sprintf("Replaces what's in the volume with the copy from %s (%s). The app ran while it was copied, so it can hold half a write.", taken, size))
	default:
		out = append(out, fmt.Sprintf("Replaces what's in the volume with the copy from %s (%s).", taken, size))
	}
	if r.GetWrittenBy() != "" {
		out = append(out, fmt.Sprintf("%s wrote these files, and %s runs on them now, so the database may not start. Deploy %s first and restore then, or restore a dump instead.", r.GetWrittenBy(), r.GetRunsOn(), r.GetWrittenBy()))
	}
	if stops := r.GetStops(); len(stops) > 0 {
		out = append(out, "Stops "+human.List(stops)+" while it happens, and starts what it stopped again after.")
	}
	return append(out, "The server keeps what's there now for 7 days. Undoing this puts it back.")
}

// ingressWords says what a step does to its server's ingress.
// startsStopped says a step starts what's there and isn't running, so nothing serves
// while it does.
func startsStopped(st *dockwellv1.PlanStep) bool {
	return slices.ContainsFunc(st.GetChanges(), func(c *dockwellv1.FieldChange) bool { return c.GetPath() == "state" && c.GetTo() == "running" })
}

// unchecked says how a container that nothing checks takes over: once it has run for
// soak, as healthy is only that it runs.
func unchecked(soak time.Duration, restart, stopped bool) string {
	if stopped {
		runs := "Nothing checks it, so it only has to start"
		if soak > 0 {
			runs = "Nothing checks it, so it has to keep running for " + human.Duration(soak)
		}
		if restart {
			return runs + ". If it doesn't, the new one goes, and what was stopped stays stopped."
		}
		return runs + ". If it doesn't, Dockwell rolls this back."
	}
	takes := "Nothing checks it, so a new container takes over once it starts"
	if soak > 0 {
		takes = "Nothing checks it, so a new container takes over once it has kept running for " + human.Duration(soak)
	}
	if restart {
		return takes + ", and what runs goes on serving until then. If it doesn't, the new one goes, and what runs stays."
	}
	return takes + ", and what runs goes on serving until then. If it doesn't, Dockwell rolls this back."
}

func rolloutWords(r *dockwellv1.RolloutPlan, restart, stopped bool) []string {
	if r == nil {
		return nil
	}
	var out []string
	say := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	shared := slices.Concat(r.GetVolumes(), r.GetPaths(), r.GetImageData())
	if r.GetRolling() {
		d := r.GetHealthyWithin().AsDuration()
		switch {
		case r.GetNoCheck() && d > 0:
			// What nothing checks is healthy as it runs: how long it must is all there is to say.
			say("%s", unchecked(r.GetSoak().AsDuration(), restart, stopped))
		case stopped:
			// Nothing runs, so nothing goes on serving, and nothing is in flight.
			if restart {
				say("It has to stay healthy for %s. If it isn't healthy within %s, the new one goes, and what was stopped stays stopped.", human.Duration(r.GetSoak().AsDuration()), human.Duration(d))
			} else {
				say("It has to stay healthy for %s. If it isn't healthy within %s, Dockwell rolls this back.", human.Duration(r.GetSoak().AsDuration()), human.Duration(d))
			}
		case d > 0:
			soak := ""
			if s := r.GetSoak().AsDuration(); s > 0 {
				soak = " and has stayed so for " + human.Duration(s)
			}
			if restart {
				say("A new container takes over once it is healthy%s, and what runs goes on serving until then. If it isn't healthy within %s, the new one goes, and what runs stays.", soak, human.Duration(d))
			} else {
				say("A new container takes over once it is healthy%s, and what runs goes on serving until then. If it isn't healthy within %s, Dockwell rolls this back.", soak, human.Duration(d))
			}
		default:
			say("What this step wants already runs, so nothing stops for it.")
		}
		if r.GetChosen() && len(shared)+len(r.GetCantTell()) > 0 {
			say("Its rollout order says to start first, so two containers may write to %s for as long as this takes. Two writers can corrupt a database, so a person approves this.", strings.Join(slices.Concat(shared, r.GetCantTell()), ", "))
		}
		if len(r.GetOthers()) > 0 && !stopped {
			say("What is in flight gets up to %s to finish. What outlasts that, such as a long WebSocket, ends with the old container.", human.Duration(r.GetDrain().AsDuration()))
		}
		if n := r.GetEarlyNetworks(); len(n) > 0 && !stopped {
			name := "its service's name"
			if a := r.GetEarlyNames(); len(a) > 0 {
				name += " and by " + strings.Join(a, ", ")
			}
			say("On %s, other services can reach the new container by %s before it has passed its check, beside the old one, as Docker can't add a name to a container later.", strings.Join(n, ", "), name)
		}
	} else if !stopped {
		if p := r.GetPorts(); len(p) > 0 {
			say("Two containers can't publish %s at once, so the old one stops first.", strings.Join(p, ", "))
		}
		if r.GetHostNetwork() {
			say("A container on the server's own network listens on the server's ports itself, so two can't run at once, and the old one stops first.")
		}
		if len(shared) > 0 {
			say("Two containers mustn't write to %s at once, so the old one stops first. If the app can, set its rollout order to start first.", strings.Join(shared, ", "))
		}
		if c := r.GetCantTell(); len(c) > 0 {
			say("Dockwell can't read where %s really lands, so it can't say two containers wouldn't write to one place, and the old one stops first. If the app can, set its rollout order to start first.", strings.Join(c, ", "))
		}
		if e := r.GetEditedRoutes(); len(e) > 0 {
			say("The route %s was edited by hand, and Dockwell leaves it as it is, so the new container has to keep the old one's address, and the old one stops first. Revert or adopt the edit to roll.", strings.Join(e, ", "))
		}
		if len(r.GetPorts()) == 0 && len(shared) == 0 && len(r.GetCantTell()) == 0 && len(r.GetEditedRoutes()) == 0 && r.GetChosen() {
			say("Its rollout order says to stop first, so there are never two.")
		}
		if restart {
			say("The old container stops first, so if the new one doesn't come up, nothing of this runs, and a restart has nothing to go back to.")
		}
	}
	for _, m := range r.GetDropped() {
		if m.GetEmptyVolume() {
			say("%s stops being what the service sees at %s, and its image puts an empty volume there. %s stays on the server, as it was.", m.GetSource(), m.GetTarget(), m.GetSource())
		} else {
			say("%s stops being what the service sees at %s. %s stays on the server, as it was.", m.GetSource(), m.GetTarget(), m.GetSource())
		}
	}
	if o := r.GetOthers(); len(o) > 0 {
		say("Removes %s.", strings.Join(o, ", "))
	}
	return out
}

func ingressWords(e *dockwellv1.IngressEffect, server string) []string {
	if e == nil {
		return nil
	}
	var out []string
	proxy, ports := e.GetProxy(), human.List(e.GetPorts())
	switch e.GetChange() {
	case dockwellv1.IngressChange_INGRESS_CHANGE_STARTS:
		out = append(out, fmt.Sprintf("%s isn't running on %s, so this route starts it, and it takes ports %s.", proxy, server, ports))
		if d := e.GetPanelDomain(); d != "" {
			out = append(out, fmt.Sprintf("The panel's domain, %s, moves behind %s. Its address, passkeys and sessions stay as they are, and it keeps its present certificate until %s has its own. HTTPS on it is down for a moment as the ports change hands; the SSH tunnel keeps working.", d, proxy, proxy))
		}
	case dockwellv1.IngressChange_INGRESS_CHANGE_STOPS:
		out = append(out, fmt.Sprintf("This is the last route on %s, so %s stops, and ports %s are free. Its certificates stay on the server for when a route comes back.", server, proxy, ports))
		if d := e.GetPanelDomain(); d != "" {
			out = append(out, fmt.Sprintf("The panel takes those ports back for its domain, %s. Its address, passkeys and sessions stay as they are.", d))
		}
	case dockwellv1.IngressChange_INGRESS_CHANGE_RESTARTS:
		out = append(out, fmt.Sprintf("%s restarts on %s, for a newer version, so everything it serves is down for that long.", proxy, server))
	case dockwellv1.IngressChange_INGRESS_CHANGE_TAKES_PORTS:
		out = append(out, fmt.Sprintf("This is the first route on %s that takes a certificate, so %s takes ports %s too, and nothing it serves stops.", server, proxy, ports))
	case dockwellv1.IngressChange_INGRESS_CHANGE_FREES_PORTS:
		out = append(out, fmt.Sprintf("No route on %s takes a certificate after this, so %s frees ports %s. Its routes that take none go on answering.", server, proxy, ports))
	default:
	}
	return out
}

// unencryptedWords says what serving a route over plain HTTP means. It isn't an
// alarm: it's what a tunnel or a private network wants, and it never stops a plan.
func unencryptedWords(server string) string {
	return fmt.Sprintf("Traffic for this route reaches %s unencrypted, as TLS ends before it. That's fine on a tunnel or a private network, and not on the open internet, where anyone on the way can read it.", server)
}

// movesWords says a route leaves the service it sent to, as what a domain reaches
// changing is easy to miss among a plan's fields.
func movesWords(m *dockwellv1.RouteMove) string {
	if m == nil {
		return ""
	}
	if m.GetTo() == "" {
		return fmt.Sprintf("%s leaves %s, and goes to port %d on the server.", m.GetWhere(), m.GetFrom(), m.GetToPort())
	}
	return fmt.Sprintf("%s leaves %s, and goes to %s.", m.GetWhere(), m.GetFrom(), m.GetTo())
}

// goesWords says what someone visiting a route finds once a step takes it away, as a
// plan's "no downtime" says nothing of it.
func goesWords(g *dockwellv1.RouteGoes) string {
	if g == nil {
		return ""
	}
	switch g.GetFind() {
	case dockwellv1.VisitorsFind_VISITORS_FIND_REFUSED:
		return fmt.Sprintf("Anyone visiting %s gets a refused connection, as nothing answers on this server's port any more.", g.GetWhere())
	case dockwellv1.VisitorsFind_VISITORS_FIND_TLS_ERROR:
		return fmt.Sprintf("Anyone visiting %s gets a browser's connection error, as nothing here answers for that name over HTTPS any more.", g.GetWhere())
	case dockwellv1.VisitorsFind_VISITORS_FIND_NOT_FOUND:
		return fmt.Sprintf("What's in front gets a 404 for %s, so visitors see whatever it shows for one.", g.GetWhere())
	case dockwellv1.VisitorsFind_VISITORS_FIND_NOTHING_BEHIND:
		return fmt.Sprintf("What's in front can't connect for %s any more, so visitors see whatever it shows for that.", g.GetWhere())
	case dockwellv1.VisitorsFind_VISITORS_FIND_REST:
		return fmt.Sprintf("Anyone visiting %s gets what the rest of %s serves, or a 404 if nothing covers that path.", g.GetWhere(), g.GetDomain())
	default:
		return fmt.Sprintf("Anyone visiting %s stops reaching it.", g.GetWhere())
	}
}

// answersWords says who a route answers, when it isn't anyone over HTTPS sent to a
// service, and for a port on the server, what that does to what's there.
func answersWords(a *dockwellv1.RouteAnswers, port *dockwellv1.ServerPortCheck, server string) []string {
	if a == nil {
		return nil
	}
	var out []string
	who := ""
	switch a.GetReach() {
	case dockwellv1.RouteReach_ROUTE_REACH_THIS_SERVER:
		who = "what runs on " + server + " itself"
		out = append(out, fmt.Sprintf("It answers only what runs on %s itself, such as a tunnel on its own network. Anyone else gets what a name with no route gets.", server))
	case dockwellv1.RouteReach_ROUTE_REACH_PROXIES:
		list := human.List(a.GetProxies())
		who = "whoever comes through " + list
		out = append(out, fmt.Sprintf("It answers only %s. Anyone else gets what a name with no route gets.", list))
	case dockwellv1.RouteReach_ROUTE_REACH_ANYONE:
		who = "anyone on the internet"
		if a.GetPlain() {
			who = "anyone who can reach " + server
			out = append(out, fmt.Sprintf("It answers anyone who can reach %s on port 80, over plain HTTP, around whatever is in front of it.", server))
		}
	default:
	}
	if port == nil {
		return out
	}
	out = append(out, fmt.Sprintf("That lets %s reach port %d, and what's there sees each caller as this server, 127.0.0.1, which an app that trusts localhost will believe.", who, port.GetPort()))
	switch {
	case port.GetListening() != "":
		out = append(out, fmt.Sprintf("Listening there now: %s.", port.GetListening()))
	case port.GetNothingListening():
		out = append(out, fmt.Sprintf("Nothing listens on port %d yet, so this answers 502 until something does.", port.GetPort()))
	}
	return out
}

// reloads says a step changes a route on a proxy that stays up, which reloads it. A
// rollout doesn't: it moves traffic with no reload (ADR 0027).
func reloads(st *dockwellv1.PlanStep) bool {
	if st.GetResource().GetKind() != "route" || st.GetAction() == dockwellv1.PlanAction_PLAN_ACTION_NONE {
		return false
	}
	switch st.GetIngress().GetChange() {
	case dockwellv1.IngressChange_INGRESS_CHANGE_STARTS, dockwellv1.IngressChange_INGRESS_CHANGE_STOPS, dockwellv1.IngressChange_INGRESS_CHANGE_RESTARTS:
		return false
	default:
		return true
	}
}

// reachWords says what a step reaches that a person approves. A domain isn't
// access to a server, and the panel's domain moving isn't given to anything.
func reachWords(access []*dockwellv1.HostAccess, server string) string {
	var reach, other, off []*dockwellv1.HostAccess
	for _, a := range access {
		switch a.GetKind() {
		case dockwellv1.HostAccessKind_HOST_ACCESS_KIND_DOMAIN, dockwellv1.HostAccessKind_HOST_ACCESS_KIND_PANEL_DOMAIN, dockwellv1.HostAccessKind_HOST_ACCESS_KIND_TRUSTED_PROXY, dockwellv1.HostAccessKind_HOST_ACCESS_KIND_PLAIN_FOR_ANYONE:
			other = append(other, a)
		case dockwellv1.HostAccessKind_HOST_ACCESS_KIND_HTTPS_OFF:
			off = append(off, a)
		default:
			reach = append(reach, a)
		}
	}
	var out []string
	if len(reach) > 0 {
		out = append(out, fmt.Sprintf("Gives it access to %s: %s.", server, human.List(accessWords(reach))))
	}
	if len(off) > 0 {
		out = append(out, fmt.Sprintf("Takes away %s.", human.List(accessWords(off))))
	}
	if len(other) > 0 {
		out = append(out, fmt.Sprintf("Takes in %s.", human.List(accessWords(other))))
	}
	return strings.Join(out, " ") + " A person approves this."
}

// returningWords warns of what would take the proxy's ports back at the next
// reboot, with how to keep it from happening. It never stops a plan.
func returningWords(names []string, server string) string {
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("Docker starts %s again when %s next restarts, which may take port 80 or 443 from the proxy, and stop this route answering. Keep it stopped with docker update --restart=no %s.",
		human.List(names), server, strings.Join(names, " "))
}

// domainWords says what DNS said of a route's domain as this was planned, if
// that is worth saying. It never stops a plan, as people often set DNS after.
func domainWords(c *dockwellv1.DomainCheck) string {
	if c == nil {
		return ""
	}
	found, here := human.List(c.GetFound()), human.List(c.GetHere())
	public := resource.PublicAddresses(c.GetHere())
	point := resource.PointHere(c.GetHere())
	switch c.GetState() {
	case dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_NO_RECORD:
		return fmt.Sprintf("%s has no DNS record yet, so its certificate can't come until it has one. %s", c.GetDomain(), point)
	case dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_ELSEWHERE:
		const tunnel = "If a tunnel or a proxy in front ends TLS, set tls to off on this route instead."
		if len(public) > 0 {
			return fmt.Sprintf("DNS for %s points at %s, not this server (%s), so its certificate can't come yet. Point it here; it comes once you do. %s", c.GetDomain(), found, human.List(public), tunnel)
		}
		return fmt.Sprintf("DNS for %s points at %s, not this server, so its certificate can't come yet. %s %s", c.GetDomain(), found, point, tunnel)
	case dockwellv1.DomainCheckState_DOMAIN_CHECK_STATE_CANT_TELL:
		return fmt.Sprintf("DNS for %s points at %s. This server's own addresses are private (%s), so the agent can't tell whether that is this server. If it isn't, its certificate can't come until it is. If a tunnel or a proxy in front ends TLS, set tls to off on this route instead.", c.GetDomain(), found, here)
	default:
		return ""
	}
}

// accessWords says what each access is, for people.
func accessWords(list []*dockwellv1.HostAccess) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, resource.AccessWords(resource.Access{
			Kind: accessKinds[a.GetKind()], Path: a.GetPath(), ReadOnly: a.GetReadOnly(), Type: a.GetType(),
			Address: a.GetAddress(), Relabel: a.GetRelabel(), Driver: a.GetDriver(), Detail: a.GetDetail(), SameAsRoot: a.GetSameAsRoot(), LeadsTo: a.GetLeadsTo(),
		}))
	}
	return out
}

var accessKinds = map[dockwellv1.HostAccessKind]resource.AccessKind{
	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PATH:    resource.AccessPath,
	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_DOCKER_SOCKET:  resource.AccessDocker,
	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_HOST_NETWORK:   resource.AccessNetwork,
	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_VOLUME_MOUNT:   resource.AccessMount,
	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_NETWORK_DRIVER: resource.AccessNetworkDriver,
	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_DOMAIN:         resource.AccessDomain,
	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_PANEL_DOMAIN:   resource.AccessPanelDomain,

	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_EXTERNAL_NETWORK: resource.AccessExternalNetwork,
	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_EXTERNAL_VOLUME:  resource.AccessExternalVolume,
	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_TAKES_ON:         resource.AccessTakesOn,
	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_CAPABILITY:       resource.AccessCapability,
	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_GROUP:            resource.AccessGroup,
	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_PRIVILEGED:       resource.AccessPrivileged,

	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_SERVER_PORT:      resource.AccessServerPort,
	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_TRUSTED_PROXY:    resource.AccessTrustedProxy,
	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_PLAIN_FOR_ANYONE: resource.AccessPlainForAnyone,
	dockwellv1.HostAccessKind_HOST_ACCESS_KIND_HTTPS_OFF:        resource.AccessHTTPSOff,
}

// upTo writes a bound to the second, rounding up, as "1 min 50 s", so it
// never reads as less than it is.
func upTo(d time.Duration) string {
	secs := int((d + time.Second - 1) / time.Second)
	if secs < 60 {
		return strconv.Itoa(secs) + " s"
	}
	out := strconv.Itoa(secs/60) + " min"
	if secs%60 > 0 {
		out += " " + strconv.Itoa(secs%60) + " s"
	}
	return out
}

func downtime(d *durationpb.Duration) string {
	if d.AsDuration() < time.Second {
		return "under 1 s"
	}
	return Span(d.AsDuration())
}

// fieldChanges writes each field that changes, never a sensitive value.
func fieldChanges(tw io.Writer, changes []*dockwellv1.FieldChange, indent string) {
	for _, c := range changes {
		from, to := c.GetFrom(), c.GetTo()
		if c.GetSensitive() {
			from, to = "(hidden)", "(hidden)"
		}
		switch c.GetOp() {
		case dockwellv1.FieldOp_FIELD_OP_ADDED:
			row(tw, indent+"+ "+c.GetPath(), to)
		case dockwellv1.FieldOp_FIELD_OP_REMOVED:
			row(tw, indent+"- "+c.GetPath(), from)
		default:
			if c.GetSensitive() {
				row(tw, indent+"~ "+c.GetPath(), "(hidden)")
				continue
			}
			row(tw, indent+"~ "+c.GetPath(), from+" → "+to)
		}
	}
}

// generic writes any message: a table for a list, and otherwise each field
// that is set.
func generic(w io.Writer, m protoreflect.Message, now time.Time) {
	fields := m.Descriptor().Fields()
	var list protoreflect.FieldDescriptor
	others := 0
	for i := range fields.Len() {
		fd := fields.Get(i)
		switch {
		case fd.IsList() && fd.Message() != nil && list == nil:
			list = fd
		case fd.Name() == "next_page_token":
		default:
			others++
		}
	}
	if list != nil && others == 0 {
		genericTable(w, list, m.Get(list).List(), now)
		if fd := fields.ByName("next_page_token"); fd != nil && m.Get(fd).String() != "" {
			fmt.Fprintf(w, "\nThere are more. See them with --page-token %s\n", m.Get(fd).String())
		}
		return
	}
	tw := newTable(w)
	genericFields(tw, m, "", now)
	_ = tw.Flush()
}

func genericFields(tw io.Writer, m protoreflect.Message, indent string, now time.Time) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		label := indent + Label(fd)
		switch {
		case fd.IsList() && fd.Message() != nil && !isWellKnown(fd.Message()):
			l := v.List()
			for i := range l.Len() {
				row(tw, fmt.Sprintf("%s %d", label, i+1), "")
				genericFields(tw, l.Get(i).Message(), indent+"  ", now)
			}
		case fd.IsList():
			l := v.List()
			items := make([]string, 0, l.Len())
			for i := range l.Len() {
				items = append(items, format(fd, l.Get(i), now))
			}
			row(tw, label, strings.Join(items, ", "))
		case fd.IsMap():
			v.Map().Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				row(tw, label+" "+k.String(), format(fd.MapValue(), mv, now))
				return true
			})
		case fd.Message() != nil && !isWellKnown(fd.Message()):
			row(tw, label, "")
			genericFields(tw, v.Message(), indent+"  ", now)
		default:
			row(tw, label, format(fd, v, now))
		}
		return true
	})
}

func genericTable(w io.Writer, fd protoreflect.FieldDescriptor, l protoreflect.List, now time.Time) {
	if l.Len() == 0 {
		fmt.Fprintf(w, "No %s.\n", strings.ToLower(Label(fd)))
		return
	}
	md := fd.Message()
	var cols []protoreflect.FieldDescriptor
	fields := md.Fields()
	for i := range fields.Len() {
		c := fields.Get(i)
		if c.IsList() || c.IsMap() || (c.Message() != nil && !isWellKnown(c.Message())) {
			continue
		}
		for j := range l.Len() {
			if l.Get(j).Message().Has(c) {
				cols = append(cols, c)
				break
			}
		}
	}
	tw := newTable(w)
	head := make([]string, len(cols))
	for i, c := range cols {
		head[i] = Label(c)
	}
	row(tw, head...)
	for i := range l.Len() {
		item := l.Get(i).Message()
		cells := make([]string, len(cols))
		for j, c := range cols {
			if item.Has(c) {
				cells[j] = format(c, item.Get(c), now)
			} else {
				cells[j] = "-"
			}
		}
		row(tw, cells...)
	}
	_ = tw.Flush()
}

func isWellKnown(md protoreflect.MessageDescriptor) bool {
	return md.FullName() == timestampName || md.FullName() == durationName
}

// labels name fields whose generic label reads badly.
var labels = map[protoreflect.Name]string{
	"create_time":    "Created",
	"expire_time":    "Expires",
	"last_used_time": "Last used",
	"last_seen_time": "Last seen",
	"apply_time":     "Applied",
	"connect_time":   "Connected",
	"commit_time":    "Committed",
	"lock_time":      "Locked",
	"start_time":     "Started",
	"finish_time":    "Finished",
	"undo_deadline":  "Undo until",
}

var acronyms = map[string]string{"id": "ID", "rp": "RP", "os": "OS", "cpu": "CPU", "ip": "IP", "url": "URL", "uri": "URI", "json": "JSON", "ttl": "TTL", "api": "API"}

// Label is a field's name for people: "server_name" is "Server name",
// "create_time" "Created".
func Label(fd protoreflect.FieldDescriptor) string {
	if l, ok := labels[fd.Name()]; ok {
		return l
	}
	words := strings.Split(string(fd.Name()), "_")
	for i, w := range words {
		if a, ok := acronyms[w]; ok {
			words[i] = a
		}
	}
	return capitalize(strings.Join(words, " "))
}

// format writes one value of fd for people.
func format(fd protoreflect.FieldDescriptor, v protoreflect.Value, now time.Time) string {
	name := string(fd.Name())
	switch fd.Kind() {
	case protoreflect.BoolKind:
		if v.Bool() {
			return "Yes"
		}
		return "No"
	case protoreflect.EnumKind:
		return enumWord(fd.Enum(), v.Enum())
	case protoreflect.Uint64Kind, protoreflect.Uint32Kind, protoreflect.Fixed64Kind:
		switch {
		case strings.HasSuffix(name, "_bytes_per_second"):
			return Bytes(v.Uint()) + "/s"
		case strings.HasSuffix(name, "_bytes"):
			return Bytes(v.Uint())
		}
	case protoreflect.DoubleKind, protoreflect.FloatKind:
		if strings.HasSuffix(name, "_percent") {
			return Percent(v.Float())
		}
	case protoreflect.MessageKind:
		switch fd.Message().FullName() {
		case timestampName:
			t, _ := v.Message().Interface().(*timestamppb.Timestamp)
			return when(t, now)
		case durationName:
			d, _ := v.Message().Interface().(*durationpb.Duration)
			return Span(d.AsDuration())
		}
	default:
	}
	return v.String()
}

// stepStates in the order a step moves through them, for telling a step
// that moved on.
var finalSteps = []dockwellv1.StepState{
	dockwellv1.StepState_STEP_STATE_DONE, dockwellv1.StepState_STEP_STATE_FAILED, dockwellv1.StepState_STEP_STATE_SKIPPED,
}

func finalStep(s dockwellv1.StepState) bool { return slices.Contains(finalSteps, s) }
