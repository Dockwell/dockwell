// SPDX-License-Identifier: AGPL-3.0-only

package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/auth"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/store"
)

// ActionAutoUpdate is the activity log's word for a change of what new apps'
// automatic updates start as (ADR 0080). Its value is the level: "patch",
// "minor", "any", or empty for off.
const ActionAutoUpdate = "settings.auto_update"

// LevelWord is an automatic update's level as a word, for the activity log.
func LevelWord(a *resourcev1.AutoUpdate) string {
	if a.GetLevel() == resourcev1.AutoUpdate_LEVEL_UNSPECIFIED {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(a.GetLevel().String(), "LEVEL_"))
}

// NewAppsAutoUpdate reads what new apps' automatic updates start as, or nil for
// off.
func NewAppsAutoUpdate(ctx context.Context, tx *store.Tx) (*resourcev1.AutoUpdate, error) {
	b, err := tx.GetSetting(ctx, store.SettingAutoUpdate)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	a := &resourcev1.AutoUpdate{}
	if err := protojson.Unmarshal(b, a); err != nil {
		return nil, fmt.Errorf("can't read what new apps' automatic updates start as: %w. Set it again in Settings", err)
	}
	return a, nil
}

// SetAutoUpdateDefault sets what new apps' automatic updates start as.
func (h *Handler) SetAutoUpdateDefault(ctx context.Context, req *connect.Request[dockwellv1.SetAutoUpdateDefaultRequest]) (*connect.Response[dockwellv1.SetAutoUpdateDefaultResponse], error) {
	p, ok := authapi.PrincipalFrom(ctx)
	if !ok {
		return nil, authapi.ReasonError(connect.CodeUnauthenticated, auth.ErrUnauthenticated, authapi.ReasonUnauthenticated)
	}
	client, _ := authapi.ClientFrom(ctx)
	a := proto.CloneOf(req.Msg.GetAutoUpdate())
	if a.GetLevel() == resourcev1.AutoUpdate_LEVEL_UNSPECIFIED {
		a = nil
	}
	var out *dockwellv1.Settings
	err := h.db.Update(ctx, func(tx *store.Tx) error {
		if a != nil && a.GetTimeZone() == "" {
			zone, err := getString(ctx, tx, keyTimeZone)
			if err != nil {
				return err
			}
			a.TimeZone = zone
		}
		if a != nil {
			if err := service.CheckAutoUpdate(a); err != nil {
				return authapi.Invalid(err)
			}
		}
		previous, err := NewAppsAutoUpdate(ctx, tx)
		if err != nil {
			return err
		}
		if !proto.Equal(previous, a) {
			if a == nil {
				err = tx.DeleteSetting(ctx, store.SettingAutoUpdate)
			} else {
				var b []byte
				if b, err = protojson.Marshal(a); err == nil {
					err = tx.PutSetting(ctx, store.SettingAutoUpdate, b)
				}
			}
			if err != nil {
				return err
			}
			detail, err := json.Marshal(Change{Value: LevelWord(a), Previous: LevelWord(previous)})
			if err != nil {
				return err
			}
			if err := tx.RecordEvent(ctx, store.Event{
				ID: id.New(id.Event), At: h.now(), Actor: p.Actor(), Action: ActionAutoUpdate, Surface: client.Surface(p), Detail: string(detail),
			}); err != nil {
				return err
			}
		}
		out, err = h.read(ctx, tx)
		return err
	})
	if err != nil {
		var said *connect.Error
		if errors.As(err, &said) {
			return nil, said
		}
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.SetAutoUpdateDefaultResponse{Settings: out}), nil
}
