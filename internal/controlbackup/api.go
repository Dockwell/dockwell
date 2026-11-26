// SPDX-License-Identifier: AGPL-3.0-only

package controlbackup

import (
	"context"
	"crypto/subtle"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/trustelem/zxcvbn"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/backup/keep"
	"github.com/dockwell/dockwell/internal/control/agentapi"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

func (m *Manager) unavailable(ctx context.Context, err error) *connect.Error {
	m.cfg.Logger.ErrorContext(ctx, "could not read or record control plane backup state", "error", err)
	return connect.NewError(connect.CodeUnavailable, errors.New("control plane backup settings are unavailable. Try again"))
}

// GetControlBackup returns the current schedule and where whole copies reached.
func (m *Manager) GetControlBackup(ctx context.Context, _ *connect.Request[dockwellv1.GetControlBackupRequest]) (*connect.Response[dockwellv1.GetControlBackupResponse], error) {
	var out *dockwellv1.GetControlBackupResponse
	err := m.cfg.DB.View(ctx, func(tx *store.Tx) error {
		s, err := tx.ControlBackup(ctx)
		if err != nil {
			return err
		}
		places, err := tx.ControlBackupPlaces(ctx, s.Generation)
		if err != nil {
			return err
		}
		servers, err := tx.ListServers(ctx)
		if err != nil {
			return err
		}
		now := m.cfg.Now()
		prefix := keep.Prefix(m.cfg.Install, s.Generation)
		out = &dockwellv1.GetControlBackupResponse{
			Install: m.cfg.Install, Generation: s.Generation, Address: m.cfg.Address,
			Destination: s.Destination, PassphraseKept: s.Kept, Hourly: s.Hourly,
			Requested: s.Requested, Running: m.running.Load(), Problem: s.Problem, NextAt: timestamppb.New(due(s, now)),
		}
		if s.LastAttempt.UnixMilli() != 0 {
			out.LastAttempt = timestamppb.New(s.LastAttempt)
		}
		byPlace := make(map[string]store.ControlBackupPlace, len(places))
		for i := range places {
			p := &places[i]
			byPlace[p.Place] = *p
		}
		if s.Destination != "" {
			p := byPlace[offsitePlace]
			if p.Target != s.DestinationPin {
				p = store.ControlBackupPlace{}
			}
			out.Offsite = placeStatus(now, s.RecoveryID, p)
			out.Offsite.Name = s.Destination
			d, err := m.destination(ctx, tx, s.Destination)
			switch {
			case err != nil:
				out.Offsite.Problem = "The destination could not be read. Check its settings"
			case d.pin != s.DestinationPin:
				out.Offsite.Problem = "The destination changed. Ask the owner to select it again"
				out.Offsite.SentAt, out.Offsite.BundleAt = nil, nil
				out.Offsite.File = ""
				out.Offsite.Behind = true
			default:
				out.Storage = d.sheet(prefix)
				out.Offsite.Path = out.Storage.GetPrefix()
			}
		}
		for i := range servers {
			server := &servers[i]
			if server.ID == m.cfg.ControlID {
				continue
			}
			out.EligibleServers = append(out.EligibleServers, &dockwellv1.ControlBackupServer{Id: server.ID, Name: server.Name})
			if slices.Contains(s.Preferred, server.ID) {
				out.PreferredServers = append(out.PreferredServers, server.ID)
			}
			if slices.Contains(s.Excluded, server.ID) {
				out.ExcludedServers = append(out.ExcludedServers, server.ID)
			}
			p, tried := byPlace[server.ID]
			var session *agentapi.Session
			var connected bool
			if m.cfg.Agents != nil {
				session, connected = m.cfg.Agents.Session(server.ID)
			}
			if !tried && (connected && session.KeepsControlBackups() || slices.Contains(s.Excluded, server.ID)) && !slices.Contains(s.Preferred, server.ID) {
				continue
			}
			st := placeStatus(now, s.RecoveryID, p)
			st.Name, st.Server = server.Name, server.ID
			switch {
			case slices.Contains(s.Excluded, server.ID):
				st.Problem = "This server is excluded from new copies"
			case !connected:
				st.Problem = "Server is disconnected"
			case !session.KeepsControlBackups():
				st.Problem = "Update Dockwell on this server to keep its copy"
			}
			out.Servers = append(out.Servers, st)
		}
		return nil
	})
	if err != nil {
		return nil, m.unavailable(ctx, err)
	}
	return connect.NewResponse(out), nil
}

func placeStatus(now time.Time, recoveryID string, p store.ControlBackupPlace) *dockwellv1.ControlBackupPlace {
	out := &dockwellv1.ControlBackupPlace{Problem: p.Problem, Path: p.Path, File: p.File}
	if p.SentAt.IsZero() {
		out.Behind = p.Problem != ""
		return out
	}
	out.SentAt = timestamppb.New(p.SentAt)
	if at, ok := keep.Taken(p.File); ok {
		out.BundleAt = timestamppb.New(at)
	}
	out.Behind = now.Sub(p.SentAt) >= 2*time.Hour
	switch {
	case recoveryID != p.RecoveryID && out.Problem == "":
		out.Problem = "The previous passphrase still opens this copy"
	case recoveryID != p.RecoveryID:
		out.Problem += ". The previous passphrase still opens this copy"
	case out.Behind && out.Problem == "":
		out.Problem = "This place has not received a backup in two hours"
	}
	return out
}

// TakeControlBackup schedules the next permitted run and records who asked.
func (m *Manager) TakeControlBackup(ctx context.Context, _ *connect.Request[dockwellv1.TakeControlBackupRequest]) (*connect.Response[dockwellv1.TakeControlBackupResponse], error) {
	var next time.Time
	p, _ := authapi.PrincipalFrom(ctx)
	client, _ := authapi.ClientFrom(ctx)
	now := m.cfg.Now()
	err := m.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		s, err := tx.ControlBackup(ctx)
		if err != nil {
			return err
		}
		s.Requested = true
		next = latest(now, due(s, now))
		if err := tx.RequestControlBackup(ctx); err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: now, Actor: p.Actor(),
			Surface: client.Surface(p), Action: ActionRequested, Subject: m.cfg.Install,
		})
	})
	if err != nil {
		return nil, m.unavailable(ctx, err)
	}
	m.wakeUp()
	return connect.NewResponse(&dockwellv1.TakeControlBackupResponse{NextAt: timestamppb.New(next)}), nil
}

func owner(ctx context.Context) (auth.Principal, auth.Client, error) {
	p, _ := authapi.PrincipalFrom(ctx)
	client, _ := authapi.ClientFrom(ctx)
	if p.Token != nil || p.Session.ID == "" {
		return p, client, authapi.StepUpRefused(auth.ErrNeedsSession)
	}
	if p.User.Role != store.RoleOwner {
		return p, client, connect.NewError(connect.CodePermissionDenied, errors.New("only the owner can change control plane backup settings"))
	}
	return p, client, nil
}

func requireOwner(ctx context.Context, tx *store.Tx, p auth.Principal) error {
	current, err := tx.Owner(ctx)
	if err != nil {
		return err
	}
	if current.ID != p.User.ID {
		return connect.NewError(connect.CodePermissionDenied, errors.New("only the current owner can change control plane backup settings"))
	}
	return nil
}

// SetControlBackupDestination pins the selected storage after the owner confirms it.
func (m *Manager) SetControlBackupDestination(ctx context.Context, req *connect.Request[dockwellv1.SetControlBackupDestinationRequest]) (*connect.Response[dockwellv1.SetControlBackupDestinationResponse], error) {
	p, client, err := owner(ctx)
	if err != nil {
		return nil, err
	}
	name := req.Msg.GetDestination()
	now := m.cfg.Now()
	err = m.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		if err := requireOwner(ctx, tx, p); err != nil {
			return err
		}
		var d destination
		if name != "" {
			var err error
			d, err = m.destination(ctx, tx, name)
			if err != nil {
				return authapi.Invalid(errors.New("choose an existing backup destination with valid storage settings"))
			}
		}
		if err := m.cfg.Auth.UseStepUpIn(ctx, tx, client, p, req.Msg.GetProof(), auth.StepUpControlBackupDestination, "destination:"+name); err != nil {
			return authapi.StepUpRefused(err)
		}
		if err := tx.SetControlBackupDestination(ctx, name, d.pin, now); err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: now, Actor: p.Actor(),
			Surface: client.Surface(p), Action: ActionDestination, Subject: m.cfg.Install, Detail: name,
		})
	})
	if err != nil {
		var refused *connect.Error
		if errors.As(err, &refused) {
			return nil, refused
		}
		return nil, m.unavailable(ctx, err)
	}
	m.wakeUp()
	return connect.NewResponse(&dockwellv1.SetControlBackupDestinationResponse{}), nil
}

// SetControlBackupServers saves the owner’s placement preferences.
func (m *Manager) SetControlBackupServers(ctx context.Context, req *connect.Request[dockwellv1.SetControlBackupServersRequest]) (*connect.Response[dockwellv1.SetControlBackupServersResponse], error) {
	p, client, err := owner(ctx)
	if err != nil {
		return nil, err
	}
	preferred, excluded := req.Msg.GetPreferred(), req.Msg.GetExcluded()
	if len(preferred) > 2 {
		return nil, authapi.Invalid(errors.New("choose at most two preferred servers"))
	}
	now := m.cfg.Now()
	err = m.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		if err := requireOwner(ctx, tx, p); err != nil {
			return err
		}
		servers, err := tx.ListServers(ctx)
		if err != nil {
			return err
		}
		available := make(map[string]bool, len(servers))
		for i := range servers {
			server := &servers[i]
			available[server.ID] = server.ID != m.cfg.ControlID
		}
		for _, server := range slices.Concat(preferred, excluded) {
			if !available[server] {
				return authapi.Invalid(errors.New("choose different servers from the list of other servers"))
			}
			delete(available, server)
		}
		if err := tx.SetControlBackupServers(ctx, preferred, excluded, now); err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: now, Actor: p.Actor(),
			Surface: client.Surface(p), Action: ActionServers, Subject: m.cfg.Install,
		})
	})
	if err != nil {
		var refused *connect.Error
		if errors.As(err, &refused) {
			return nil, refused
		}
		return nil, m.unavailable(ctx, err)
	}
	m.wakeUp()
	return connect.NewResponse(&dockwellv1.SetControlBackupServersResponse{}), nil
}

// ShowControlBackupPassphrase spends a fresh proof and records the reveal.
func (m *Manager) ShowControlBackupPassphrase(ctx context.Context, req *connect.Request[dockwellv1.ShowControlBackupPassphraseRequest]) (*connect.Response[dockwellv1.ShowControlBackupPassphraseResponse], error) {
	p, client, err := owner(ctx)
	if err != nil {
		return nil, err
	}
	var phrase string
	err = m.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		if err := requireOwner(ctx, tx, p); err != nil {
			return err
		}
		if err := m.cfg.Auth.UseStepUpIn(ctx, tx, client, p, req.Msg.GetProof(), auth.StepUpControlBackupShow, p.User.ID); err != nil {
			return authapi.StepUpRefused(err)
		}
		s, err := tx.ControlBackup(ctx)
		if err != nil {
			return err
		}
		r, err := m.openRecovery(s)
		if err != nil {
			return err
		}
		clear(r.Base)
		phrase = r.Phrase
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: m.cfg.Now(), Actor: p.Actor(),
			Surface: client.Surface(p), Action: ActionShow, Subject: m.cfg.Install,
		})
	})
	if err != nil {
		var refused *connect.Error
		if errors.As(err, &refused) {
			return nil, refused
		}
		return nil, m.unavailable(ctx, err)
	}
	res := connect.NewResponse(&dockwellv1.ShowControlBackupPassphraseResponse{Passphrase: phrase})
	res.Header().Set("Cache-Control", "no-store")
	return res, nil
}

func validatePhrase(phrase string) error {
	if len(phrase) > 256 || utf8.RuneCountInString(phrase) < 20 || strings.TrimSpace(phrase) != phrase ||
		strings.ContainsAny(phrase, "\r\n\x00") {
		return errors.New("use a passphrase of at least 20 characters and up to 256 bytes, on one line")
	}
	if zxcvbn.PasswordStrength(phrase, nil).Guesses < math.Exp2(80) {
		return errors.New("this passphrase is too easy to guess. Use a longer random phrase or generate one")
	}
	return nil
}

// SetControlBackupPassphrase changes the phrase and queues a new bundle.
func (m *Manager) SetControlBackupPassphrase(ctx context.Context, req *connect.Request[dockwellv1.SetControlBackupPassphraseRequest]) (*connect.Response[dockwellv1.SetControlBackupPassphraseResponse], error) {
	p, client, err := owner(ctx)
	if err != nil {
		return nil, err
	}
	if !m.deriving.CompareAndSwap(false, true) {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("a recovery passphrase is already being set. Try again"))
	}
	defer m.deriving.Store(false)
	release, err := m.cfg.Auth.HoldStepUp(ctx, client, p, req.Msg.GetProof(), auth.StepUpControlBackupSet, p.User.ID)
	if err != nil {
		return nil, authapi.StepUpRefused(err)
	}
	ok := false
	defer func() {
		if !ok {
			release()
		}
	}()
	phrase := req.Msg.GetPassphrase()
	if phrase == "" {
		phrase = GeneratePhrase()
	} else if err := validatePhrase(phrase); err != nil {
		return nil, authapi.Invalid(err)
	}
	now := m.cfg.Now()
	r := NewRecovery(phrase, now)
	defer clear(r.Base)
	sealed, err := m.sealRecovery(r)
	if err != nil {
		return nil, m.unavailable(ctx, err)
	}
	err = m.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		if err := requireOwner(ctx, tx, p); err != nil {
			return err
		}
		if err := tx.SetControlBackupPassphrase(ctx, r.ID, sealed, now); err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: now, Actor: p.Actor(),
			Surface: client.Surface(p), Action: ActionSet, Subject: m.cfg.Install,
		})
	})
	if err != nil {
		var refused *connect.Error
		if errors.As(err, &refused) {
			return nil, refused
		}
		return nil, m.unavailable(ctx, err)
	}
	ok = true
	m.wakeUp()
	return connect.NewResponse(&dockwellv1.SetControlBackupPassphraseResponse{}), nil
}

// ConfirmControlBackupPassphrase records that the owner can type the current phrase.
func (m *Manager) ConfirmControlBackupPassphrase(ctx context.Context, req *connect.Request[dockwellv1.ConfirmControlBackupPassphraseRequest]) (*connect.Response[dockwellv1.ConfirmControlBackupPassphraseResponse], error) {
	p, client, err := owner(ctx)
	if err != nil {
		return nil, err
	}
	err = m.cfg.DB.Update(ctx, func(tx *store.Tx) error {
		if err := requireOwner(ctx, tx, p); err != nil {
			return err
		}
		s, err := tx.ControlBackup(ctx)
		if err != nil {
			return err
		}
		r, err := m.openRecovery(s)
		if err != nil {
			return err
		}
		clear(r.Base)
		if subtle.ConstantTimeCompare([]byte(r.Phrase), []byte(req.Msg.GetPassphrase())) != 1 {
			return authapi.Invalid(errors.New("the recovery passphrase does not match. Enter it again"))
		}
		if s.Kept {
			return nil
		}
		if err := tx.ConfirmControlBackupPassphrase(ctx, m.cfg.Now()); err != nil {
			return err
		}
		return tx.RecordEvent(ctx, store.Event{
			ID: id.New(id.Event), At: m.cfg.Now(), Actor: p.Actor(),
			Surface: client.Surface(p), Action: ActionConfirm, Subject: m.cfg.Install,
		})
	})
	if err != nil {
		var refused *connect.Error
		if errors.As(err, &refused) {
			return nil, refused
		}
		return nil, m.unavailable(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.ConfirmControlBackupPassphraseResponse{}), nil
}
