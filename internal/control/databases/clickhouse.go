// SPDX-License-Identifier: AGPL-3.0-only

package databases

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/catalog"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/changes"
	"github.com/dockwell/dockwell/internal/database"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/service"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/revision"
	"github.com/dockwell/dockwell/internal/store"
)

func (h *Handler) changeClickHousePassword(ctx context.Context, db *placed, user string) (*dockwellv1.Change, error) {
	h.read(ctx, []*placed{db})
	if i := slices.IndexFunc(db.db.GetReadings(), func(r *dockwellv1.DatabaseReading) bool {
		return r.GetUnread() == "" && r.GetUser() != ""
	}); i >= 0 {
		db.firstUser = db.db.GetReadings()[i].GetUser()
	}
	if user != "" && user != db.firstUser {
		return nil, invalidField("user", errors.New("only the ClickHouse service's first user can have its password changed here"))
	}
	if strings.Contains(database.Tag(db.db.GetImage()), "distroless") {
		return nil, authapi.ReasonError(connect.CodeFailedPrecondition, errors.New("this ClickHouse image has no shell to prepare its password config. Use the official Alpine image"), "no_shell")
	}
	if err := database.CheckSQLName("user", db.firstUser); err != nil {
		return nil, invalidField("user", err)
	}

	password, err := generate()
	if err != nil {
		return nil, err
	}
	env := db.ref.Scope
	secret := db.db.GetPasswordSecret()
	var edits []*dockwellv1.Edit
	err = h.db.View(ctx, func(tx *store.Tx) error {
		if secret == "" {
			var err error
			secret, err = freeSecret(ctx, tx, env, strings.ToUpper(strings.ReplaceAll(db.ref.Name, "-", "_"))+"_CLICKHOUSE_PASSWORD")
			if err != nil {
				return err
			}
		}
		edits = append(edits, clickHouseSecret(env, secret, &resourcev1.Secret{Value: password}))
		spec := proto.CloneOf(db.spec)
		if prepareClickHousePassword(spec, db.firstUser, secret) {
			edits = append(edits, serviceEdit(db.ref, spec, db.revision))
		}
		if db.db.GetPasswordSecret() == "" {
			linkEdits, err := h.clickHouseLinkPasswords(ctx, tx, db, secret)
			if err != nil {
				return err
			}
			edits = append(edits, linkEdits...)
		}
		clientEdits, err := h.clickHouseClientEdits(ctx, tx, db, secret)
		edits = append(edits, clientEdits...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return h.changes.Propose(ctx, "Set the ClickHouse password in "+db.ref.Name, edits, nil, changes.Also{Notes: []revision.Note{{
		Reason: "clickhouse_password", Ref: db.ref,
		Message: "ClickHouse restarts with a password hash on its volume. Clients with their own settings need the new password.",
	}}})
}

// prepareClickHousePassword puts the image's first account under the same hashed
// users config as a new install. Keep a catalog app's existing entrypoint wrapper.
func prepareClickHousePassword(svc *resourcev1.Service, user, secret string) bool {
	app := svc.GetApp()
	cmd := app.GetCommand()
	if database.ClickHouseAuthWrapped(cmd) && cmd[2] == database.ClickHouseAuthSetup && svc.GetSecrets()["CLICKHOUSE_PASSWORD"] == secret {
		return false
	}
	app.Command = database.WrapClickHouseAuth(cmd)
	if app.Env == nil {
		app.Env = map[string]string{}
	}
	if svc.Secrets == nil {
		svc.Secrets = map[string]string{}
	}
	for _, name := range []string{"CLICKHOUSE_PASSWORD", "CLICKHOUSE_PASSWORD_FILE", "CLICKHOUSE_USER", "CLICKHOUSE_SKIP_USER_SETUP"} {
		delete(app.Env, name)
		delete(svc.Secrets, name)
		delete(svc.Variables, name)
	}
	// Recreate the settings normally written by the entrypoint. With setup
	// already skipped, any existing users XML keeps its own authorization settings.
	var imageSettings string
	if app.Env["CLICKHOUSE_SKIP_USER_SETUP"] != "1" {
		const management = "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT"
		if app.Env[management] == "" && svc.GetSecrets()[management] == "" && svc.GetVariables()[management] == "" {
			app.Env[management] = "0"
		}
		imageSettings = `<profile>default</profile><quota>default</quota><networks replace="replace"><ip>::/0</ip></networks><access_management from_env="CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT"/>`
	}
	app.Env["CLICKHOUSE_SKIP_USER_SETUP"], app.Env["CLICKHOUSE_USER"] = "1", user
	svc.Secrets["CLICKHOUSE_PASSWORD"] = secret
	app.Mounts = slices.DeleteFunc(app.Mounts, func(m *resourcev1.Mount) bool { return m.GetTarget() == database.ClickHouseAuthTarget })
	removeDefault := ""
	if user != "default" {
		removeDefault = `<default remove="remove"/>`
	}
	auth := fmt.Sprintf(`<clickhouse><users>%s<%s><password remove="remove"/><password_sha256_hex from_env="DOCKWELL_CLICKHOUSE_PASSWORD_SHA256"/>%s</%s></users></clickhouse>`, removeDefault, user, imageSettings, user)
	app.Mounts = append(app.Mounts, &resourcev1.Mount{
		Target: database.ClickHouseAuthTarget, ReadOnly: true,
		Source: &resourcev1.Mount_File{File: &resourcev1.MountFile{Content: auth}},
	})
	return true
}

// BoundClickHouseLogs mounts the bundled logging config while retaining the
// taken-on service's existing query settings and data.
func (h *Handler) BoundClickHouseLogs(ctx context.Context, req *connect.Request[dockwellv1.BoundClickHouseLogsRequest]) (*connect.Response[dockwellv1.BoundClickHouseLogsResponse], error) {
	m := req.Msg
	if m.GetId() == "" {
		return nil, invalidField("id", errors.New("pass the ClickHouse service's ID or name"))
	}
	list, err := h.list(ctx)
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	db, err := h.pick(ctx, list, m.GetId(), m.GetProject(), m.GetEnvironment())
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	if !database.Of(db.db.GetEngine()).ClickHouse() {
		return nil, authapi.Invalid(errors.New("this logging plan is for ClickHouse"))
	}
	config, err := catalog.ClickHouseLogConfig()
	if err != nil {
		return nil, h.fail(ctx, err)
	}
	spec := proto.CloneOf(db.spec)
	app := spec.GetApp()
	app.Mounts = slices.DeleteFunc(app.GetMounts(), func(m *resourcev1.Mount) bool { return m.GetTarget() == database.ClickHouseLogsTarget })
	app.Mounts = append(app.Mounts, &resourcev1.Mount{
		Target: database.ClickHouseLogsTarget, ReadOnly: true,
		Source: &resourcev1.Mount_File{File: &resourcev1.MountFile{Content: config}},
	})
	change, err := h.changes.Propose(ctx, "Bound ClickHouse logs for "+db.ref.Name, []*dockwellv1.Edit{serviceEdit(db.ref, spec, db.revision)}, nil, changes.Also{Notes: []revision.Note{{
		Reason: "clickhouse_logs", Ref: db.ref,
		Message: "ClickHouse restarts with rotating warning logs and stops writing system log tables. Existing logs stay on disk.",
	}}})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&dockwellv1.BoundClickHouseLogsResponse{Change: change}), nil
}

// clickHouseLinkPasswords updates managed URLs in same-environment links that
// were made before the server had a password.
func (h *Handler) clickHouseLinkPasswords(ctx context.Context, tx *store.Tx, db *placed, password string) ([]*dockwellv1.Edit, error) {
	rows, err := tx.ListResources(ctx, db.ref.Scope, linkKind)
	if err != nil {
		return nil, err
	}
	var edits []*dockwellv1.Edit
	for _, row := range rows {
		link := &resourcev1.Link{}
		if row.Spec == nil || proto.Unmarshal(row.Spec, link) != nil || link.GetService() != db.ref.Name ||
			(link.GetMeetsEnvironment() != "" && link.GetMeetsEnvironment() != db.ref.Scope) ||
			link.GetEngine() != resourcev1.LinkEngine_LINK_ENGINE_CLICKHOUSE {
			continue
		}
		for _, set := range link.GetSets() {
			if set.GetPart() != resourcev1.LinkPart_LINK_PART_URL || set.GetTemplate() == "" || strings.Contains(set.GetTemplate(), "{password}") {
				continue
			}
			secret, err := tx.GetResource(ctx, string(variable.SecretKind), row.Scope, set.GetName())
			if errors.Is(err, store.ErrNotFound) || err == nil && secret.Spec == nil {
				continue
			}
			if err != nil {
				return nil, err
			}
			decoded, unreadable, err := h.keeper.DecodeEach(ctx, tx, []store.StoredSpec{
				{At: store.InRevision(secret.ID, secret.Kind, secret.Revision), Spec: secret.Spec},
			})
			if err != nil {
				return nil, err
			}
			if unreadable[0] != nil {
				return nil, unreadable[0]
			}
			spec, _ := decoded[0].(*resourcev1.Secret)
			tmpl := spec.GetFrom().GetTemplate()
			if tmpl == nil {
				continue
			}
			text, ok := clickHouseURLWithAuth(tmpl.GetText(), db.firstUser, password)
			if !ok {
				continue
			}
			tmpl.Text = text
			tmpl.Names = append(tmpl.Names, clickHouseURLCredential(password))
			edit := clickHouseSecret(row.Scope, secret.Name, spec)
			edit.BaseRevision = int64(secret.Revision)
			edits = append(edits, edit)
		}
	}
	return edits, nil
}

func clickHouseURLWithAuth(text, user, password string) (string, bool) {
	scheme, rest, ok := strings.Cut(text, "://")
	if !ok {
		return "", false
	}
	authority := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority = rest[:i]
	}
	if userinfo, host, hasUser := strings.Cut(authority, "@"); hasUser {
		user, _, _ = strings.Cut(userinfo, ":")
		rest = host + strings.TrimPrefix(rest, authority)
	} else {
		user = url.User(user).String()
	}
	return scheme + "://" + user + ":${" + password + "}@" + rest, true
}

func clickHouseURLCredential(password string) *resourcev1.TemplateName {
	return &resourcev1.TemplateName{Name: password, Secret: true, Encoding: resourcev1.Encoding_ENCODING_USERINFO}
}

var clickHouseURLKeys = []string{"CLICKHOUSE_DATABASE_URL", "CLICKHOUSE_URL"}

func clickHouseClientURL(raw, host string) *url.URL {
	at, err := url.Parse(raw)
	if err == nil && at.Hostname() == host && (at.Scheme == "http" || at.Scheme == "https" || at.Scheme == "clickhouse") {
		return at
	}
	return nil
}

// clickHouseLiteralClient finds a companion that signs in with its own URL,
// before there is a Dockwell link or a password secret to follow.
func (h *Handler) clickHouseLiteralClient(ctx context.Context, tx *store.Tx, db *placed) (string, error) {
	rows, err := tx.ListResources(ctx, db.ref.Scope, string(service.Kind))
	if err != nil {
		return "", err
	}
	var stored []store.StoredSpec
	var names []string
	for _, row := range rows {
		if row.Name != db.ref.Name && row.Spec != nil {
			stored = append(stored, store.StoredSpec{At: store.InRevision(row.ID, row.Kind, row.Revision), Spec: row.Spec})
			names = append(names, row.Name)
		}
	}
	decoded, failures, err := h.keeper.DecodeEach(ctx, tx, stored)
	if err != nil {
		return "", err
	}
	for i, spec := range decoded {
		if failures[i] != nil {
			return "", failures[i]
		}
		svc, _ := spec.(*resourcev1.Service)
		for _, key := range []string{"CLICKHOUSE_DATABASE_URL", "CLICKHOUSE_URL", "CLICKHOUSE_HOST", "CLICKHOUSE_ADDRESS"} {
			value, err := h.clickHouseClientSetting(ctx, tx, db.ref.Scope, key, svc)
			if err != nil {
				return "", err
			}
			if clickHouseClientURL(value, db.ref.Name) != nil {
				return names[i], nil
			}
			if key == "CLICKHOUSE_HOST" || key == "CLICKHOUSE_ADDRESS" {
				if endpoint, err := url.Parse("//" + value); err == nil && endpoint.Hostname() == db.ref.Name {
					return names[i], nil
				}
			}
		}
	}
	return "", nil
}

func (h *Handler) clickHouseClientSetting(ctx context.Context, tx *store.Tx, env, key string, svc *resourcev1.Service) (string, error) {
	kind := variable.VariableKind
	name, bound := svc.GetVariables()[key]
	if secret, ok := svc.GetSecrets()[key]; ok {
		kind, name, bound = variable.SecretKind, secret, true
	}
	if !bound {
		return svc.GetApp().GetEnv()[key], nil
	}
	row, err := tx.GetResource(ctx, string(kind), env, cmp.Or(name, key))
	if errors.Is(err, store.ErrNotFound) || err == nil && row.Spec == nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	decoded, unreadable, err := h.keeper.DecodeEach(ctx, tx, []store.StoredSpec{{
		At: store.InRevision(row.ID, row.Kind, row.Revision), Spec: row.Spec,
	}})
	if err != nil {
		return "", err
	}
	if unreadable[0] != nil {
		return "", unreadable[0]
	}
	switch value := decoded[0].(type) {
	case *resourcev1.Secret:
		return value.GetValue(), nil
	case *resourcev1.Variable:
		return value.GetValue(), nil
	default:
		return "", nil
	}
}

// clickHouseClientEdits keeps app companions using a literal ClickHouse URL on
// their own network signed in. Linked credentials already follow the secret.
func (h *Handler) clickHouseClientEdits(ctx context.Context, tx *store.Tx, db *placed, password string) ([]*dockwellv1.Edit, error) {
	rows, err := tx.ListResources(ctx, db.ref.Scope, string(service.Kind))
	if err != nil {
		return nil, err
	}
	var edits []*dockwellv1.Edit
	for i := range rows {
		row := &rows[i]
		if row.Name == db.ref.Name || row.Spec == nil {
			continue
		}
		decoded, unreadable, err := h.keeper.DecodeEach(ctx, tx, []store.StoredSpec{
			{At: store.InRevision(row.ID, row.Kind, row.Revision), Spec: row.Spec},
		})
		if err != nil {
			return nil, err
		}
		if unreadable[0] != nil {
			return nil, unreadable[0]
		}
		svc, _ := decoded[0].(*resourcev1.Service)
		changed := false
		for _, key := range clickHouseURLKeys {
			at := clickHouseClientURL(svc.GetApp().GetEnv()[key], db.ref.Name)
			if at == nil || at.User.Username() != "" && at.User.Username() != db.firstUser {
				continue
			}
			dest, err := freeSecret(ctx, tx, db.ref.Scope, strings.ToUpper(strings.ReplaceAll(row.Name, "-", "_"))+"_"+key)
			if err != nil {
				return nil, err
			}
			at.User = nil
			text, _ := clickHouseURLWithAuth(strings.ReplaceAll(at.String(), "$", "$$"), db.firstUser, password)
			edits = append(edits, clickHouseSecret(db.ref.Scope, dest, &resourcev1.Secret{From: &resourcev1.ValueFrom{
				Source: &resourcev1.ValueFrom_Template{Template: &resourcev1.ValueTemplate{
					Text: text, Names: []*resourcev1.TemplateName{clickHouseURLCredential(password)},
				}},
			}}))
			delete(svc.GetApp().Env, key)
			svc.ShownEnv = slices.DeleteFunc(svc.GetShownEnv(), func(name string) bool { return name == key })
			if svc.Secrets == nil {
				svc.Secrets = map[string]string{}
			}
			svc.Secrets[key] = dest
			changed = true
		}
		if changed {
			ref := resource.Ref{Kind: service.Kind, Scope: db.ref.Scope, Name: row.Name}
			edits = append(edits, serviceEdit(ref, svc, row.Revision))
		}
	}
	return edits, nil
}

func clickHouseSecret(environment, name string, spec *resourcev1.Secret) *dockwellv1.Edit {
	return &dockwellv1.Edit{
		Resource: &dockwellv1.ResourceRef{Kind: string(variable.SecretKind), Scope: environment, Name: name},
		Spec:     &resourcev1.Spec{Kind: &resourcev1.Spec_Secret{Secret: spec}},
	}
}
