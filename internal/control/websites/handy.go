// SPDX-License-Identifier: AGPL-3.0-only

package websites

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/control/place"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/ingress"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/backuppolicy"
	"github.com/dockwell/dockwell/internal/resource/network"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/resource/route/routehash"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/website"
)

// validateCustomCaddyOnServer validates a website's custom Caddy directives using Caddy's
// own adapter on the server that runs (or will run) the website.
func (h *Handler) validateCustomCaddyOnServer(ctx context.Context, serverID string, spec *resourcev1.Website) (bool, error) {
	if strings.TrimSpace(spec.GetCustomCaddy()) == "" {
		return false, nil
	}
	if serverID == "" {
		if h.agents != nil {
			if connected := h.agents.Connected(); len(connected) == 1 {
				serverID = connected[0]
			}
		}
	}
	if serverID == "" || h.agents == nil {
		return false, authapi.Invalid(errors.New("custom Caddy config must be validated by Caddy on the target server before applying, and no server was selected or connected"))
	}
	session, ok := h.agents.Session(serverID)
	if !ok {
		return false, connect.NewError(connect.CodeUnavailable, errors.New("the website's server isn't connected right now, so Caddy can't validate the custom config"))
	}
	// Always validate the unpaused config so custom directives edited while a website is
	// paused are still compiled and verified by Caddy's adapter (M2).
	unpaused := proto.CloneOf(spec)
	unpaused.Paused = false
	cfg := website.Config(unpaused)
	res, err := session.ValidateCaddyfile(ctx, cfg, website.AllowedRoot(unpaused))
	if err != nil {
		if errors.Is(err, agentapi.ErrOlderAgent) {
			return false, connect.NewError(connect.CodeFailedPrecondition, errors.New("update the server's agent to validate custom Caddy config"))
		}
		return false, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("caddy couldn't validate the custom config: %w", err))
	}
	if !res.GetValid() {
		return false, authapi.Invalid(fmt.Errorf("caddy rejected the custom config: %s", cmp.Or(res.GetError(), "invalid Caddyfile syntax")))
	}
	return true, nil
}

// prepareBasicAuthSecrets hashes any newly provided Basic Auth user passwords, prepares
// Dockwell secret edits in env (password secret + bcrypt hash secret), and deletes
// only managed Basic Auth secrets belonging to previous Basic Auth users that were removed.
func prepareBasicAuthSecrets(env string, prev, spec *resourcev1.Website, passwords map[string]string, existingSecrets []store.Resource) ([]*dockwellv1.Edit, error) {
	existing := map[string]bool{}
	for i := range existingSecrets {
		existing[existingSecrets[i].Name] = true
	}
	managed := map[string]bool{}
	if prevBA := prev.GetRules().GetBasicAuth(); prevBA != nil {
		for _, u := range prevBA.GetUsers() {
			if u.GetUsername() != "" {
				managed[website.BasicAuthPasswordSecret(u.GetUsername())] = true
				managed[website.BasicAuthHashSecret(u.GetUsername())] = true
			}
			if u.GetSecret() != "" {
				managed[u.GetSecret()] = true
			}
		}
	}
	keep := map[string]bool{}
	var edits []*dockwellv1.Edit
	if ba := spec.GetRules().GetBasicAuth(); ba != nil {
		for _, u := range ba.GetUsers() {
			pwSecret := website.BasicAuthPasswordSecret(u.GetUsername())
			hashSecret := website.BasicAuthHashSecret(u.GetUsername())
			keep[pwSecret] = true
			keep[hashSecret] = true
			if pw := passwords[u.GetUsername()]; pw != "" {
				if len(pw) > 72 {
					return nil, fmt.Errorf("password for %q is longer than bcrypt's 72-byte limit. Use a shorter password", u.GetUsername())
				}
				hash, err := routehash.Hash(pw)
				if err != nil {
					return nil, fmt.Errorf("can't hash Basic Auth password for %s: %w", u.GetUsername(), err)
				}
				edits = append(edits,
					&dockwellv1.Edit{
						Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: env, Name: pwSecret},
						Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: pw}}},
					},
					&dockwellv1.Edit{
						Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: env, Name: hashSecret},
						Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: &resourcev1.Secret{Value: hash}}},
					},
				)
			} else if !existing[pwSecret] || !existing[hashSecret] {
				return nil, fmt.Errorf("basic auth user %q needs a password", u.GetUsername())
			}
			u.Secret = pwSecret
		}
	}
	for i := range existingSecrets {
		name := existingSecrets[i].Name
		if managed[name] && !keep[name] {
			edits = append(edits, &dockwellv1.Edit{
				Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: env, Name: name},
				Delete:   true,
			})
		}
	}
	return edits, nil
}

// resolveSubpathMembers resolves any sub-path proxy targeting a Dockwell service_id
// into NetworkMember entries on the website's SiteNetwork, checking developer/admin
// project access and server placement.
func (h *Handler) resolveSubpathMembers(ctx context.Context, tx *store.Tx, tree scope.Tree, spec *resourcev1.Website, siteServers []string) ([]*resourcev1.NetworkMember, error) {
	proxies := spec.GetRules().GetSubpathProxies()
	if len(proxies) == 0 {
		return nil, nil
	}
	needsLookup := false
	for _, sp := range proxies {
		if sp.GetServiceId() != "" {
			needsLookup = true
			break
		}
	}
	if !needsLookup {
		return nil, nil
	}
	type svcInfo struct {
		projectID string
		envID     string
		protected bool
		name      string
		placed    []string
	}
	byID := map[string]svcInfo{}
	for _, p := range tree.Projects {
		for _, e := range p.Environments {
			svcs, err := tx.ListResources(ctx, e.ID, string(service.Kind))
			if err != nil {
				return nil, err
			}
			for i := range svcs {
				placed, err := tx.Placement(ctx, svcs[i].ID, svcs[i].Revision)
				if err != nil {
					return nil, err
				}
				byID[svcs[i].ID] = svcInfo{projectID: p.ID, envID: e.ID, protected: e.Protected, name: svcs[i].Name, placed: placed}
			}
		}
	}
	var members []*resourcev1.NetworkMember
	for i, sp := range proxies {
		if sp.GetServiceId() == "" {
			continue
		}
		target, ok := byID[sp.GetServiceId()]
		if !ok {
			return nil, fmt.Errorf("sub-path proxy %s names service %s, which doesn't exist or isn't in a project you can access", sp.GetPath(), sp.GetServiceId())
		}
		if c, hasCaller := place.CallerFrom(ctx); hasCaller {
			held := place.Holds(c.Grants, place.Place{Project: target.projectID, Environment: target.envID, Protected: target.protected})
			if held == "" {
				return nil, fmt.Errorf("sub-path proxy %s names service %s, which doesn't exist or isn't in a project you can access", sp.GetPath(), sp.GetServiceId())
			}
			need := store.RoleDeveloper
			if target.protected {
				need = store.RoleAdmin
			}
			if store.RoleRank(held) < store.RoleRank(need) {
				return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("sub-path proxy %s targets %s, which needs %s access in its project", sp.GetPath(), target.name, need))
			}
		}
		if len(siteServers) > 0 && len(target.placed) > 0 {
			shared := slices.ContainsFunc(siteServers, func(s string) bool { return slices.Contains(target.placed, s) })
			if !shared {
				return nil, fmt.Errorf("sub-path proxy %s targets %s, which runs on a different server than this website", sp.GetPath(), target.name)
			}
		}
		members = append(members, &resourcev1.NetworkMember{
			Environment: target.envID,
			Service:     target.name,
			Alias:       website.SubpathAlias(i),
		})
	}
	return members, nil
}

// visitorNotes summarizes what changes for visitors in plain language on the plan.
func visitorNotes(env string, was, is *resourcev1.Website) []revision.Note {
	var out []revision.Note
	ref := resource.Ref{Kind: service.Kind, Scope: env, Name: website.WebService}
	routeRef := resource.Ref{Kind: route.Kind, Scope: env, Name: website.RouteName}

	// Pause / resume.
	if is.GetPaused() && (was == nil || !was.GetPaused() || was.GetPauseMode() != is.GetPauseMode()) {
		msg := "Visitors see a 503 maintenance page while this website is paused. Its domains, certificate and files stay."
		if is.GetPauseMode() == resourcev1.WebsitePauseMode_WEBSITE_PAUSE_MODE_PLAIN_503 {
			msg = "Visitors see a plain 503 Service Unavailable response while this website is paused. Its domains, certificate and files stay."
		}
		out = append(out, revision.Note{Reason: "website_paused", Message: msg, Ref: ref})
	} else if was != nil && was.GetPaused() && !is.GetPaused() {
		out = append(out, revision.Note{Reason: "website_resumed", Message: "This website resumes serving visitors normally.", Ref: ref})
	}

	// Rules changes (skip while paused).
	if !is.GetPaused() {
		wasR, isR := was.GetRules(), is.GetRules()
		if !proto.Equal(wasR.GetBasicAuth(), isR.GetBasicAuth()) {
			if n := len(isR.GetBasicAuth().GetUsers()); n > 0 {
				where := "the whole website"
				if p := isR.GetBasicAuth().GetPath(); p != "" {
					where = p
				}
				out = append(out, revision.Note{
					Reason:  "website_basic_auth",
					Message: fmt.Sprintf("Visitors must sign in with Basic Auth (%d user(s)) to access %s. Passwords are stored as Dockwell secrets and hashed with bcrypt.", n, where),
					Ref:     ref,
				})
			} else if len(wasR.GetBasicAuth().GetUsers()) > 0 {
				out = append(out, revision.Note{Reason: "website_basic_auth", Message: "Basic Auth is removed; visitors no longer need a username and password.", Ref: ref})
			}
		}
		if !proto.Equal(wasR.GetCors(), isR.GetCors()) {
			if isR.GetCors().GetEnabled() {
				out = append(out, revision.Note{
					Reason:  "website_cors",
					Message: fmt.Sprintf("Cross-origin requests (CORS) are allowed from %s.", strings.Join(isR.GetCors().GetAllowedOrigins(), ", ")),
					Ref:     ref,
				})
			} else if wasR.GetCors().GetEnabled() {
				out = append(out, revision.Note{Reason: "website_cors", Message: "CORS response headers are turned off.", Ref: ref})
			}
		}
		if !proto.Equal(wasR.GetRateLimit(), isR.GetRateLimit()) {
			if isR.GetRateLimit().GetEnabled() {
				rpm := cmp.Or(isR.GetRateLimit().GetRequestsPerMinute(), uint32(website.DefaultRateLimitRPM))
				out = append(out, revision.Note{
					Reason:  "website_rate_limit",
					Message: fmt.Sprintf("Each visitor address is limited to %d requests/min (burst %d); excess requests receive 429 Too Many Requests.", rpm, isR.GetRateLimit().GetBurst()),
					Ref:     routeRef,
				})
			} else if wasR.GetRateLimit().GetEnabled() {
				out = append(out, revision.Note{Reason: "website_rate_limit", Message: "Per-address rate limiting is turned off.", Ref: routeRef})
			}
		}
		if !slices.EqualFunc(wasR.GetPathRules(), isR.GetPathRules(), func(a, b *resourcev1.WebsitePathRule) bool { return proto.Equal(a, b) }) {
			if n := len(isR.GetPathRules()); n > 0 {
				out = append(out, revision.Note{
					Reason:  "website_path_rules",
					Message: fmt.Sprintf("Applies %d path redirect and rewrite rule(s) for visitors.", n),
					Ref:     ref,
				})
			} else if len(wasR.GetPathRules()) > 0 {
				out = append(out, revision.Note{Reason: "website_path_rules", Message: "Path redirects and rewrites are cleared.", Ref: ref})
			}
		}
		if !slices.EqualFunc(wasR.GetSubpathProxies(), isR.GetSubpathProxies(), func(a, b *resourcev1.WebsiteSubpathProxy) bool { return proto.Equal(a, b) }) {
			if n := len(isR.GetSubpathProxies()); n > 0 {
				paths := make([]string, n)
				for i, sp := range isR.GetSubpathProxies() {
					paths[i] = sp.GetPath()
				}
				out = append(out, revision.Note{
					Reason:  "website_subpath_proxies",
					Message: fmt.Sprintf("Routes %s to separate upstream app(s) or address(es).", strings.Join(paths, ", ")),
					Ref:     ref,
				})
			} else if len(wasR.GetSubpathProxies()) > 0 {
				out = append(out, revision.Note{Reason: "website_subpath_proxies", Message: "Sub-path proxies are removed.", Ref: ref})
			}
		}
		if !slices.EqualFunc(wasR.GetHeaders(), isR.GetHeaders(), func(a, b *resourcev1.WebsiteHeader) bool { return proto.Equal(a, b) }) {
			if n := len(isR.GetHeaders()); n > 0 {
				out = append(out, revision.Note{
					Reason:  "website_headers",
					Message: fmt.Sprintf("Adds or removes %d custom HTTP response header(s).", n),
					Ref:     ref,
				})
			} else if len(wasR.GetHeaders()) > 0 {
				out = append(out, revision.Note{Reason: "website_headers", Message: "Extra HTTP response headers are cleared.", Ref: ref})
			}
		}
		if !proto.Equal(wasR.GetStaticCache(), isR.GetStaticCache()) {
			if isR.GetStaticCache().GetEnabled() {
				age := cmp.Or(isR.GetStaticCache().GetMaxAgeSeconds(), uint32(website.DefaultStaticCacheMaxAge))
				out = append(out, revision.Note{
					Reason:  "website_static_cache",
					Message: fmt.Sprintf("Static files are cached by browsers with max-age=%d s.", age),
					Ref:     ref,
				})
			} else if wasR.GetStaticCache().GetEnabled() {
				out = append(out, revision.Note{Reason: "website_static_cache", Message: "Static file Cache-Control headers are turned off.", Ref: ref})
			}
		}
		if strings.TrimSpace(was.GetCustomCaddy()) != strings.TrimSpace(is.GetCustomCaddy()) {
			if strings.TrimSpace(is.GetCustomCaddy()) != "" {
				out = append(out, revision.Note{
					Reason:  "website_custom_caddy",
					Message: "Applies custom Caddy directives scoped to this website, validated by Caddy's adapter on this server.",
					Ref:     ref,
				})
			} else {
				out = append(out, revision.Note{Reason: "website_custom_caddy", Message: "Custom Caddy directives are removed.", Ref: ref})
			}
		}
	}

	// PHP tuning / extension changes.
	if p := is.GetPhp(); p != nil && !proto.Equal(was.GetPhp(), p) {
		if p.GetMemoryLimitMb() != 0 || p.GetMaxExecutionTimeS() != 0 || p.GetPostMaxSizeMb() != 0 || p.GetFpmMaxChildren() != 0 || len(p.GetDisabledExtensions()) > 0 {
			mem := cmp.Or(p.GetMemoryLimitMb(), uint32(website.DefaultPHPMemoryLimitMB))
			execS := cmp.Or(p.GetMaxExecutionTimeS(), uint32(website.DefaultPHPMaxExecutionTimeS))
			workers := cmp.Or(p.GetFpmMaxChildren(), uint32(website.DefaultPHPFPMMaxChildren))
			msg := fmt.Sprintf("PHP %s runs with %d MiB memory_limit, %d s max_execution_time, and up to %d FPM workers.", p.GetVersion(), mem, execS, workers)
			if len(p.GetDisabledExtensions()) > 0 {
				msg += fmt.Sprintf(" Disabled extensions: %s.", strings.Join(p.GetDisabledExtensions(), ", "))
			}
			out = append(out, revision.Note{Reason: "website_php_settings", Message: msg, Ref: resource.Ref{Kind: service.Kind, Scope: env, Name: website.PHPService}})
		}
	}
	return out
}

// BatchUpdateWebsites plans changing pause state, PHP version, or HTTPS across up to
// 50 websites as one change and plan.
func (h *Handler) BatchUpdateWebsites(ctx context.Context, req *connect.Request[dockwellv1.BatchUpdateWebsitesRequest]) (*connect.Response[dockwellv1.BatchUpdateWebsitesResponse], error) {
	m := req.Msg
	ids := m.GetWebsiteIds()
	if len(ids) == 0 {
		return nil, authapi.Invalid(errors.New("select at least one website"))
	}
	if len(ids) > 50 {
		return nil, authapi.Invalid(errors.New("select up to 50 websites"))
	}
	phpVersion := strings.TrimSpace(m.GetPhpVersion())
	if m.Paused == nil && phpVersion == "" && m.Https == nil {
		return nil, authapi.Invalid(errors.New("choose at least one change: pause/resume, PHP version, or HTTPS"))
	}
	if phpVersion != "" {
		if _, ok := website.PHPRuntime(phpVersion); !ok {
			return nil, authapi.Invalid(fmt.Errorf("PHP %s isn't one Dockwell offers", phpVersion))
		}
	}
	switch m.GetPauseMode() {
	case resourcev1.WebsitePauseMode_WEBSITE_PAUSE_MODE_UNSPECIFIED,
		resourcev1.WebsitePauseMode_WEBSITE_PAUSE_MODE_MAINTENANCE,
		resourcev1.WebsitePauseMode_WEBSITE_PAUSE_MODE_PLAIN_503:
	default:
		return nil, authapi.Invalid(errors.New("pause mode must be maintenance page or plain 503"))
	}

	var selected []*site
	var tree scope.Tree
	var subpathsBySite map[string][]*resourcev1.NetworkMember
	err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		if tree, err = scope.Load(ctx, tx); err != nil {
			return err
		}
		all, err := h.sites(ctx, tx)
		if err != nil {
			return err
		}
		byID := make(map[string]*site, len(all))
		for i := range all {
			byID[all[i].project.ID] = &all[i]
		}
		seen := map[string]bool{}
		subpathsBySite = map[string][]*resourcev1.NetworkMember{}
		for _, id := range ids {
			item, ok := byID[id]
			if !ok {
				return siteNotFound(id)
			}
			if seen[id] {
				return authapi.Invalid(fmt.Errorf("website %s is listed twice. Select it once", id))
			}
			seen[id] = true
			if item.route == nil {
				return authapi.ReasonError(connect.CodeFailedPrecondition,
					fmt.Errorf("%s has no route left to update", item.project.Name), "no_route")
			}
			if phpVersion != "" && item.spec.GetWebsite().GetPhp() == nil {
				return authapi.Invalid(fmt.Errorf("%s isn't a PHP website, so its PHP version can't be switched", cmp.Or(item.rt.GetDomain(), item.project.Name)))
			}
			members, err := h.resolveSubpathMembers(ctx, tx, tree, item.spec.GetWebsite(), item.placed)
			if err != nil {
				var cerr *connect.Error
				if errors.As(err, &cerr) {
					return cerr
				}
				return authapi.Invalid(err)
			}
			subpathsBySite[id] = members
			selected = append(selected, item)
		}
		return nil
	})
	var rpcErr *connect.Error
	if errors.As(err, &rpcErr) {
		return nil, rpcErr
	}
	if err != nil {
		return nil, h.internal(ctx, err)
	}

	var edits []*dockwellv1.Edit
	var notes []revision.Note
	for _, s := range selected {
		was := s.spec.GetWebsite()
		next := proto.CloneOf(was)
		if m.Paused != nil {
			next.Paused = m.GetPaused()
			if m.GetPauseMode() != resourcev1.WebsitePauseMode_WEBSITE_PAUSE_MODE_UNSPECIFIED {
				next.PauseMode = m.GetPauseMode()
			}
		}
		if phpVersion != "" && next.GetPhp() != nil {
			next.GetPhp().Version = phpVersion
		}
		if err := website.Check(next); err != nil {
			return nil, authapi.Invalid(err)
		}
		// Run Caddy's adapter check on every site with custom_caddy before applying or resuming (M2).
		if strings.TrimSpace(next.GetCustomCaddy()) != "" {
			var targetServer string
			if len(s.placed) > 0 {
				targetServer = s.placed[0]
			}
			if _, err := h.validateCustomCaddyOnServer(ctx, targetServer, next); err != nil {
				return nil, err
			}
		}
		front := website.FrontOf(s.rt)
		front.SubpathMembers = subpathsBySite[s.project.ID]
		if m.Https != nil {
			if m.GetHttps() {
				front.TLS = ""
				front.PlainForAnyone = false
			} else {
				front.TLS = ingress.TLSOff
				front.Proof = ""
				front.CustomCert = nil
			}
		}
		domain := s.rt.GetDomain()
		serving := website.Resources(next, domain, front)
		if m.Https == nil {
			keepRedirectFronts(serving, s)
		}
		keepEdits(serving, s)

		edits = append(edits, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(scope.ProjectKind), Scope: s.org, Name: s.project.Name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Project{Project: &resourcev1.Project{DisplayName: s.spec.GetDisplayName(), Catalog: s.spec.GetCatalog(), Website: next}}},
		})
		edits = append(edits, puts(s.env, serving, s.placed, redirectNames(serving, s.redirects, s.routes))...)
		for _, r := range s.networks {
			if serving.Networks[r.Name] == nil {
				edits = append(edits, &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(network.Kind), Scope: s.env, Name: r.Name}, Delete: true})
			}
		}
		for _, r := range s.services {
			if serving.Services[r.Name] == nil {
				edits = append(edits, &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(service.Kind), Scope: s.env, Name: r.Name}, Delete: true})
			}
		}
		notes = append(notes, visitorNotes(s.env, was, next)...)
	}

	summary := strings.TrimSpace(m.GetSummary())
	if summary == "" {
		var parts []string
		if m.Paused != nil {
			if m.GetPaused() {
				parts = append(parts, "Pause")
			} else {
				parts = append(parts, "Resume")
			}
		}
		if phpVersion != "" {
			parts = append(parts, "switch to PHP "+phpVersion)
		}
		if m.Https != nil {
			if m.GetHttps() {
				parts = append(parts, "enable HTTPS")
			} else {
				parts = append(parts, "disable HTTPS")
			}
		}
		summary = fmt.Sprintf("%s on %d website(s)", strings.Join(parts, ", "), len(selected))
	}
	c, err := h.changes.Propose(ctx, summary, edits, serversField, changes.Also{
		IfNothing:                   fault.New(revision.ErrInvalid, "selected websites already match those settings"),
		Notes:                       notes,
		WebsiteCustomCaddyValidated: true,
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.BatchUpdateWebsitesResponse{Change: c}), nil
}

// deleteSiteExtras removes managed Basic Auth secrets and backup policies when deleting a website.
func deleteSiteExtras(s *site) []*dockwellv1.Edit {
	var edits []*dockwellv1.Edit
	for _, r := range s.secrets {
		edits = append(edits, &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: s.env, Name: r.Name}, Delete: true})
	}
	for _, r := range s.policies {
		edits = append(edits, &dockwellv1.Edit{Resource: &dockwellv1.ResourceRef{Kind: string(backuppolicy.Kind), Scope: s.env, Name: r.Name}, Delete: true})
	}
	return edits
}
