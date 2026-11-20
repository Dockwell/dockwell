// SPDX-License-Identifier: AGPL-3.0-only

package alerts

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/resource/connection"
	"github.com/dockwell/dockwell/internal/resource/link"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/secrets"
	"github.com/dockwell/dockwell/internal/store"
)

var (
	errConnNotFound    = errors.New("connection not found")
	errConnInvalid     = errors.New("connection invalid")
	errChannelNotFound = errors.New("channel not found")
	errChannelGone     = errors.New("channel gone")
	errChannelChanged  = errors.New("channel changed")
)

type connError struct {
	sentinel error
	msg      string
}

func (e *connError) Error() string        { return e.msg }
func (e *connError) Is(target error) bool { return target == e.sentinel }

// resolveLiveSend reads the live alert channel by ID and (when connection-backed)
// its mail connection in one store snapshot immediately before an outbox send
// (H4, ADR 0099). If the channel was removed or now differs from what was queued,
// it returns errChannelGone or errChannelChanged without opening any connection.
func resolveLiveSend(ctx context.Context, db *store.DB, keeper *secrets.Keeper, channelID string, queued *resourcev1.AlertChannel) (resolved *resourcev1.AlertChannel, err error) {
	err = db.View(ctx, func(tx *store.Tx) error {
		r, err := tx.GetResourceByID(ctx, channelID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && (r.Spec == nil || r.Kind != string(alert.ChannelKind))) {
			return errChannelGone
		}
		if err != nil {
			return err
		}
		raw, err := keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
		if err != nil {
			return err
		}
		live, ok := raw.(*resourcev1.AlertChannel)
		if !ok {
			return errChannelGone
		}
		if !proto.Equal(live, queued) {
			return errChannelChanged
		}
		if live.GetEmail().GetConnection() == "" {
			resolved = live
			return nil
		}
		org, err := tx.Organization(ctx)
		if err != nil {
			return err
		}
		out, err := resolveEmailInTx(ctx, tx, keeper, org.ID, live, false)
		if err != nil {
			return err
		}
		resolved = out
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resolved, nil
}

// resolveTestChannel loads a named channel (if name != ""), validates and applies
// Keep/CheckNew to the channel being tested, determines whether it matches the
// live stored channel, and resolves its mail connection (with full draft
// isolation when !asStored) all inside one store snapshot (H4, ADR 0099).
func resolveTestChannel(ctx context.Context, db *store.DB, keeper *secrets.Keeper, name string, form *resourcev1.AlertChannel) (stored *channel, spec, sendSpec *resourcev1.AlertChannel, asStored bool, err error) {
	err = db.View(ctx, func(tx *store.Tx) error {
		org, err := tx.Organization(ctx)
		if err != nil {
			return err
		}
		if name != "" {
			r, err := tx.GetResource(ctx, string(alert.ChannelKind), org.ID, name)
			if errors.Is(err, store.ErrNotFound) || (err == nil && r.Spec == nil) {
				return &connError{
					sentinel: errChannelNotFound,
					msg:      fmt.Sprintf("there's no alert channel %s. List them to find its name", name),
				}
			}
			if err != nil {
				return err
			}
			raw, err := keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
			if err != nil {
				return fmt.Errorf("can't read the alert channel %s: %w", r.Name, err)
			}
			ch, ok := raw.(*resourcev1.AlertChannel)
			if !ok {
				return &connError{
					sentinel: errChannelNotFound,
					msg:      fmt.Sprintf("there's no alert channel %s. List them to find its name", name),
				}
			}
			stored = &channel{id: r.ID, name: r.Name, spec: ch}
		}
		switch {
		case form == nil && stored == nil:
			return &connError{sentinel: errConnInvalid, msg: "give the channel to test, by name or as a form holds it"}
		case form == nil:
			spec = proto.CloneOf(stored.spec)
		default:
			spec = proto.CloneOf(form)
		}
		if err := (alert.Channel{}).Validate(spec); err != nil {
			return &connError{sentinel: errConnInvalid, msg: err.Error()}
		}
		if stored != nil {
			if _, err := (alert.Channel{}).Keep(stored.spec, spec); err != nil {
				return &connError{sentinel: errConnInvalid, msg: err.Error()}
			}
		}
		if err := (alert.Channel{}).CheckNew(spec); err != nil {
			return &connError{sentinel: errConnInvalid, msg: err.Error()}
		}
		asStored = stored != nil && proto.Equal(spec, stored.spec)
		if spec.GetEmail().GetConnection() == "" {
			sendSpec = spec
			return nil
		}
		out, err := resolveEmailInTx(ctx, tx, keeper, org.ID, spec, !asStored)
		if err != nil {
			return err
		}
		sendSpec = out
		return nil
	})
	if err != nil {
		return nil, nil, nil, false, err
	}
	return stored, spec, sendSpec, asStored, nil
}

// resolveEmailInTx expands an email channel naming a mail connection into a
// transient copy holding the connection's host, port, security, user and
// password inside the caller's read transaction (ADR 0099).
func resolveEmailInTx(ctx context.Context, tx *store.Tx, keeper *secrets.Keeper, orgID string, spec *resourcev1.AlertChannel, checkIsolation bool) (*resourcev1.AlertChannel, error) {
	connName := spec.GetEmail().GetConnection()
	if connName == "" {
		return spec, nil
	}
	r, err := tx.GetResource(ctx, string(variable.ConnectionKind), orgID, connName)
	if errors.Is(err, store.ErrNotFound) || (err == nil && r.Spec == nil) {
		return nil, &connError{
			sentinel: errConnNotFound,
			msg:      fmt.Sprintf("alert channel can't send through connection %s, which isn't on this install", connName),
		}
	}
	if err != nil {
		return nil, err
	}
	raw, err := keeper.Decode(ctx, tx, store.InRevision(r.ID, r.Kind, r.Revision), r.Spec)
	if err != nil {
		return nil, err
	}
	c, ok := raw.(*resourcev1.ConnectionSpec)
	if !ok || c.GetKind() != resourcev1.LinkKind_LINK_KIND_SMTP || c.GetSmtp() == nil {
		return nil, &connError{
			sentinel: errConnInvalid,
			msg:      fmt.Sprintf("connection %s isn't a mail server", connName),
		}
	}
	smtp := c.GetSmtp()
	sec := resourcev1.AlertEmail_Security(connection.Security(smtp))
	if err := alert.CheckEmailTransport(smtp.GetHost(), sec, smtp.GetUser() != ""); err != nil {
		return nil, &connError{
			sentinel: errConnInvalid,
			msg:      fmt.Sprintf("connection %s can't send alert email: %s", connName, err.Error()),
		}
	}
	if checkIsolation {
		if err := checkDraftConnectionIsolation(ctx, tx, keeper, orgID, connName, c); err != nil {
			return nil, err
		}
	}
	out := proto.CloneOf(spec)
	em := out.GetEmail()
	em.Host = smtp.GetHost()
	em.Port = uint32(connection.Port(smtp)) //nolint:gosec // validated 1..65535
	em.Security = sec
	em.Username = smtp.GetUser()
	em.Password = smtp.GetPassword()
	smtp.User, smtp.Password = "", ""
	return out, nil
}

// checkDraftConnectionIsolation refuses testing an unsaved alert channel draft
// against a mail connection that is shared, linked by an app, or whose current
// credential an app holds via another linked SMTP connection or a live plain
// secret (shared connection.SMTPHeldByApp predicate) (ADR 0099). It fails closed
// on any decode error.
func checkDraftConnectionIsolation(ctx context.Context, tx *store.Tx, keeper *secrets.Keeper, orgID, connName string, c *resourcev1.ConnectionSpec) error {
	if c.GetShared() {
		return &connError{
			sentinel: errConnInvalid,
			msg:      fmt.Sprintf("connection %s is marked shared, because a connection alerts use serves no app. Add a dedicated mail connection for alerts, or unshare %s first", connName, connName),
		}
	}
	links, err := tx.ListKind(ctx, string(link.Kind))
	if err != nil {
		return err
	}
	var otherLinkedNames []string
	for i := range links {
		lr := &links[i]
		spec, err := keeper.Decode(ctx, tx, store.InRevision(lr.ID, lr.Kind, lr.Revision), lr.Spec)
		if err != nil {
			return fmt.Errorf("can't check whether link %s uses connection %s: %w", lr.Name, connName, err)
		}
		if l, ok := spec.(*resourcev1.Link); ok && l.GetConnection() != "" {
			if l.GetConnection() == connName {
				return &connError{
					sentinel: errConnInvalid,
					msg:      fmt.Sprintf("connection %s is used by an app link, so an alert channel can't send through it. Add a separate mail connection for alerts", connName),
				}
			}
			if l.GetKind() == resourcev1.LinkKind_LINK_KIND_SMTP && !slices.Contains(otherLinkedNames, l.GetConnection()) {
				otherLinkedNames = append(otherLinkedNames, l.GetConnection())
			}
		}
	}
	m := c.GetSmtp()
	if m.GetPassword() == "" {
		return nil
	}
	linkedAppConns := make([]*resourcev1.ConnectionSmtp, 0, len(otherLinkedNames))
	for _, otherName := range otherLinkedNames {
		cr, err := tx.GetResource(ctx, string(variable.ConnectionKind), orgID, otherName)
		if errors.Is(err, store.ErrNotFound) || err == nil && cr.Spec == nil {
			continue
		}
		if err != nil {
			return err
		}
		raw, err := keeper.Decode(ctx, tx, store.InRevision(cr.ID, cr.Kind, cr.Revision), cr.Spec)
		if err != nil {
			return fmt.Errorf("can't check whether linked connection %s shares credentials with %s: %w", otherName, connName, err)
		}
		if cs, ok := raw.(*resourcev1.ConnectionSpec); ok && cs.GetKind() == resourcev1.LinkKind_LINK_KIND_SMTP && cs.GetSmtp() != nil {
			linkedAppConns = append(linkedAppConns, cs.GetSmtp())
		}
	}
	storedSecrets, err := tx.ListKind(ctx, string(variable.SecretKind))
	if err != nil {
		return err
	}
	plainSecrets := make([]string, 0, len(storedSecrets))
	for i := range storedSecrets {
		sr := &storedSecrets[i]
		spec, err := keeper.Decode(ctx, tx, store.InRevision(sr.ID, sr.Kind, sr.Revision), sr.Spec)
		if err != nil {
			return fmt.Errorf("can't check whether an app holds the password of connection %s: %w", connName, err)
		}
		if sec, ok := spec.(*resourcev1.Secret); ok && sec.GetFrom() == nil && sec.GetValue() != "" {
			plainSecrets = append(plainSecrets, sec.GetValue())
		}
	}
	if connection.SMTPHeldByApp(m, linkedAppConns, plainSecrets) {
		return &connError{
			sentinel: errConnInvalid,
			msg:      fmt.Sprintf("an app holds the password of connection %s. Paste a password made for alerts alone on %s, and revoke the one the app had at your mail provider", connName, connName),
		}
	}
	return nil
}

// clearEmailAuth wipes transient SMTP credentials off a resolved email channel
// once a send finishes so they never linger past the call.
func clearEmailAuth(c *resourcev1.AlertChannel) {
	if em := c.GetEmail(); em != nil {
		em.Username = ""
		em.Password = ""
	}
}
