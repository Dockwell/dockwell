// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/fault"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/binding"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/store"
)

var _ resource.Values = (*Keeper)(nil)

// ErrNotPlaced means a server asked for, or was about to be sent, the
// values of a resource that isn't placed on it.
var ErrNotPlaced = errors.New("the resource isn't placed on that server")

// Values returns what a resource's latest revision uses, for the server
// it is about to run on: each value by the name the resource sees it as,
// read at the revision the spec pins. It gives values only for a resource
// whose latest revision is placed on server, and only for that revision's
// spec as the server sees it, which desired must be: a server gets the
// secrets of what runs on it, and nothing else.
//
// What only the control plane keeps (resource.ControlOnly), such as which of a
// service's values reads show (ADR 0081), never reaches a server, so it doesn't
// count: a revision that changes only that applies nothing, and the spec a
// server runs, and is sent to keep (ADR 0054), is still the latest's as the
// server sees it (#593).
func (k *Keeper) Values(ctx context.Context, server string, ref resource.Ref, desired proto.Message) (map[string]string, error) {
	t, err := k.kinds.Type(ref.Kind)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	err = k.db.View(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResource(ctx, string(ref.Kind), ref.Scope, ref.Name)
		if errors.Is(err, store.ErrNotFound) || err == nil && r.Spec == nil {
			return fault.New(ErrNotPlaced, "%s isn't on record, so no values go to %s for it", ref, server)
		}
		if err != nil {
			return err
		}
		placed, err := tx.Placement(ctx, r.ID, r.Revision)
		if err != nil {
			return err
		}
		if !slices.Contains(placed, server) {
			return fault.New(ErrNotPlaced, "%s %s doesn't run on %s, so its variables and secrets don't go there", ref.Kind, ref.Name, server)
		}
		latest, err := k.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return err
		}
		if !proto.Equal(k.kinds.OnServer(ref.Kind, latest), k.kinds.OnServer(ref.Kind, desired)) {
			return fault.New(ErrNotPlaced, "the spec to apply isn't the latest revision of %s %s, so its values don't go to %s", ref.Kind, ref.Name, server)
		}
		for _, u := range resource.Uses(t, latest) {
			v, err := k.valueAt(ctx, tx, ref.Scope, u)
			if err != nil {
				return err
			}
			out[u.As] = v
		}
		return nil
	})
	if err != nil {
		clear(out)
		return nil, err
	}
	return out, nil
}

// TakenOnNamingValues says which of refs were taken on from a running container and
// name values in what runs on server, as NamingValues has it (ADR 0096). An agent
// that doesn't keep a taken-on container's references reads its command with the
// values in, so the agent stream reads, plans and applies none of these on one.
func (k *Keeper) TakenOnNamingValues(ctx context.Context, server string, refs []resource.Ref) ([]resource.Ref, error) {
	var taken []resource.Ref
	err := k.db.View(ctx, func(tx *store.Tx) error {
		names := make([]store.TakenOnName, 0, len(refs))
		for _, r := range refs {
			names = append(names, store.TakenOnName{Kind: string(r.Kind), Scope: r.Scope, Name: r.Name})
		}
		found, err := tx.TakenOn(ctx, names)
		if err != nil {
			return err
		}
		for i, n := range names {
			if found[n] {
				taken = append(taken, refs[i])
			}
		}
		return nil
	})
	if err != nil || len(taken) == 0 {
		return nil, err
	}
	return k.NamingValues(ctx, server, taken)
}

// NamingValues says which of refs name values in what runs, rather than hold them
// (Container.references), on server: in their latest revision, or in any revision
// the server was given, as what it runs may be an older one than the latest, such as
// one a change that never reached it would have replaced. An agent from before
// references reads such a container's command as Docker has it, value and all, so the
// agent stream reads, plans and applies none of these on one (ADR 0022).
func (k *Keeper) NamingValues(ctx context.Context, server string, refs []resource.Ref) ([]resource.Ref, error) {
	var out []resource.Ref
	err := k.db.View(ctx, func(tx *store.Tx) error {
		found := make([]store.Resource, 0, len(refs))
		ids := make([]string, 0, len(refs))
		for _, ref := range refs {
			r, err := tx.GetResource(ctx, string(ref.Kind), ref.Scope, ref.Name)
			if errors.Is(err, store.ErrNotFound) {
				found = append(found, store.Resource{})
				continue
			}
			if err != nil {
				return err
			}
			found = append(found, r)
			ids = append(ids, r.ID)
		}
		given, err := tx.RevisionsGiven(ctx, server, ids)
		if err != nil {
			return err
		}
		for i, r := range found {
			if r.ID == "" {
				continue
			}
			numbers := append([]int{r.Revision}, given[r.ID]...)
			slices.Sort(numbers)
			for _, n := range slices.Compact(numbers) {
				names, err := k.namesValuesAt(ctx, tx, r, n)
				if err != nil {
					return err
				}
				if names {
					out = append(out, refs[i])
					break
				}
			}
		}
		return nil
	})
	return out, err
}

// namesValuesAt reports a revision of r whose spec names values.
func (k *Keeper) namesValuesAt(ctx context.Context, tx *store.Tx, r store.Resource, number int) (bool, error) {
	spec := r.Spec
	if number != r.Revision {
		rev, err := tx.GetRevision(ctx, r.ID, number)
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		spec = rev.Spec
	}
	if spec == nil {
		return false, nil // a revision that deleted it
	}
	m, err := k.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, number), spec)
	if err != nil {
		return false, err
	}
	return binding.WritesReferences(m), nil
}

// valueAt reads what a use resolves to, at the revision it is pinned to.
func (k *Keeper) valueAt(ctx context.Context, tx *store.Tx, scopeID string, u resource.Use) (string, error) {
	return k.valueFrom(ctx, tx, scopeID, u, 0, nil)
}

// trace stops a read where a value comes from a connection, and says which. A
// reveal below an admin reads with one first, before it asks for the passkey.
type trace struct {
	connection       string
	allowConnections bool
}

// errFromConnection stops a traced read at a connection's value.
var (
	errFromConnection = errors.New("a value comes from a connection")
	errFromCustom     = errors.New("a value comes from a Custom connection")
)

// maxFrom bounds how far values written from others go: a URL from a password from
// another environment is two deep.
const maxFrom = 4

// valueFrom is valueAt, depth steps into values written from others. A use that names
// its own scope, as a link's value from another environment does, is read there.
func (k *Keeper) valueFrom(ctx context.Context, tx *store.Tx, scopeID string, u resource.Use, depth int, tr *trace) (string, error) {
	if u.Scope != "" {
		scopeID = u.Scope
	}
	vt, err := k.kinds.Type(u.Kind)
	if err != nil {
		return "", err
	}
	if _, ok := vt.(resource.Valuer); !ok {
		return "", fmt.Errorf("a %s has no value to use", u.Kind)
	}
	r, err := tx.GetResource(ctx, string(u.Kind), scopeID, u.Name)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("there's no %s %s for %s", u.Kind, u.Name, u.As)
	}
	if err != nil {
		return "", err
	}
	if u.Revision <= 0 {
		return "", fmt.Errorf("%s uses %s %s at no revision; plan the change again", u.As, u.Kind, u.Name)
	}
	rev, err := tx.GetRevision(ctx, r.ID, u.Revision)
	if err != nil {
		return "", err
	}
	if rev.Spec == nil {
		return "", fmt.Errorf("%s uses %s %s at revision %d, which deleted it", u.As, u.Kind, u.Name, u.Revision)
	}
	spec, err := k.Decode(ctx, tx, store.InRevision(r.ID, string(u.Kind), rev.Revision), rev.Spec)
	if err != nil {
		return "", err
	}
	return k.written(ctx, tx, scopeID, u.Kind, spec, depth, tr)
}

// written is a value's spec written out: what it holds, or what it comes from, read
// at the revisions it pins (ADR 0084). With a trace, it stops at a value from a
// connection, before reading it.
func (k *Keeper) written(ctx context.Context, tx *store.Tx, scopeID string, kind resource.Kind, spec proto.Message, depth int, tr *trace) (string, error) {
	vt, err := k.kinds.Type(kind)
	if err != nil {
		return "", err
	}
	valuer, ok := vt.(resource.Valuer)
	if !ok {
		return "", fmt.Errorf("a %s has no value to use", kind)
	}
	from := variable.FromOf(spec)
	if from == nil {
		return valuer.Value(spec), nil
	}
	if depth >= maxFrom {
		return "", fmt.Errorf("a value is written from others more than %d deep, as one names itself through another. Write one of them out as a value of its own", maxFrom)
	}
	uses := resource.Uses(vt, spec)
	switch src := from.GetSource().(type) {
	case *resourcev1.ValueFrom_Template:
		values := make(map[string]string, len(uses))
		for _, u := range uses {
			v, err := k.valueFrom(ctx, tx, scopeID, u, depth+1, tr)
			if err != nil {
				return "", err
			}
			values[u.Name] = v
		}
		return variable.Write(src.Template, values)
	case *resourcev1.ValueFrom_Value:
		if len(uses) != 1 {
			return "", errors.New("a value from another environment names none")
		}
		return k.valueFrom(ctx, tx, scopeID, uses[0], depth+1, tr)
	case *resourcev1.ValueFrom_Connection:
		if len(uses) == 0 {
			return "", errors.New("a value from a connection names none")
		}
		if tr != nil {
			switch {
			case src.Connection.GetCustomField() != "":
				return "", errFromCustom
			case tr.allowConnections:
				return "", nil
			default:
				tr.connection = src.Connection.GetName()
				return "", errFromConnection
			}
		}
		part := src.Connection.GetPart()
		if variable.ReadsLink(part) {
			i := slices.IndexFunc(uses, func(u resource.Use) bool { return u.As == variable.LinkUse })
			if i < 0 {
				return "", fmt.Errorf("a value of a link's %s names no link", part)
			}
			l, err := k.linkAt(ctx, tx, scopeID, uses[i])
			if err != nil {
				return "", err
			}
			// A mail server's sender is the link's where it says one, for its app alone,
			// and the connection's otherwise.
			switch part {
			case resourcev1.LinkPart_LINK_PART_SENDER:
				if l.GetSender() != "" {
					return l.GetSender(), nil
				}
			case resourcev1.LinkPart_LINK_PART_SENDER_NAME:
				if l.GetSenderName() != "" {
					return l.GetSenderName(), nil
				}
			default:
				return l.GetPrefix(), nil
			}
		}
		return k.connectionPart(ctx, tx, uses[0], part, src.Connection.GetCustomField())
	}
	return "", errors.New("a value says it comes from somewhere, and not where")
}

// connectionPart is one part of a connection's values, read at the revision a value
// pins, opened as every sealed spec is (ADR 0099).
func (k *Keeper) connectionPart(ctx context.Context, tx *store.Tx, u resource.Use, part resourcev1.LinkPart, custom string) (string, error) {
	r, err := tx.GetResource(ctx, string(u.Kind), u.Scope, u.Name)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("there's no connection %s for %s", u.Name, u.As)
	}
	if err != nil {
		return "", err
	}
	if u.Revision <= 0 {
		return "", fmt.Errorf("%s uses the connection %s at no revision; plan the change again", u.As, u.Name)
	}
	rev, err := tx.GetRevision(ctx, r.ID, u.Revision)
	if err != nil {
		return "", err
	}
	if rev.Spec == nil {
		return "", fmt.Errorf("%s uses the connection %s at revision %d, which deleted it", u.As, u.Name, u.Revision)
	}
	spec, err := k.Decode(ctx, tx, store.InRevision(r.ID, string(u.Kind), rev.Revision), rev.Spec)
	if err != nil {
		return "", err
	}
	c, ok := spec.(*resourcev1.ConnectionSpec)
	if !ok {
		return "", fmt.Errorf("%s isn't a connection", u.Name)
	}
	if custom != "" {
		field := connection.CustomField(c, custom)
		if field == nil {
			return "", fmt.Errorf("the connection %s has no field %s", u.Name, custom)
		}
		if connection.CustomSecret(field) {
			return field.GetSecretValue(), nil
		}
		return field.GetValue(), nil
	}
	value, _, ok := connection.Part(c, part)
	if !ok {
		return "", fmt.Errorf("the connection %s holds no %s", u.Name, part)
	}
	return value, nil
}

// linkAt is a link as a value pins it, for what the link owns: the prefix it keeps
// its app's files under, so the value can't say other than the link does (ADR 0099,
// the re-check's N3), and the sender it says for its app.
func (k *Keeper) linkAt(ctx context.Context, tx *store.Tx, scopeID string, u resource.Use) (*resourcev1.Link, error) {
	r, err := tx.GetResource(ctx, string(u.Kind), cmp.Or(u.Scope, scopeID), u.Name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("there's no link %s for %s", u.Name, u.As)
	}
	if err != nil {
		return nil, err
	}
	if u.Revision <= 0 {
		return nil, fmt.Errorf("%s uses the link %s at no revision; plan the change again", u.As, u.Name)
	}
	rev, err := tx.GetRevision(ctx, r.ID, u.Revision)
	if err != nil {
		return nil, err
	}
	if rev.Spec == nil {
		return nil, fmt.Errorf("%s uses the link %s at revision %d, which deleted it", u.As, u.Name, u.Revision)
	}
	spec, err := k.Decode(ctx, tx, store.InRevision(r.ID, string(u.Kind), rev.Revision), rev.Spec)
	if err != nil {
		return nil, err
	}
	l, ok := spec.(*resourcev1.Link)
	if !ok {
		return nil, fmt.Errorf("%s isn't a link", u.Name)
	}
	return l, nil
}

// Revealed is a secret's value, as RevealSecret returns it.
type Revealed struct {
	Value    string
	Revision int
}

// ErrNotSecret means a reveal named something that isn't a secret.
var ErrNotSecret = errors.New("there's no such secret")

// ErrConnectionKey means a reveal below an admin named a secret that reads a
// connection's value, which is install-wide (ADR 0099).
var ErrConnectionKey = errors.New("only an admin reveals a connection's value")

// Reveal returns a secret's latest value, and records in the audit log, in
// the same transaction, that actor revealed it. If the record can't be
// written, nothing is revealed. confirm checks, in the same transaction,
// that a person in a signed-in session confirmed it with a passkey just now,
// or with their session's cover (ADR 0075), and may spend the proof: it is
// called once nothing else refuses, such as there being no such secret, and
// before the value is read. A secret that reads a connection's value, directly or
// through others, is revealed only where connections says the caller is an admin
// of the install: the value is the connection's, whichever app reads it (ADR 0099).
func (k *Keeper) Reveal(ctx context.Context, secretID, actor, surface string, connections bool, confirm func(*store.Tx) error) (Revealed, error) {
	var out Revealed
	err := k.db.Update(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResourceByID(ctx, secretID)
		if errors.Is(err, store.ErrNotFound) || err == nil && (r.Kind != string(variable.SecretKind) || r.Spec == nil) {
			return fault.New(ErrNotSecret, "there's no secret %s. Check its ID with dockwell variables list", secretID)
		}
		if err != nil {
			return err
		}
		spec, err := k.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return err
		}
		tr := trace{allowConnections: connections}
		_, err = k.written(ctx, tx, r.Scope, variable.SecretKind, spec, 0, &tr)
		switch {
		case errors.Is(err, errFromCustom):
			return fault.New(ErrConnectionKey, "this secret is supplied by a Custom connection and can only be used by its app")
		case errors.Is(err, errFromConnection):
			return fault.New(ErrConnectionKey, "%s reads its value from the connection %s, so only an admin reveals it. Ask an admin of the install", r.Name, tr.connection)
		}
		if err := confirm(tx); err != nil {
			return err
		}
		value, err := k.written(ctx, tx, r.Scope, variable.SecretKind, spec, 0, nil)
		if err != nil {
			return err
		}
		out = Revealed{Value: value, Revision: r.Revision}
		env := r.Scope
		if e, err := tx.GetResourceByID(ctx, r.Scope); err == nil {
			env = e.Name
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: k.now(), Actor: actor, Action: "secret.reveal", Subject: r.ID, Target: r.ID, Surface: surface,
			Detail: fmt.Sprintf("revealed %s, revision %d, in %s", r.Name, r.Revision, env),
		})
	})
	if err != nil {
		out = Revealed{}
		return out, err
	}
	return out, nil
}
