// SPDX-License-Identifier: AGPL-3.0-only

// Package settings serves dockwell.v1.SettingsService: the install's own
// settings, which aren't managed state, so a change is on the activity log with
// what it replaced rather than a revision with a plan; how the panel is served,
// as dockwell run was started; and what the control plane is, and how it's
// doing. See docs/adr/0042-the-installs-settings.md.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // a server's own zone files aren't always there
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/store"
)

// Where each setting is kept in the store's settings.
const (
	keyName     = store.SettingInstallName
	keyTimeZone = store.SettingTimeZone
)

// What goes on the activity log, and which setting each event is of.
const (
	ActionName     = "settings.name"
	ActionTimeZone = "settings.time_zone"
)

// Actions are every action this package puts on the activity log.
var Actions = []string{ActionName, ActionTimeZone}

// MaxName is the most characters a name holds.
const MaxName = 64

// DefaultTimeZone is the zone new schedules start in until someone sets one.
const DefaultTimeZone = "UTC"

// Serving is how the panel is served, as dockwell run was started.
type Serving struct {
	// Listen is the loopback address the panel always answers on.
	Listen string
	// Domain is the domain it also answers on over HTTPS, if any.
	Domain string
	// HTTPSListen is where Dockwell serves that domain itself, if it does.
	HTTPSListen string
	// Proxied is true when the server's ingress serves the domain instead.
	Proxied bool
}

// Handler implements SettingsService.
type Handler struct {
	db      *store.DB
	serving Serving
	plane   Plane
	now     func() time.Time
}

var _ dockwellv1connect.SettingsServiceHandler = (*Handler)(nil)

// New returns a handler that keeps settings in db, says the panel is served as
// serving has it, and the control plane is as plane has it.
func New(db *store.DB, serving Serving, plane Plane) *Handler {
	return &Handler{db: db, serving: serving, plane: plane, now: time.Now}
}

// Change is what an event of a setting keeps: the value, and the one it replaced.
type Change struct {
	Value    string `json:"value,omitempty"`
	Previous string `json:"previous,omitempty"`
}

// ReadEvent says what an event of a setting changed, for the timeline.
func ReadEvent(action, detail string) (*dockwellv1.EventSetting, bool) {
	var setting string
	switch action {
	case ActionName:
		setting = "name"
	case ActionTimeZone:
		setting = "time_zone"
	default:
		return nil, false
	}
	var c Change
	if err := json.Unmarshal([]byte(detail), &c); err != nil {
		return nil, false
	}
	return &dockwellv1.EventSetting{Setting: setting, Value: c.Value, Previous: c.Previous}, true
}

// GetSettings returns the install's settings, and how its panel is served.
func (h *Handler) GetSettings(ctx context.Context, _ *connect.Request[dockwellv1.GetSettingsRequest]) (*connect.Response[dockwellv1.GetSettingsResponse], error) {
	var out *dockwellv1.Settings
	err := h.db.View(ctx, func(tx *store.Tx) error {
		var err error
		out, err = h.read(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.GetSettingsResponse{Settings: out}), nil
}

// SetInstallName names the install.
func (h *Handler) SetInstallName(ctx context.Context, req *connect.Request[dockwellv1.SetInstallNameRequest]) (*connect.Response[dockwellv1.SetInstallNameResponse], error) {
	name := strings.TrimSpace(req.Msg.GetName())
	if err := checkName(name); err != nil {
		return nil, authapi.Invalid(err)
	}
	out, err := h.set(ctx, keyName, ActionName, name)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.SetInstallNameResponse{Settings: out}), nil
}

// SetTimeZone sets the zone new schedules start in.
func (h *Handler) SetTimeZone(ctx context.Context, req *connect.Request[dockwellv1.SetTimeZoneRequest]) (*connect.Response[dockwellv1.SetTimeZoneResponse], error) {
	zone := strings.TrimSpace(req.Msg.GetTimeZone())
	if zone != "" {
		if strings.EqualFold(zone, "local") || zone != strings.TrimPrefix(zone, "/") {
			return nil, authapi.Invalid(fmt.Errorf("%q isn't a time zone. Name one as the IANA does, such as Europe/Amsterdam or UTC", zone))
		}
		if _, err := time.LoadLocation(zone); err != nil {
			return nil, authapi.Invalid(fmt.Errorf("%q isn't a time zone Dockwell knows. Name one as the IANA does, such as Europe/Amsterdam or UTC", zone))
		}
	}
	if zone == DefaultTimeZone {
		zone = ""
	}
	out, err := h.set(ctx, keyTimeZone, ActionTimeZone, zone)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.SetTimeZoneResponse{Settings: out}), nil
}

// checkName says why a name can't be one, or nothing when it can.
func checkName(name string) error {
	if n := utf8.RuneCountInString(name); n > MaxName {
		return fmt.Errorf("that name is %d characters, and a name holds %d at most. Choose a shorter one", n, MaxName)
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return errors.New("a name can't hold a line break or another control character. Write it on one line")
		}
	}
	return nil
}

// set keeps value under key, or clears it when value is empty, and puts what it
// replaced on the activity log, unless it's the same as it was.
func (h *Handler) set(ctx context.Context, key, action, value string) (*dockwellv1.Settings, error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	client, _ := authapi.ClientFrom(ctx)
	var out *dockwellv1.Settings
	err := h.db.Update(ctx, func(tx *store.Tx) error {
		previous, err := getString(ctx, tx, key)
		if err != nil {
			return err
		}
		if previous != value {
			if value == "" {
				err = tx.DeleteSetting(ctx, key)
			} else {
				err = tx.PutSetting(ctx, key, []byte(value))
			}
			if err != nil {
				return err
			}
			detail, err := json.Marshal(Change{Value: value, Previous: previous})
			if err != nil {
				return err
			}
			if err := tx.RecordEvent(ctx, store.Event{
				ID: id.New(id.Event), At: h.now(), Actor: p.Actor(), Action: action, Surface: client.Surface(p), Detail: string(detail),
			}); err != nil {
				return err
			}
		}
		out, err = h.read(ctx, tx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// read reads the settings in tx.
func (h *Handler) read(ctx context.Context, tx *store.Tx) (*dockwellv1.Settings, error) {
	name, err := getString(ctx, tx, keyName)
	if err != nil {
		return nil, err
	}
	zone, err := getString(ctx, tx, keyTimeZone)
	if err != nil {
		return nil, err
	}
	if zone == "" {
		zone = DefaultTimeZone
	}
	out := &dockwellv1.Settings{Name: name, TimeZone: zone, Panel: h.panel()}
	switch org, err := tx.Organization(ctx); {
	case err == nil:
		out.CreateTime = timestamppb.New(org.CreatedAt)
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	return out, nil
}

func (h *Handler) panel() *dockwellv1.PanelServing {
	s := h.serving
	out := &dockwellv1.PanelServing{Listen: s.Listen, Domain: s.Domain, ServedBy: dockwellv1.PanelServedBy_PANEL_SERVED_BY_NONE}
	switch {
	case s.Domain == "":
	case s.Proxied:
		out.ServedBy = dockwellv1.PanelServedBy_PANEL_SERVED_BY_INGRESS
	default:
		out.ServedBy, out.HttpsListen = dockwellv1.PanelServedBy_PANEL_SERVED_BY_DOCKWELL, s.HTTPSListen
	}
	return out
}

// getString reads a setting kept as text, or "" when it isn't set.
func getString(ctx context.Context, tx *store.Tx, key string) (string, error) {
	v, err := tx.GetSetting(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	return string(v), err
}
