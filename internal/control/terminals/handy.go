// SPDX-License-Identifier: AGPL-3.0-only

package terminals

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/actors"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// ReasonLooksLikeSecret is returned when a saved command appears to contain a
// credential and the caller did not acknowledge plain-text storage.
const ReasonLooksLikeSecret = "looks_like_secret" //nolint:gosec // error reason code, not a credential

// Audit events for install-wide shared saved commands (ADR 0108).
// Only the command ID, name, previous name, and group are recorded — never the body.
const (
	EventCommandSave   = "terminal_command.save"
	EventCommandDelete = "terminal_command.delete"
)

// CommandActions are the shared saved-command events shown on Activity under Settings.
var CommandActions = []string{EventCommandSave, EventCommandDelete}

// CommandTold is what a shared saved-command event records on Activity. It never
// holds the command body.
type CommandTold struct {
	ID           string `json:"id,omitempty"`
	Name         string `json:"name,omitempty"`
	PreviousName string `json:"previous_name,omitempty"`
	Group        string `json:"group,omitempty"`
}

// ReadCommandTold decodes a shared saved-command event's detail.
func ReadCommandTold(detail string) (CommandTold, bool) {
	var t CommandTold
	err := json.Unmarshal([]byte(detail), &t)
	return t, err == nil
}

func (t CommandTold) String() string {
	b, _ := json.Marshal(t)
	return string(b)
}

// Limits for terminal preferences and saved commands.
const (
	MinFontSize        = 10.0
	MaxFontSize        = 24.0
	MinLineHeight      = 1.0
	MaxLineHeight      = 2.0
	MinScrollback      = 500
	MaxScrollback      = 50000
	MaxSavedCommands   = 200
	MaxCommandNameLen  = 80
	MaxCommandGroupLen = 64
	MaxCommandBodyLen  = 4096
)

// FontFamilies are the validated font family presets for terminal display.
var FontFamilies = []string{
	"",
	"default",
	"system-mono",
	"fira-code",
	"ibm-plex-mono",
	"source-code-pro",
}

func userPrefsKey(userID string) string    { return store.SettingUserTerminalPreferences(userID) }
func userCommandsKey(userID string) string { return store.SettingUserTerminalCommands(userID) }

const sharedCommandsKey = store.SettingSharedTerminalCommands

type storedPrefs struct {
	FontSize    float64 `json:"font_size,omitempty"`
	FontFamily  string  `json:"font_family,omitempty"`
	LineHeight  float64 `json:"line_height,omitempty"`
	CursorStyle int32   `json:"cursor_style,omitempty"`
	CursorBlink *bool   `json:"cursor_blink,omitempty"`
	Scrollback  uint32  `json:"scrollback,omitempty"`
}

func (s storedPrefs) proto() *dockwellv1.TerminalPreferences {
	return &dockwellv1.TerminalPreferences{
		FontSize:    s.FontSize,
		FontFamily:  s.FontFamily,
		LineHeight:  s.LineHeight,
		CursorStyle: dockwellv1.TerminalCursorStyle(s.CursorStyle),
		CursorBlink: s.CursorBlink,
		Scrollback:  s.Scrollback,
	}
}

type storedCommand struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Command   string    `json:"command"`
	Group     string    `json:"group,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedBy string    `json:"created_by,omitempty"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

func (c storedCommand) proto(shared bool, resolveActor func(string) string) *dockwellv1.SavedCommand {
	out := &dockwellv1.SavedCommand{
		Id:      c.ID,
		Name:    c.Name,
		Command: c.Command,
		Group:   c.Group,
		Shared:  shared,
	}
	if !c.CreatedAt.IsZero() {
		out.CreateTime = timestamppb.New(c.CreatedAt)
	}
	if !c.UpdatedAt.IsZero() {
		out.UpdateTime = timestamppb.New(c.UpdatedAt)
	}
	if shared && resolveActor != nil {
		if c.CreatedBy != "" {
			out.CreatedBy = resolveActor(c.CreatedBy)
		}
		if c.UpdatedBy != "" {
			out.UpdatedBy = resolveActor(c.UpdatedBy)
		}
	}
	return out
}

func actorNameResolver(ctx context.Context, tx *store.Tx) func(string) string {
	look := actors.New().In(ctx, tx)
	return func(actor string) string {
		if actor == "" {
			return ""
		}
		if resolved := look.Actor(actor); resolved != nil && resolved.GetName() != "" {
			return resolved.GetName()
		}
		return actor
	}
}

// GetTerminalWorkingDirectory returns a host terminal's live working directory
// when the agent can read it from /proc, and its home directory as a fallback.
// For container shells or older agents, home is left empty so the panel falls
// back to Files' own default instead of assuming /root.
func (h *Handler) GetTerminalWorkingDirectory(ctx context.Context, req *connect.Request[dockwellv1.GetTerminalWorkingDirectoryRequest]) (*connect.Response[dockwellv1.GetTerminalWorkingDirectoryResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	e, err := h.mine(p, req.Msg.GetTerminalId())
	if err != nil {
		return nil, err
	}
	srv, err := h.server(ctx, e.server)
	if err != nil {
		return nil, err
	}
	session, err := h.session(ctx, srv)
	if err != nil {
		return nil, err
	}
	if !session.ReadsTerminalCwd() {
		return connect.NewResponse(&dockwellv1.GetTerminalWorkingDirectoryResponse{
			Known: false,
		}), nil
	}
	res, err := session.TerminalCwd(ctx, e.id)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	cwd := res.GetCwd()
	return connect.NewResponse(&dockwellv1.GetTerminalWorkingDirectoryResponse{
		Path:  cwd,
		Home:  res.GetHome(),
		Known: cwd != "",
	}), nil
}

// GetTerminalPreferences returns the caller's saved terminal display settings.
func (h *Handler) GetTerminalPreferences(ctx context.Context, _ *connect.Request[dockwellv1.GetTerminalPreferencesRequest]) (*connect.Response[dockwellv1.GetTerminalPreferencesResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	var prefs storedPrefs
	err = h.db.View(ctx, func(tx *store.Tx) error {
		raw, gerr := tx.GetSetting(ctx, userPrefsKey(p.User.ID))
		if errors.Is(gerr, store.ErrNotFound) {
			return nil
		}
		if gerr != nil {
			return gerr
		}
		if uerr := json.Unmarshal(raw, &prefs); uerr != nil {
			prefs = storedPrefs{}
		}
		return nil
	})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.GetTerminalPreferencesResponse{Preferences: prefs.proto()}), nil
}

// UpdateTerminalPreferences validates and saves the caller's terminal display settings.
func (h *Handler) UpdateTerminalPreferences(ctx context.Context, req *connect.Request[dockwellv1.UpdateTerminalPreferencesRequest]) (*connect.Response[dockwellv1.UpdateTerminalPreferencesResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	in := req.Msg.GetPreferences()
	if in == nil {
		in = &dockwellv1.TerminalPreferences{}
	}
	prefs, err := validatePreferences(in)
	if err != nil {
		return nil, authapi.Invalid(err)
	}
	raw, err := json.Marshal(prefs)
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	if err := h.db.Update(ctx, func(tx *store.Tx) error {
		return tx.PutSetting(ctx, userPrefsKey(p.User.ID), raw)
	}); err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.UpdateTerminalPreferencesResponse{Preferences: prefs.proto()}), nil
}

func validatePreferences(in *dockwellv1.TerminalPreferences) (storedPrefs, error) {
	fs := in.GetFontSize()
	if math.IsNaN(fs) || math.IsInf(fs, 0) || (fs != 0 && (fs < MinFontSize || fs > MaxFontSize)) {
		return storedPrefs{}, fmt.Errorf("font size must be 0 (default) or between %g and %g px", MinFontSize, MaxFontSize)
	}
	ff := strings.TrimSpace(in.GetFontFamily())
	if !slices.Contains(FontFamilies, ff) {
		return storedPrefs{}, fmt.Errorf("unknown font family %q; choose one of the listed terminal fonts", ff)
	}
	if ff == "default" {
		ff = ""
	}
	lh := in.GetLineHeight()
	if math.IsNaN(lh) || math.IsInf(lh, 0) || (lh != 0 && (lh < MinLineHeight || lh > MaxLineHeight)) {
		return storedPrefs{}, fmt.Errorf("line height must be 0 (default) or between %g and %g", MinLineHeight, MaxLineHeight)
	}
	cs := in.GetCursorStyle()
	if cs < dockwellv1.TerminalCursorStyle_TERMINAL_CURSOR_STYLE_UNSPECIFIED || cs > dockwellv1.TerminalCursorStyle_TERMINAL_CURSOR_STYLE_BAR {
		return storedPrefs{}, errors.New("unknown cursor style; choose block, underline or bar")
	}
	sb := in.GetScrollback()
	if sb != 0 && (sb < MinScrollback || sb > MaxScrollback) {
		return storedPrefs{}, fmt.Errorf("scrollback must be 0 (default) or between %d and %d lines", MinScrollback, MaxScrollback)
	}
	var blink *bool
	if in.CursorBlink != nil {
		v := in.GetCursorBlink()
		blink = &v
	}
	return storedPrefs{
		FontSize:    fs,
		FontFamily:  ff,
		LineHeight:  lh,
		CursorStyle: int32(cs),
		CursorBlink: blink,
		Scrollback:  sb,
	}, nil
}

// ListSavedCommands returns the caller's personal commands and shared install commands.
func (h *Handler) ListSavedCommands(ctx context.Context, _ *connect.Request[dockwellv1.ListSavedCommandsRequest]) (*connect.Response[dockwellv1.ListSavedCommandsResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	var out []*dockwellv1.SavedCommand
	err = h.db.View(ctx, func(tx *store.Tx) error {
		var rerr error
		out, rerr = readAllCommands(ctx, tx, p.User.ID)
		return rerr
	})
	if err != nil {
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.ListSavedCommandsResponse{Commands: out}), nil
}

// SaveCommand creates or updates a personal or shared saved terminal command.
func (h *Handler) SaveCommand(ctx context.Context, req *connect.Request[dockwellv1.SaveCommandRequest]) (*connect.Response[dockwellv1.SaveCommandResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	name, body, group, err := validateCommandFields(req.Msg.GetName(), req.Msg.GetCommand(), req.Msg.GetGroup())
	if err != nil {
		return nil, authapi.Invalid(err)
	}
	if secret, why := LooksLikeSecret(body); secret && !req.Msg.GetAllowPlainTextSecret() {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument,
			fmt.Errorf("this command looks like it contains a secret (%s). Saved commands are kept in plain text in Dockwell's database; remove the secret or confirm plain-text storage", why),
			ReasonLooksLikeSecret)
	}
	client, _ := authapi.ClientFrom(ctx)
	surface := client.Surface(p)
	actor := p.Actor()
	now := h.now().UTC()
	targetShared := req.Msg.GetShared()
	cmdID := strings.TrimSpace(req.Msg.GetId())
	var out *dockwellv1.SavedCommand
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		personal, err := readCommandsKey(ctx, tx, userCommandsKey(p.User.ID))
		if err != nil {
			return err
		}
		shared, err := readCommandsKey(ctx, tx, sharedCommandsKey)
		if err != nil {
			return err
		}
		var (
			found      bool
			fromShared bool
			existing   storedCommand
			saved      storedCommand
		)
		if cmdID != "" {
			if idx := indexCommand(personal, cmdID); idx >= 0 {
				found, fromShared, existing = true, false, personal[idx]
				personal = slices.Delete(personal, idx, idx+1)
			} else if idx := indexCommand(shared, cmdID); idx >= 0 {
				found, fromShared, existing = true, true, shared[idx]
				shared = slices.Delete(shared, idx, idx+1)
			}
			if !found {
				return authapi.ReasonError(connect.CodeNotFound,
					fmt.Errorf("saved command %s wasn't found. It may have been deleted", cmdID), "not_found")
			}
			var createdBy, updatedBy string
			if targetShared {
				if fromShared {
					createdBy = cmp.Or(existing.CreatedBy, actor)
				} else {
					createdBy = actor
				}
				updatedBy = actor
			}
			saved = storedCommand{
				ID:        existing.ID,
				Name:      name,
				Command:   body,
				Group:     group,
				CreatedAt: existing.CreatedAt,
				UpdatedAt: now,
				CreatedBy: createdBy,
				UpdatedBy: updatedBy,
			}
		} else {
			saved = storedCommand{
				ID:        id.New(id.TerminalCommand),
				Name:      name,
				Command:   body,
				Group:     group,
				CreatedAt: now,
				UpdatedAt: now,
			}
			if targetShared {
				saved.CreatedBy = actor
				saved.UpdatedBy = actor
			}
		}
		if targetShared {
			if len(shared) >= MaxSavedCommands {
				return authapi.ReasonError(connect.CodeResourceExhausted,
					fmt.Errorf("this install already has %d shared saved commands, which is the limit. Delete one before adding another", MaxSavedCommands), "too_many_commands")
			}
			shared = append(shared, saved)
		} else {
			if len(personal) >= MaxSavedCommands {
				return authapi.ReasonError(connect.CodeResourceExhausted,
					fmt.Errorf("you already have %d personal saved commands, which is the limit. Delete one before adding another", MaxSavedCommands), "too_many_commands")
			}
			personal = append(personal, saved)
		}
		if !targetShared || (found && !fromShared) {
			if err := writeCommandsKey(ctx, tx, userCommandsKey(p.User.ID), personal); err != nil {
				return err
			}
		}
		if targetShared || (found && fromShared) {
			if err := writeCommandsKey(ctx, tx, sharedCommandsKey, shared); err != nil {
				return err
			}
		}
		switch {
		case targetShared:
			prevName := ""
			if found && fromShared {
				prevName = existing.Name
			}
			if err := tx.RecordEvent(ctx, store.Event{
				ID:      id.New(id.Event),
				At:      now,
				Actor:   actor,
				Action:  EventCommandSave,
				Subject: saved.ID,
				Surface: surface,
				Detail:  CommandTold{ID: saved.ID, Name: saved.Name, PreviousName: prevName, Group: saved.Group}.String(),
			}); err != nil {
				return err
			}
		case found && fromShared:
			// Moving a shared command out of shared deletes it for every other admin.
			if err := tx.RecordEvent(ctx, store.Event{
				ID:      id.New(id.Event),
				At:      now,
				Actor:   actor,
				Action:  EventCommandDelete,
				Subject: existing.ID,
				Surface: surface,
				Detail:  CommandTold{ID: existing.ID, Name: existing.Name, Group: existing.Group}.String(),
			}); err != nil {
				return err
			}
		}
		out = saved.proto(targetShared, actorNameResolver(ctx, tx))
		return nil
	})
	if err != nil {
		var cerr *connect.Error
		if errors.As(err, &cerr) {
			return nil, err
		}
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.SaveCommandResponse{Command: out}), nil
}

// DeleteSavedCommand removes a personal or shared saved command.
func (h *Handler) DeleteSavedCommand(ctx context.Context, req *connect.Request[dockwellv1.DeleteSavedCommandRequest]) (*connect.Response[dockwellv1.DeleteSavedCommandResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	cmdID := strings.TrimSpace(req.Msg.GetId())
	if cmdID == "" {
		return nil, authapi.Invalid(errors.New("choose a saved command to delete"))
	}
	isShared := req.Msg.GetShared()
	key := userCommandsKey(p.User.ID)
	if isShared {
		key = sharedCommandsKey
	}
	client, _ := authapi.ClientFrom(ctx)
	surface := client.Surface(p)
	actor := p.Actor()
	now := h.now().UTC()
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		list, err := readCommandsKey(ctx, tx, key)
		if err != nil {
			return err
		}
		idx := indexCommand(list, cmdID)
		if idx < 0 {
			return authapi.ReasonError(connect.CodeNotFound,
				fmt.Errorf("saved command %s wasn't found. It may have been deleted already", cmdID), "not_found")
		}
		removed := list[idx]
		list = slices.Delete(list, idx, idx+1)
		if err := writeCommandsKey(ctx, tx, key, list); err != nil {
			return err
		}
		if isShared {
			return tx.RecordEvent(ctx, store.Event{
				ID:      id.New(id.Event),
				At:      now,
				Actor:   actor,
				Action:  EventCommandDelete,
				Subject: removed.ID,
				Surface: surface,
				Detail:  CommandTold{ID: removed.ID, Name: removed.Name, Group: removed.Group}.String(),
			})
		}
		return nil
	})
	if err != nil {
		var cerr *connect.Error
		if errors.As(err, &cerr) {
			return nil, err
		}
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.DeleteSavedCommandResponse{}), nil
}

// ImportSavedCommands imports a batch of saved commands atomically.
func (h *Handler) ImportSavedCommands(ctx context.Context, req *connect.Request[dockwellv1.ImportSavedCommandsRequest]) (*connect.Response[dockwellv1.ImportSavedCommandsResponse], error) {
	p, err := person(ctx)
	if err != nil {
		return nil, err
	}
	inputs := req.Msg.GetCommands()
	if len(inputs) == 0 {
		return nil, authapi.Invalid(errors.New("the file has no commands to import"))
	}
	if len(inputs) > MaxSavedCommands {
		return nil, authapi.Invalid(fmt.Errorf("import at most %d commands at a time", MaxSavedCommands))
	}
	type cleanItem struct {
		name, body, group string
		shared            bool
	}
	clean := make([]cleanItem, 0, len(inputs))
	var secretNames []string
	for i, item := range inputs {
		name, body, group, err := validateCommandFields(item.GetName(), item.GetCommand(), item.GetGroup())
		if err != nil {
			return nil, authapi.Invalid(fmt.Errorf("command %d: %w", i+1, err))
		}
		if secret, _ := LooksLikeSecret(body); secret {
			secretNames = append(secretNames, name)
		}
		clean = append(clean, cleanItem{name: name, body: body, group: group, shared: item.GetShared()})
	}
	if len(secretNames) > 0 && !req.Msg.GetAllowPlainTextSecret() {
		return nil, authapi.ReasonError(connect.CodeInvalidArgument,
			fmt.Errorf("these imported commands look like they contain secrets (%s). Saved commands are kept in plain text in Dockwell's database; remove the secrets or confirm plain-text storage", strings.Join(secretNames, ", ")),
			ReasonLooksLikeSecret)
	}
	client, _ := authapi.ClientFrom(ctx)
	surface := client.Surface(p)
	actor := p.Actor()
	now := h.now().UTC()
	var all []*dockwellv1.SavedCommand
	err = h.db.Update(ctx, func(tx *store.Tx) error {
		personal, err := readCommandsKey(ctx, tx, userCommandsKey(p.User.ID))
		if err != nil {
			return err
		}
		shared, err := readCommandsKey(ctx, tx, sharedCommandsKey)
		if err != nil {
			return err
		}
		addPersonal, addShared := 0, 0
		for _, item := range clean {
			if item.shared {
				addShared++
			} else {
				addPersonal++
			}
		}
		if len(personal)+addPersonal > MaxSavedCommands {
			return authapi.ReasonError(connect.CodeResourceExhausted,
				fmt.Errorf("importing %d personal commands would exceed the %d command limit. Delete some commands or import fewer", addPersonal, MaxSavedCommands), "too_many_commands")
		}
		if len(shared)+addShared > MaxSavedCommands {
			return authapi.ReasonError(connect.CodeResourceExhausted,
				fmt.Errorf("importing %d shared commands would exceed the %d command limit. Delete some commands or import fewer", addShared, MaxSavedCommands), "too_many_commands")
		}
		for _, item := range clean {
			entry := storedCommand{
				ID:        id.New(id.TerminalCommand),
				Name:      item.name,
				Command:   item.body,
				Group:     item.group,
				CreatedAt: now,
				UpdatedAt: now,
			}
			if item.shared {
				entry.CreatedBy = actor
				entry.UpdatedBy = actor
				shared = append(shared, entry)
				if err := tx.RecordEvent(ctx, store.Event{
					ID:      id.New(id.Event),
					At:      now,
					Actor:   actor,
					Action:  EventCommandSave,
					Subject: entry.ID,
					Surface: surface,
					Detail:  CommandTold{ID: entry.ID, Name: entry.Name, Group: entry.Group}.String(),
				}); err != nil {
					return err
				}
			} else {
				personal = append(personal, entry)
			}
		}
		if addPersonal > 0 {
			if err := writeCommandsKey(ctx, tx, userCommandsKey(p.User.ID), personal); err != nil {
				return err
			}
		}
		if addShared > 0 {
			if err := writeCommandsKey(ctx, tx, sharedCommandsKey, shared); err != nil {
				return err
			}
		}
		all = mergeCommands(personal, shared, actorNameResolver(ctx, tx))
		return nil
	})
	if err != nil {
		var cerr *connect.Error
		if errors.As(err, &cerr) {
			return nil, err
		}
		return nil, h.rpcError(ctx, err)
	}
	return connect.NewResponse(&dockwellv1.ImportSavedCommandsResponse{
		Commands: all,
		Imported: uint32(len(clean)), //nolint:gosec // bounded by MaxSavedCommands (200)
	}), nil
}

func validateCommandFields(rawName, rawBody, rawGroup string) (name, body, group string, err error) {
	name = strings.TrimSpace(rawName)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > MaxCommandNameLen || hasControlChars(name) {
		return "", "", "", fmt.Errorf("name must be 1 to %d printable characters", MaxCommandNameLen)
	}
	// Fold CRLF line endings to LF, then strip trailing blank newlines so a
	// saved command never ends with an accidental Enter.
	body = strings.ReplaceAll(rawBody, "\r\n", "\n")
	body = strings.TrimRight(body, "\n")
	if strings.TrimSpace(body) == "" || !utf8.ValidString(body) || len(body) > MaxCommandBodyLen || hasBodyControlChars(body) {
		return "", "", "", fmt.Errorf("command must be 1 to %d bytes of valid UTF-8 with no control characters other than newlines and tabs", MaxCommandBodyLen)
	}
	group = strings.TrimSpace(rawGroup)
	if !utf8.ValidString(group) || utf8.RuneCountInString(group) > MaxCommandGroupLen || hasControlChars(group) {
		return "", "", "", fmt.Errorf("group must be at most %d printable characters", MaxCommandGroupLen)
	}
	return name, body, group, nil
}

func hasControlChars(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f || hiddenRune(r) })
}

// hiddenRune is a character that changes what a command does without showing:
// C1 controls (0x80-0x9f, which some programs read as escapes), marks that
// reorder how text is drawn ("Trojan Source"), and zero-width characters. A
// shared command could then read as one thing and run another (re-check N1).
func hiddenRune(r rune) bool {
	switch {
	case r >= 0x80 && r <= 0x9f,
		r == 0x061c, r == 0x200e, r == 0x200f,
		r >= 0x202a && r <= 0x202e,
		r >= 0x2066 && r <= 0x2069,
		r >= 0x200b && r <= 0x200d, r == 0x2060, r == 0xfeff:
		return true
	}
	return false
}

// hasBodyControlChars reports any ASCII control character other than LF and TAB,
// including bare CR, ESC, NUL, ^C, ^U, ^D and DEL.
func hasBodyControlChars(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		if r == '\n' || r == '\t' {
			return false
		}
		return r < 0x20 || r == 0x7f || hiddenRune(r)
	})
}

func readCommandsKey(ctx context.Context, tx *store.Tx, key string) ([]storedCommand, error) {
	raw, err := tx.GetSetting(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []storedCommand
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decoding saved commands: %w", err)
	}
	return list, nil
}

func writeCommandsKey(ctx context.Context, tx *store.Tx, key string, list []storedCommand) error {
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return tx.PutSetting(ctx, key, raw)
}

func readAllCommands(ctx context.Context, tx *store.Tx, userID string) ([]*dockwellv1.SavedCommand, error) {
	personal, err := readCommandsKey(ctx, tx, userCommandsKey(userID))
	if err != nil {
		return nil, err
	}
	shared, err := readCommandsKey(ctx, tx, sharedCommandsKey)
	if err != nil {
		return nil, err
	}
	return mergeCommands(personal, shared, actorNameResolver(ctx, tx)), nil
}

func mergeCommands(personal, shared []storedCommand, resolveActor func(string) string) []*dockwellv1.SavedCommand {
	out := make([]*dockwellv1.SavedCommand, 0, len(personal)+len(shared))
	for i := range personal {
		out = append(out, personal[i].proto(false, nil))
	}
	for i := range shared {
		out = append(out, shared[i].proto(true, resolveActor))
	}
	return out
}

func indexCommand(list []storedCommand, cmdID string) int {
	for i := range list {
		if list[i].ID == cmdID {
			return i
		}
	}
	return -1
}

// Secret-like patterns in commands. Kept aligned with the panel's live check.
var (
	rePEMKey      = regexp.MustCompile(`-{5}BEGIN [A-Z0-9 ]*PRIVATE KEY-{5}`)
	reTokenPrefix = regexp.MustCompile(`\b(ghp_[A-Za-z0-9]{20,}|gho_[A-Za-z0-9]{20,}|ghu_[A-Za-z0-9]{20,}|ghs_[A-Za-z0-9]{20,}|ghr_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|sk-[A-Za-z0-9_-]{20,}|sk_(?:live|test)_[A-Za-z0-9]{16,}|rk_(?:live|test)_[A-Za-z0-9]{16,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,}|SG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}|hf_[A-Za-z0-9]{20,}|npm_[A-Za-z0-9]{20,}|AIza[0-9A-Za-z_-]{35})\b`)
	reJWT         = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
	reURLCreds    = regexp.MustCompile(`(?i)\b(?:https?|postgres(?:ql)?|mysql|mariadb|mongodb(?:\+srv)?|rediss?|amqps?|s?ftp)://([^/\s:@]+):([^/\s@]+)@`)
	reAuthHeader  = regexp.MustCompile(`(?i)Authorization\s*:\s*(?:Bearer|Basic|Token)\s+([^\s'"]+)`)
	reEnvAssign   = regexp.MustCompile(`(?i)\b[A-Z0-9_]*(?:PASSWORD|PASSWD|SECRET|API_?KEY|ACCESS_?KEY|PRIVATE_?KEY|AUTH_?TOKEN|CLIENT_?SECRET|TOKEN)\s*=\s*['"]?([^\s'"]+)`)
	reSecretFlag  = regexp.MustCompile(`(?i)(?:--password|--passwd|--token|--api-key|--secret|--auth-token)(?:=|\s+)['"]?([^\s'"]+)`)
	reCurlAuth    = regexp.MustCompile(`(?i)\bcurl\b[^\n]*?\s(?:-u|--user)\s+['"]?[^:\s'"]+:([^\s'"]+)`)
	reDBPassFlag  = regexp.MustCompile(`(?i)\b(?:mysql|mariadb|psql|redis-cli)\b[^\n]*?\s(?:-p|-a)\s*['"]?([^\s'"-]+)`)
)

// LooksLikeSecret reports whether a command appears to embed a literal
// credential, API key, token, or private key, returning a short human reason.
func LooksLikeSecret(command string) (bool, string) {
	if rePEMKey.MatchString(command) {
		return true, "private key material"
	}
	if reTokenPrefix.MatchString(command) {
		return true, "an API key or token"
	}
	if reJWT.MatchString(command) {
		return true, "a signed bearer token"
	}
	if m := reURLCreds.FindStringSubmatch(command); len(m) == 3 && !isPlaceholderValue(m[2]) {
		return true, "a password in a URL"
	}
	if m := reAuthHeader.FindStringSubmatch(command); len(m) == 2 && !isPlaceholderValue(m[1]) {
		return true, "an Authorization header"
	}
	if m := reEnvAssign.FindStringSubmatch(command); len(m) == 2 && !isPlaceholderValue(m[1]) {
		return true, "an inline password, token or secret"
	}
	if m := reSecretFlag.FindStringSubmatch(command); len(m) == 2 && !isPlaceholderValue(m[1]) {
		return true, "a password or token flag"
	}
	if m := reCurlAuth.FindStringSubmatch(command); len(m) == 2 && !isPlaceholderValue(m[1]) {
		return true, "credentials in curl -u"
	}
	if m := reDBPassFlag.FindStringSubmatch(command); len(m) == 2 && !isPlaceholderValue(m[1]) {
		return true, "a database password flag"
	}
	return false, ""
}

func isPlaceholderValue(v string) bool {
	v = strings.TrimSpace(strings.Trim(v, `"'`))
	if v == "" {
		return true
	}
	if strings.HasPrefix(v, "$") || (strings.HasPrefix(v, "<") && strings.HasSuffix(v, ">")) || (strings.HasPrefix(v, "{{") && strings.HasSuffix(v, "}}")) {
		return true
	}
	lower := strings.ToLower(v)
	switch lower {
	case "...", "xxx", "xxxx", "changeme", "change_me", "example", "placeholder", "redacted", "none", "null", "true", "false":
		return true
	}
	return strings.HasPrefix(lower, "your_") || strings.HasPrefix(lower, "your-")
}
