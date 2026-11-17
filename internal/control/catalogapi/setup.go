// SPDX-License-Identifier: AGPL-3.0-only

package catalogapi

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

// SetupFor is how long a setup link works (ADR 0105).
const SetupFor = time.Hour

// NoteSetupLink is the note of the change that writes a setup link.
const NoteSetupLink = "setup_link"

// StepUps confirms a person's passkey, as auth.Service does.
type StepUps interface {
	HoldStepUp(ctx context.Context, client auth.Client, p auth.Principal, proof string, action auth.StepUpAction, target string) (func(), error)
	AskedForStepUp(p auth.Principal, action auth.StepUpAction, target string, missed func())
}

// SetStepUps has the handler confirm a person's passkey with s, for a setup link.
func (h *Handler) SetStepUps(s StepUps) { h.stepUps = s }

// heldRoute is one of an install's held routes, as it's stored.
type heldRoute struct {
	ref  resource.Ref
	spec *resourcev1.Route
}

// SetUpCatalogInstall gives a held install's domain a setup link: a key, good once
// and for an hour, whose hash it writes on the install's held routes of the domain,
// as a change it applies at once, as the person asking. The key goes back to them
// alone, in the address, and is on no change (ADR 0105).
func (h *Handler) SetUpCatalogInstall(ctx context.Context, req *connect.Request[dockwellv1.SetUpCatalogInstallRequest]) (*connect.Response[dockwellv1.SetUpCatalogInstallResponse], error) {
	if h.stepUps == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("this control plane makes no setup links"))
	}
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok || p.Session.ID == "" {
		return nil, authapi.ReasonError(connect.CodePermissionDenied, errors.New("a setup link is for a person signed in to the panel, whose browser opens it, not for an API token"), "needs_session")
	}
	s, err := h.catalog(ctx)
	if err != nil {
		return nil, err
	}
	found, err := h.find(ctx, s, req.Msg.GetId())
	if err != nil {
		return nil, err
	}
	install := found.install
	if !install.GetHeld() {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s isn't held, so it needs no setup link: open it at its domain", install.GetProject()), "not_held")
	}
	routes, err := h.heldRoutes(ctx, install.GetEnvironmentId())
	if err != nil {
		return nil, h.internal(ctx, err)
	}
	domain := strings.ToLower(strings.TrimSpace(req.Msg.GetDomain()))
	if domain == "" && len(routes) > 0 {
		domain = routes[0].spec.GetDomain()
	}
	routes = slices.DeleteFunc(routes, func(r heldRoute) bool { return !strings.EqualFold(r.spec.GetDomain(), domain) })
	if len(routes) == 0 {
		if domain == "" {
			return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s has no domain yet, so there's nothing to set it up at. Add a domain to it, or open it through the panel", install.GetProject()), "no_domain")
		}
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, fmt.Errorf("%s isn't one of %s's held domains. Choose one it has", domain, install.GetProject()), "no_domain")
	}
	// A Secure cookie isn't kept over plain HTTP, so a domain no browser reaches over
	// HTTPS gets no link (ADR 0105).
	if slices.ContainsFunc(routes, func(r heldRoute) bool { return !overHTTPS(r.spec) }) {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s answers over plain HTTP, where a setup link's cookie isn't kept. Open %s through the panel instead, or serve the domain over HTTPS", domain, install.GetProject()), "no_https")
	}
	// The passkey, once nothing else refuses. With no proof, the session's cover may
	// stand in (ADR 0075).
	client, _ := authapi.ClientFrom(ctx)
	target := install.GetProjectId()
	giveBack, err := h.stepUps.HoldStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpSetUpCatalogInstall, target)
	if err != nil && req.Msg.GetProof() == "" {
		if !errors.Is(err, auth.ErrNeedsStepUp) {
			return nil, authapi.StepUpRefused(err)
		}
		h.stepUps.AskedForStepUp(p, auth.StepUpSetUpCatalogInstall, target, func() {})
		return nil, authapi.ReasonError(connect.CodePermissionDenied,
			fmt.Errorf("a setup link lets whoever has it set %s up, so making one takes your passkey. Confirm with it, then make the link again", install.GetProject()),
			authapi.ReasonNeedsStepUp)
	}
	if err != nil {
		return nil, authapi.ReasonError(connect.CodePermissionDenied, err, authapi.ReasonNeedsStepUp)
	}
	done := false
	defer func() {
		if !done {
			giveBack()
		}
	}()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, h.internal(ctx, err)
	}
	key := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(key))
	until := time.Now().Add(SetupFor).Truncate(time.Second)
	setup := &resourcev1.RouteSetup{KeyHash: hex.EncodeToString(sum[:]), Until: until.Unix()}
	var edits []*dockwellv1.Edit
	var refs []resource.Ref
	for _, r := range routes {
		spec := proto.CloneOf(r.spec)
		spec.Setup = proto.CloneOf(setup)
		edits = append(edits, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(r.ref.Kind), Scope: r.ref.Scope, Name: r.ref.Name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: spec}},
		})
		refs = append(refs, r.ref)
	}
	app := cmp.Or(install.GetName(), install.GetPin().GetEntry())
	notes := []revision.Note{{
		Reason:  NoteSetupLink,
		Message: "A setup link lets one browser through " + domain + "'s hold for an hour, to set " + app + " up; everyone else still finds it isn't open yet",
	}}
	change, err := h.changes.Propose(ctx, "Set "+install.GetProject()+" up at "+domain, edits, nil, changes.Also{SetsUp: refs, Notes: notes})
	if err != nil {
		return nil, err
	}
	applied, err := h.changes.ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint()}))
	if err != nil {
		return nil, err
	}
	done = true
	// The link works once its server's agent has it, so it goes back only then,
	// never to a first click that would find the app isn't open yet (the review's M4).
	if err := h.waitApplied(ctx, change.GetId(), install.GetProject()); err != nil {
		return nil, err
	}
	path := strings.TrimSuffix(routes[0].spec.GetPath(), "/")
	link := url.URL{Scheme: "https", Host: domain, Path: path + "/.well-known/dockwell-setup", RawQuery: url.Values{"key": {key}}.Encode()}
	return connect.NewResponse(&dockwellv1.SetUpCatalogInstallResponse{
		Url: link.String(), Until: timestamppb.New(until), Change: applied.Msg.GetChange(),
	}), nil
}

// heldRoutes are an environment's held routes that send to its apps, by domain,
// then path.
func (h *Handler) heldRoutes(ctx context.Context, environment string) ([]heldRoute, error) {
	var out []heldRoute
	err := h.db.View(ctx, func(tx *store.Tx) error {
		list, err := tx.ListResources(ctx, environment, string(route.Kind))
		if err != nil {
			return err
		}
		for i := range list {
			r := list[i]
			if r.Spec == nil {
				continue
			}
			spec, err := h.keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return err
			}
			rt, ok := spec.(*resourcev1.Route)
			if !ok || !rt.GetHeld() || rt.GetDomain() == "" {
				continue
			}
			out = append(out, heldRoute{ref: resource.Ref{Kind: route.Kind, Scope: environment, Name: r.Name}, spec: rt})
		}
		return nil
	})
	slices.SortFunc(out, func(a, b heldRoute) int {
		return cmp.Or(cmp.Compare(a.spec.GetDomain(), b.spec.GetDomain()), cmp.Compare(a.spec.GetPath(), b.spec.GetPath()))
	})
	return out, err
}

// overHTTPS says whether a browser reaches a route over HTTPS, as the route's own
// rule says (route.PlainAtTheBrowser), which the panel mirrors (the review's M6).
func overHTTPS(r *resourcev1.Route) bool { return !route.PlainAtTheBrowser(r) }

// setupWait bounds how long a setup link waits for its change to reach the server.
var setupWait = time.Minute

// waitApplied waits for the change that writes a setup link to apply on its
// server, and says why where it failed or didn't within setupWait.
func (h *Handler) waitApplied(ctx context.Context, change, project string) error {
	ctx, cancel := context.WithTimeout(ctx, setupWait)
	defer cancel()
	for {
		var got store.Change
		err := h.db.View(ctx, func(tx *store.Tx) error {
			var err error
			got, err = tx.GetChange(ctx, change)
			return err
		})
		if err != nil && ctx.Err() == nil {
			return h.internal(ctx, err)
		}
		switch got.State {
		case store.ChangeApplied:
			return nil
		case store.ChangeFailed, store.ChangeDiscarded:
			return authapi.ReasonError(connect.CodeFailedPrecondition,
				fmt.Errorf("the change that writes %s's setup link didn't apply: %s. Make a new link once that's fixed", project, cmp.Or(got.Error, got.State)), "not_applied")
		}
		select {
		case <-ctx.Done():
			return authapi.ReasonError(connect.CodeUnavailable,
				fmt.Errorf("the change that writes %s's setup link hasn't reached its server within %s, so the link wouldn't work yet. Check that the server is connected, then make a new link", project, setupWait), "not_applied")
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// clearSetups takes the setup link off an install's held routes, as a change it
// applies at once, where any has one.
func (h *Handler) clearSetups(ctx context.Context, install *dockwellv1.CatalogInstall) error {
	routes, err := h.heldRoutes(ctx, install.GetEnvironmentId())
	if err != nil {
		return h.internal(ctx, err)
	}
	var edits []*dockwellv1.Edit
	var refs []resource.Ref
	for _, r := range routes {
		if r.spec.GetSetup() == nil {
			continue
		}
		spec := proto.CloneOf(r.spec)
		spec.Setup = nil
		edits = append(edits, &dockwellv1.Edit{
			Resource: &dockwellv1.ResourceRef{Kind: string(r.ref.Kind), Scope: r.ref.Scope, Name: r.ref.Name},
			Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Route{Route: spec}},
		})
		refs = append(refs, r.ref)
	}
	if len(edits) == 0 {
		return nil
	}
	change, err := h.changes.Propose(ctx, "Take "+install.GetProject()+"'s setup link away", edits, nil, changes.Also{SetsUp: refs})
	if err != nil {
		return err
	}
	_, err = h.changes.ApplyChange(ctx, connect.NewRequest(&dockwellv1.ApplyChangeRequest{Id: change.GetId(), Fingerprint: change.GetPlan().GetFingerprint()}))
	return err
}
