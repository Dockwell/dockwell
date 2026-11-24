// SPDX-License-Identifier: AGPL-3.0-only

package apicli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/human"
)

const (
	baselineCLITargetCap = 12
	defaultCLIServerCap  = 200
	defaultCLITotalCap   = 500
)

func dockerCleanupCommand(env *Env, o *options) *cobra.Command {
	var servers []string
	var kinds []string
	var taggedImages bool
	var namedVolumes bool
	var limit int
	var summary string
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Plan removal of unused Docker images, volumes, networks or build cache",
		Long: "Previews unused Docker artifacts on the selected servers and plans a reviewed cleanup change.\n\n" +
			"By default it selects the safe set: dangling images, idle build cache, unused networks and anonymous volumes. " +
			"Unused tagged images stay unselected unless you pass --tagged-images, and unused named volumes stay unselected unless you pass --named-volumes. " +
			"Use --kind to scope to one or more kinds (images, volumes, networks, build-cache), and --limit to cap how many artifacts this plan removes.\n\n" +
			"It prints the plan and needs the same passkey approval in the panel as any Docker cleanup.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usage(fmt.Errorf("%s takes no arguments, but got %q. Run %q to see how to use it", cmd.CommandPath(), args[0], cmd.CommandPath()+" --help"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			parsedKinds, err := parseCleanupKinds(kinds)
			if err != nil {
				return usage(err)
			}
			if limit < 0 {
				return usage(fmt.Errorf("--limit is %d, which is negative. Pass 0 for the server's limit, or a positive number", limit))
			}
			r := &runner{env: env, opts: o, command: cmd.CommandPath()}
			return r.dockerCleanup(cmd.Context(), servers, parsedKinds, taggedImages, namedVolumes, limit, summary)
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&servers, "server", nil, "a server to clean up, by name or ID; repeat it for several. Leave it out while there is only one server")
	f.StringArrayVar(&kinds, "kind", nil, "artifact kind to include: images, volumes, networks or build-cache; repeat for several. Leave it out for all kinds")
	f.BoolVar(&taggedImages, "tagged-images", false, "include unused images with tags or digests")
	f.BoolVar(&namedVolumes, "named-volumes", false, "include unused named volumes whose data will be deleted")
	f.IntVar(&limit, "limit", 0, "maximum number of artifacts to include in this plan (0 uses the server and fleet caps)")
	f.StringVar(&summary, "summary", "", "what the change is for, in your words, for Activity")
	f.BoolVarP(&o.yes, "yes", "y", false, "apply without asking")
	f.BoolVar(&o.planOnly, "plan-only", false, "show the full plan and leave it planned, without applying it")
	return cmd
}

func parseCleanupKinds(raw []string) ([]dockwellv1.DockerCleanupKind, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []dockwellv1.DockerCleanupKind
	for _, value := range raw {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(strings.ToLower(part))
			if part == "" {
				continue
			}
			var kind dockwellv1.DockerCleanupKind
			switch part {
			case "image", "images":
				kind = dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE
			case "volume", "volumes":
				kind = dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME
			case "network", "networks":
				kind = dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_NETWORK
			case "cache", "build-cache", "build_cache", "buildcache":
				kind = dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_BUILD_CACHE
			default:
				return nil, fmt.Errorf("%q isn't a Docker cleanup kind. Use images, volumes, networks or build-cache", part)
			}
			if !slices.Contains(out, kind) {
				out = append(out, kind)
			}
		}
	}
	return out, nil
}

func cleanupKindWanted(kinds []dockwellv1.DockerCleanupKind, kind dockwellv1.DockerCleanupKind) bool {
	return len(kinds) == 0 || slices.Contains(kinds, kind)
}

func cleanupPriority(t *dockwellv1.DockerCleanupTarget) int {
	switch t.GetKind() {
	case dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE:
		if t.GetReason() == dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_DANGLING {
			return 0
		}
		return 4
	case dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_BUILD_CACHE:
		return 1
	case dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_NETWORK:
		return 2
	case dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME:
		if t.GetReason() == dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_ANONYMOUS {
			return 3
		}
		return 5
	default:
		return 6
	}
}

func sortCleanupTargetsForBatch(targets []*dockwellv1.DockerCleanupTarget) []*dockwellv1.DockerCleanupTarget {
	ordered := slices.Clone(targets)
	slices.SortStableFunc(ordered, func(a, b *dockwellv1.DockerCleanupTarget) int {
		if pa, pb := cleanupPriority(a), cleanupPriority(b); pa != pb {
			return pa - pb
		}
		ba, bb := a.GetEstimatedBytes(), b.GetEstimatedBytes()
		if ba != bb {
			if ba > bb {
				return -1
			}
			return 1
		}
		if c := strings.Compare(a.GetServerId(), b.GetServerId()); c != 0 {
			return c
		}
		if c := strings.Compare(a.GetEngineId(), b.GetEngineId()); c != 0 {
			return c
		}
		return strings.Compare(a.GetName(), b.GetName())
	})
	return ordered
}

func (r *runner) dockerCleanup(ctx context.Context, serverIDs []string, kinds []dockwellv1.DockerCleanupKind, taggedImages, namedVolumes bool, limit int, summary string) error {
	if err := r.connect(ctx); err != nil {
		return err
	}
	filters := &dockwellv1.DockerCleanupFilters{
		TaggedImages: cleanupKindWanted(kinds, dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE),
		Networks:     cleanupKindWanted(kinds, dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_NETWORK),
		Volumes:      cleanupKindWanted(kinds, dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME),
		Kinds:        kinds,
	}
	preview, err := r.readAllDockerCleanup(ctx, serverIDs, filters)
	if err != nil {
		return r.explain(err)
	}
	snapshot := preview.snapshot
	candidates := preview.candidates
	exclusions := preview.exclusions
	pauses := preview.pauses
	unavailable := preview.unavailable
	for _, p := range pauses {
		fmt.Fprintf(r.env.Err, "Cleanup is paused on %s. Re-enable it in the panel before removing artifacts there.\n", p.GetServerId())
	}
	for _, u := range unavailable {
		if u.GetState() != dockwellv1.DockerReadState_DOCKER_READ_STATE_UNSPECIFIED {
			fmt.Fprintf(r.env.Err, "Server %s couldn't be read (%s).\n", u.GetServerId(), strings.ToLower(word(u.GetState())))
		}
	}
	legacyControlPlane := preview.maxTargetsTotal == 0 && len(preview.serverLimits) == 0
	limitByServer := make(map[string]int, len(preview.serverLimits))
	var unsupportedVolumes []string
	for _, sl := range preview.serverLimits {
		if sl.GetMaxTargets() > 0 {
			limitByServer[sl.GetServerId()] = int(sl.GetMaxTargets())
		}
		if cleanupKindWanted(kinds, dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME) &&
			len(sl.GetSupportedKinds()) > 0 &&
			!slices.Contains(sl.GetSupportedKinds(), dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME) {
			unsupportedVolumes = append(unsupportedVolumes, sl.GetServerId())
		}
	}
	if legacyControlPlane && cleanupKindWanted(kinds, dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME) {
		fmt.Fprintln(r.env.Err, "This control plane didn't report cleanup limits, so volume cleanup is skipped and at most 12 targets are planned. Update the control plane to remove volumes or larger batches.")
	}
	for _, sid := range unsupportedVolumes {
		fmt.Fprintf(r.env.Err, "Server %s needs an updated agent to remove unused Docker volumes.\n", sid)
	}

	var matching []*dockwellv1.DockerCleanupTarget
	var skippedTagged, skippedNamed int
	for _, target := range candidates {
		if !cleanupKindWanted(kinds, target.GetKind()) {
			continue
		}
		if legacyControlPlane && target.GetKind() == dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME {
			continue
		}
		switch {
		case target.GetKind() == dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE &&
			target.GetReason() == dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED:
			if taggedImages {
				matching = append(matching, target)
			} else {
				skippedTagged++
			}
		case target.GetKind() == dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME &&
			target.GetReason() == dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED:
			if namedVolumes {
				matching = append(matching, target)
			} else {
				skippedNamed++
			}
		default:
			matching = append(matching, target)
		}
	}

	totalCap := int(preview.maxTargetsTotal)
	defaultServerCap := defaultCLIServerCap
	if legacyControlPlane {
		totalCap = baselineCLITargetCap
		defaultServerCap = baselineCLITargetCap
	} else if totalCap <= 0 {
		totalCap = defaultCLITotalCap
	}
	if limit > 0 && limit < totalCap {
		totalCap = limit
	}
	var selected, deferred []*dockwellv1.DockerCleanupTarget
	perServerCount := map[string]int{}
	for _, target := range sortCleanupTargetsForBatch(matching) {
		serverCap := limitByServer[target.GetServerId()]
		if serverCap <= 0 {
			serverCap = defaultServerCap
		}
		if len(selected) >= totalCap || perServerCount[target.GetServerId()] >= serverCap {
			deferred = append(deferred, target)
			continue
		}
		selected = append(selected, target)
		perServerCount[target.GetServerId()]++
	}

	if len(selected) == 0 {
		if r.opts.json {
			return JSON(r.env.Out, &dockwellv1.PreviewDockerCleanupResponse{
				Candidates:      candidates,
				Exclusions:      exclusions,
				Snapshot:        snapshot,
				Unavailable:     unavailable,
				Pauses:          pauses,
				ServerLimits:    preview.serverLimits,
				MaxTargetsTotal: preview.maxTargetsTotal,
			}, true)
		}
		fmt.Fprintln(r.env.Out, "No unused Docker artifacts to remove.")
		var optIns []string
		if skippedTagged > 0 {
			optIns = append(optIns, fmt.Sprintf("%d unused tagged %s (--tagged-images)", skippedTagged, pluralWord(skippedTagged, "image", "images")))
		}
		if skippedNamed > 0 {
			optIns = append(optIns, fmt.Sprintf("%d unused named %s (--named-volumes)", skippedNamed, pluralWord(skippedNamed, "volume", "volumes")))
		}
		if len(optIns) > 0 {
			fmt.Fprintf(r.env.Out, "Kept %s.\n", human.List(optIns))
		}
		if len(exclusions) > 0 {
			fmt.Fprintf(r.env.Out, "%d protected %s stayed untouched.\n", len(exclusions), pluralWord(len(exclusions), "artifact", "artifacts"))
		}
		return nil
	}
	planServers := slices.Clone(serverIDs)
	if len(planServers) == 0 {
		for _, t := range selected {
			if !slices.Contains(planServers, t.GetServerId()) {
				planServers = append(planServers, t.GetServerId())
			}
		}
	}
	planReq := &dockwellv1.PlanDockerMaintenanceRequest{
		Summary:          summary,
		ServerIds:        planServers,
		ExpectedSnapshot: snapshot,
		Operation: &dockwellv1.PlanDockerMaintenanceRequest_Cleanup{
			Cleanup: &dockwellv1.DockerCleanupSelection{
				Filters: filters,
				Targets: selected,
			},
		},
	}
	var res *dockwellv1.PlanDockerMaintenanceResponse
	err = r.do(ctx, func() error {
		out, err := r.client.Docker().PlanDockerMaintenance(ctx, connect.NewRequest(planReq))
		if err == nil {
			res = out.Msg
		}
		return err
	})
	if err != nil {
		return r.explain(err)
	}
	if len(deferred) > 0 && !r.opts.json {
		fmt.Fprintf(r.env.Err, "Leaving %d more matching %s for the next cleanup:\n", len(deferred), pluralWord(len(deferred), "artifact", "artifacts"))
		for _, t := range deferred {
			fmt.Fprintf(r.env.Err, "  - %s\n", cleanupTargetLine(t))
		}
	}
	r.cleanupReviewCmd = dockerCleanupReviewCommand(serverIDs, kinds, taggedImages, namedVolumes)
	return r.planned(ctx, res, res.GetChange())
}

type dockerCleanupPreview struct {
	snapshot        string
	candidates      []*dockwellv1.DockerCleanupTarget
	exclusions      []*dockwellv1.DockerCleanupTarget
	pauses          []*dockwellv1.DockerCleanupPause
	unavailable     []*dockwellv1.DockerUnavailable
	serverLimits    []*dockwellv1.DockerCleanupServerLimits
	maxTargetsTotal uint32
}

func (r *runner) readAllDockerCleanup(ctx context.Context, serverIDs []string, filters *dockwellv1.DockerCleanupFilters) (*dockerCleanupPreview, error) {
	deadline := time.Now().Add(90 * time.Second)
	for {
		var first *dockwellv1.PreviewDockerCleanupResponse
		err := r.do(ctx, func() error {
			out, cerr := r.client.Docker().PreviewDockerCleanup(ctx, connect.NewRequest(&dockwellv1.PreviewDockerCleanupRequest{
				ServerIds: serverIDs,
				Filters:   filters,
				PageSize:  250,
			}))
			if cerr == nil {
				first = out.Msg
			}
			return cerr
		})
		if err != nil {
			return nil, err
		}
		measuring := slices.ContainsFunc(first.GetUnavailable(), func(row *dockwellv1.DockerUnavailable) bool {
			return row.GetState() == dockwellv1.DockerReadState_DOCKER_READ_STATE_UNSPECIFIED
		})
		if measuring {
			if time.Now().After(deadline) {
				return nil, errors.New("disk use is still being measured on this server. Try again in a moment")
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Second):
				continue
			}
		}
		out := &dockerCleanupPreview{
			snapshot:        first.GetSnapshot(),
			pauses:          first.GetPauses(),
			unavailable:     first.GetUnavailable(),
			serverLimits:    first.GetServerLimits(),
			maxTargetsTotal: first.GetMaxTargetsTotal(),
			candidates:      append([]*dockwellv1.DockerCleanupTarget(nil), first.GetCandidates()...),
			exclusions:      append([]*dockwellv1.DockerCleanupTarget(nil), first.GetExclusions()...),
		}
		token := first.GetNextPageToken()
		for token != "" {
			var page *dockwellv1.PreviewDockerCleanupResponse
			err = r.do(ctx, func() error {
				res, cerr := r.client.Docker().PreviewDockerCleanup(ctx, connect.NewRequest(&dockwellv1.PreviewDockerCleanupRequest{
					ServerIds: serverIDs,
					Filters:   filters,
					PageSize:  250,
					PageToken: token,
				}))
				if cerr == nil {
					page = res.Msg
				}
				return cerr
			})
			if err != nil {
				return nil, err
			}
			out.candidates = append(out.candidates, page.GetCandidates()...)
			out.exclusions = append(out.exclusions, page.GetExclusions()...)
			token = page.GetNextPageToken()
		}
		return out, nil
	}
}

func pluralWord(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}

func dockerMaintenanceWords(m *dockwellv1.DockerMaintenance, server string, full bool) []string {
	if m.GetResumeCleanupToken() != "" {
		return []string{"Re-enable Docker cleanup on " + server + " after a safety pause."}
	}
	targets := m.GetTargets()
	if len(targets) == 0 {
		return nil
	}
	var total uint64
	var hasBytes bool
	for _, t := range targets {
		if t.EstimatedBytes != nil {
			total += t.GetEstimatedBytes()
			hasBytes = true
		}
	}
	head := fmt.Sprintf("Remove %d Docker %s on %s", len(targets), pluralWord(len(targets), "artifact", "artifacts"), server)
	if hasBytes {
		head += fmt.Sprintf(" (%s estimated)", Bytes(total))
	}
	lines := []string{head + ":"}
	const previewLimit = 25
	shown := len(targets)
	if !full && shown > previewLimit {
		shown = previewLimit
	}
	for _, t := range targets[:shown] {
		lines = append(lines, "  - "+cleanupTargetLine(t))
	}
	if len(targets) > shown {
		hidden := targets[shown:]
		hiddenNamed := 0
		for _, t := range hidden {
			if t.GetKind() == dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME &&
				t.GetReason() == dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED {
				hiddenNamed++
			}
		}
		if hiddenNamed > 0 {
			lines = append(lines, fmt.Sprintf("  ... and %d more (including %d named %s whose data will be deleted; run with --plan-only or --json to list every target)",
				len(hidden), hiddenNamed, pluralWord(hiddenNamed, "volume", "volumes")))
		} else {
			lines = append(lines, fmt.Sprintf("  ... and %d more (run with --plan-only or --json to list every target)", len(hidden)))
		}
	}
	return lines
}

func cleanupTargetName(t *dockwellv1.DockerCleanupTarget) string {
	name := t.GetName()
	if t.GetKind() == dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE &&
		len(t.GetImageTags()) == 0 && (name == "" || name == t.GetEngineId()) {
		if id := t.GetEngineId(); len(id) >= 19 {
			return id[7:19]
		}
	}
	if name == "" {
		return t.GetEngineId()
	}
	return name
}

func cleanupTargetLine(t *dockwellv1.DockerCleanupTarget) string {
	kind := "artifact"
	switch t.GetKind() {
	case dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE:
		kind = "image"
	case dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_BUILD_CACHE:
		kind = "build cache"
	case dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_NETWORK:
		kind = "network"
	case dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME:
		kind = "volume"
	default:
	}
	name := cleanupTargetName(t)
	var meta []string
	switch {
	case t.GetKind() == dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME &&
		t.GetReason() == dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_ANONYMOUS:
		meta = append(meta, "anonymous")
	case t.GetKind() == dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME &&
		t.GetReason() == dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED:
		meta = append(meta, "named, data is deleted")
	case t.GetReason() == dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_DANGLING:
		meta = append(meta, "dangling")
	case t.GetReason() == dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED:
		meta = append(meta, "unused")
	case t.GetReason() == dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_IDLE:
		meta = append(meta, "idle")
	}
	if t.EstimatedBytes != nil {
		meta = append(meta, Bytes(t.GetEstimatedBytes()))
	}
	if len(meta) == 0 {
		return fmt.Sprintf("%s %s", kind, name)
	}
	return fmt.Sprintf("%s %s (%s)", kind, name, strings.Join(meta, ", "))
}

func dockerReceiptReasonWords(r *dockwellv1.DockerReceipt) string {
	switch r.GetReason() {
	case dockwellv1.DockerReceiptReason_DOCKER_RECEIPT_REASON_IN_USE:
		return "now in use"
	case dockwellv1.DockerReceiptReason_DOCKER_RECEIPT_REASON_GONE:
		return "already gone"
	case dockwellv1.DockerReceiptReason_DOCKER_RECEIPT_REASON_REFERENCE_MOVED:
		return "reference moved"
	case dockwellv1.DockerReceiptReason_DOCKER_RECEIPT_REASON_NEWER:
		return "newer than preview"
	case dockwellv1.DockerReceiptReason_DOCKER_RECEIPT_REASON_PROTECTED:
		return "now protected"
	case dockwellv1.DockerReceiptReason_DOCKER_RECEIPT_REASON_IMAGE_ID_REFUSED:
		return "Docker refused to remove this image by ID"
	case dockwellv1.DockerReceiptReason_DOCKER_RECEIPT_REASON_EXTRA_CACHE_REMOVED:
		return "Docker removed other build cache records"
	case dockwellv1.DockerReceiptReason_DOCKER_RECEIPT_REASON_CACHE_UNVERIFIED:
		return "build cache removal could not be verified"
	case dockwellv1.DockerReceiptReason_DOCKER_RECEIPT_REASON_VOLUME_UNVERIFIED:
		return "volume removal could not be verified"
	case dockwellv1.DockerReceiptReason_DOCKER_RECEIPT_REASON_VOLUME_REPLACED:
		return "a different volume now uses this name"
	default:
	}
	switch r.GetState() {
	case dockwellv1.DockerReceiptState_DOCKER_RECEIPT_STATE_CHANGED:
		return "changed since you reviewed"
	case dockwellv1.DockerReceiptState_DOCKER_RECEIPT_STATE_FAILED:
		return "failed"
	case dockwellv1.DockerReceiptState_DOCKER_RECEIPT_STATE_UNKNOWN:
		return "outcome unknown"
	default:
		return ""
	}
}

func cleanupKindFlagName(kind dockwellv1.DockerCleanupKind) string {
	switch kind {
	case dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE:
		return "images"
	case dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME:
		return "volumes"
	case dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_NETWORK:
		return "networks"
	case dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_BUILD_CACHE:
		return "build-cache"
	default:
		return ""
	}
}

func cleanupFlagArg(value string) string {
	if value == "" || strings.ContainsAny(value, " \t\n\r\"'\\") {
		return fmt.Sprintf("%q", value)
	}
	return value
}

func dockerCleanupReviewCommand(servers []string, kinds []dockwellv1.DockerCleanupKind, taggedImages, namedVolumes bool) string {
	parts := []string{"dockwell docker cleanup"}
	for _, s := range servers {
		if s != "" {
			parts = append(parts, "--server", cleanupFlagArg(s))
		}
	}
	for _, k := range kinds {
		if name := cleanupKindFlagName(k); name != "" {
			parts = append(parts, "--kind", name)
		}
	}
	if taggedImages {
		parts = append(parts, "--tagged-images")
	}
	if namedVolumes {
		parts = append(parts, "--named-volumes")
	}
	return strings.Join(parts, " ")
}

func dockerCleanupReviewCommandForChange(c *dockwellv1.Change, receipts []*dockwellv1.DockerReceipt) string {
	serverNames := map[string]string{}
	var plannedKinds []dockwellv1.DockerCleanupKind
	var taggedImages, namedVolumes bool
	for _, sp := range c.GetPlan().GetServers() {
		if sp.GetServerId() != "" && sp.GetServerName() != "" {
			serverNames[sp.GetServerId()] = sp.GetServerName()
		}
		for _, step := range sp.GetSteps() {
			for _, target := range step.GetDockerMaintenance().GetTargets() {
				if kind := target.GetKind(); cleanupKindFlagName(kind) != "" && !slices.Contains(plannedKinds, kind) {
					plannedKinds = append(plannedKinds, kind)
				}
				if target.GetKind() == dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE &&
					target.GetReason() == dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED {
					taggedImages = true
				}
				if target.GetKind() == dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME &&
					target.GetReason() == dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED {
					namedVolumes = true
				}
			}
		}
	}
	var servers []string
	var receiptKinds []dockwellv1.DockerCleanupKind
	for _, r := range receipts {
		target := r.GetTarget()
		if kind := target.GetKind(); cleanupKindFlagName(kind) != "" && !slices.Contains(receiptKinds, kind) {
			receiptKinds = append(receiptKinds, kind)
		}
		if r.GetState() == dockwellv1.DockerReceiptState_DOCKER_RECEIPT_STATE_CHANGED {
			sid := target.GetServerId()
			label := serverNames[sid]
			if label == "" {
				label = sid
			}
			if label != "" && !slices.Contains(servers, label) {
				servers = append(servers, label)
			}
		}
		if target.GetKind() == dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE &&
			target.GetReason() == dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED {
			taggedImages = true
		}
		if target.GetKind() == dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME &&
			target.GetReason() == dockwellv1.DockerCleanupReason_DOCKER_CLEANUP_REASON_UNUSED {
			namedVolumes = true
		}
	}
	kinds := plannedKinds
	if len(kinds) == 0 {
		kinds = receiptKinds
	}
	// All four kinds means no --kind filter was needed.
	if len(kinds) >= 4 {
		kinds = nil
	} else {
		ordered := make([]dockwellv1.DockerCleanupKind, 0, len(kinds))
		for _, k := range []dockwellv1.DockerCleanupKind{
			dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_IMAGE,
			dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_VOLUME,
			dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_NETWORK,
			dockwellv1.DockerCleanupKind_DOCKER_CLEANUP_KIND_BUILD_CACHE,
		} {
			if slices.Contains(kinds, k) {
				ordered = append(ordered, k)
			}
		}
		kinds = ordered
	}
	return dockerCleanupReviewCommand(servers, kinds, taggedImages, namedVolumes)
}

func dockerCleanupOutcomeLines(receipts []*dockwellv1.DockerReceipt, reviewCmd string) []string {
	if len(receipts) == 0 {
		return nil
	}
	var removed, changed, failed, unknown int
	var notable []*dockwellv1.DockerReceipt
	for _, r := range receipts {
		switch r.GetState() {
		case dockwellv1.DockerReceiptState_DOCKER_RECEIPT_STATE_REMOVED:
			removed++
		case dockwellv1.DockerReceiptState_DOCKER_RECEIPT_STATE_CHANGED:
			changed++
			notable = append(notable, r)
		case dockwellv1.DockerReceiptState_DOCKER_RECEIPT_STATE_FAILED:
			failed++
			notable = append(notable, r)
		case dockwellv1.DockerReceiptState_DOCKER_RECEIPT_STATE_UNKNOWN:
			unknown++
			notable = append(notable, r)
		default:
		}
	}
	var parts []string
	if removed > 0 || changed > 0 {
		parts = append(parts, fmt.Sprintf("%d removed", removed))
	}
	if changed > 0 {
		parts = append(parts, fmt.Sprintf("%d changed since you reviewed", changed))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	if unknown > 0 {
		parts = append(parts, fmt.Sprintf("%d outcome unknown", unknown))
	}
	if len(parts) == 0 {
		return nil
	}
	summary := strings.Join(parts, " · ")
	if changed > 0 {
		if reviewCmd == "" {
			reviewCmd = dockerCleanupReviewCommandForChange(nil, receipts)
		}
		summary += fmt.Sprintf(". Run %s to review again.", reviewCmd)
	} else {
		summary += "."
	}
	lines := []string{summary}
	const detailLimit = 25
	shown := min(len(notable), detailLimit)
	for _, r := range notable[:shown] {
		lines = append(lines, fmt.Sprintf("  - %s: %s", cleanupTargetName(r.GetTarget()), dockerReceiptReasonWords(r)))
	}
	if len(notable) > shown {
		lines = append(lines, fmt.Sprintf("  ... and %d more", len(notable)-shown))
	}
	return lines
}
