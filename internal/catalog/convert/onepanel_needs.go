// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// onePanelEngines are the apps 1Panel's form names, by the engine Dockwell meets a
// need with.
var onePanelEngines = map[string]catalogv1.Engine{
	"mysql":      catalogv1.Engine_ENGINE_MYSQL,
	"mariadb":    catalogv1.Engine_ENGINE_MARIADB,
	"postgres":   catalogv1.Engine_ENGINE_POSTGRESQL,
	"postgresql": catalogv1.Engine_ENGINE_POSTGRESQL,
	"redis":      catalogv1.Engine_ENGINE_REDIS,
	"mongodb":    catalogv1.Engine_ENGINE_MONGODB,
}

// onePanelParts are the variables 1Panel fills from a database server an app uses,
// beside its host, by what each holds.
var onePanelParts = map[string]catalogv1.Part{
	"PANEL_DB_PORT":          catalogv1.Part_PART_PORT,
	"PANEL_DB_NAME":          catalogv1.Part_PART_DATABASE,
	"PANEL_DB_USER":          catalogv1.Part_PART_USER,
	"PANEL_DB_USER_PASSWORD": catalogv1.Part_PART_PASSWORD,
	"PANEL_DB_PASSWORD":      catalogv1.Part_PART_PASSWORD,
}

// onePanelRedisParts are those it fills from a Redis.
var onePanelRedisParts = map[string]catalogv1.Part{
	"PANEL_REDIS_PORT":          catalogv1.Part_PART_PORT,
	"PANEL_REDIS_ROOT_PASSWORD": catalogv1.Part_PART_PASSWORD,
	"PANEL_REDIS_PASSWORD":      catalogv1.Part_PART_PASSWORD,
}

// onePanelAdmin are the fields with which an app signs in to another app's server as
// its administrator.
var onePanelAdmin = []string{"PANEL_DB_ROOT_USER", "PANEL_DB_ROOT_PASSWORD"}

// onePanelNeeds reads what an app takes from another app 1Panel runs: an apps field,
// which offers database servers by engine and opens a child field for the host, or a
// service field, whose key names the engine. Each is a need (ADR 0084), and the
// fields 1Panel fills from that server are the need's, which filled says. An app
// whose Compose file reads the engine's word, as Gitea's DB_TYPE does, has it as an
// engine part, with 1Panel's word for each engine the field offers, or the app's own
// where its init.sh turns 1Panel's into one (typeWords).
//
// An app is left out where it uses an app Dockwell has no engine for, as Kafka or
// MinIO, or signs in to another app's server as its administrator, which Dockwell
// gives no app of a server it may share.
func onePanelNeeds(fields []onePanelField, compose []byte, typeWords map[string]string) ([]NeedDraft, map[string]bool, error) {
	filled := map[string]bool{}
	var needs []NeedDraft
	db, redis := -1, -1
	for i := range fields {
		f := &fields[i]
		if f.Disabled || f.Type != "service" && f.Type != "apps" {
			continue
		}
		host, engines, typed := f.EnvKey, []catalogv1.Engine(nil), ""
		words := map[catalogv1.Engine]string{}
		switch {
		case f.Type == "apps" && f.Child != nil && f.Child.EnvKey != "":
			host = f.Child.EnvKey
			for _, v := range f.Values {
				value := fmt.Sprint(v.Value)
				if e, ok := onePanelEngines[value]; ok && !slices.Contains(engines, e) {
					engines = append(engines, e)
					words[e] = cmp.Or(typeWords[value], value)
				}
			}
			if f.EnvKey != "" && bytes.Contains(compose, []byte(f.EnvKey)) {
				// The app reads the engine's word, as an engine part.
				typed = f.EnvKey
			}
			if e, ok := onePanelEngines[fmt.Sprint(f.Default)]; ok && slices.Contains(engines, e) {
				// 1Panel's default first, as a new one beside the app is of it.
				engines = append([]catalogv1.Engine{e}, slices.DeleteFunc(engines, func(x catalogv1.Engine) bool { return x == e })...)
			}
		case f.Type == "service" && f.EnvKey != "":
			if e, ok := onePanelEngines[f.Key]; ok {
				engines = []catalogv1.Engine{e}
			}
		default:
			continue
		}
		what := cmp.Or(f.Key, f.LabelEn, host)
		switch {
		case len(engines) == 0:
			return nil, nil, leave(ClassAnotherApp, "it uses %s from another app, for its %s, which Dockwell has no engine for", what, host)
		case slices.Contains(engines, catalogv1.Engine_ENGINE_REDIS) && len(engines) > 1:
			return nil, nil, leave(ClassAnotherApp, "it takes Redis or a database for its %s, as one field", host)
		}
		filled[host] = true
		if f.EnvKey != "" {
			filled[f.EnvKey] = true
		}
		if engines[0] == catalogv1.Engine_ENGINE_REDIS {
			if redis >= 0 {
				return nil, nil, leave(ClassAnotherApp, "it takes two Redis servers, which 1Panel names alike")
			}
			redis = len(needs)
			needs = append(needs, NeedDraft{
				Name: "redis", Label: "Redis",
				// A Valkey a person runs speaks Redis's protocol too.
				Engines: []catalogv1.Engine{catalogv1.Engine_ENGINE_REDIS, catalogv1.Engine_ENGINE_VALKEY},
				Parts:   map[string]catalogv1.Part{host: catalogv1.Part_PART_HOST},
			})
			continue
		}
		if db >= 0 {
			return nil, nil, leave(ClassAnotherApp, "it takes two database servers, which 1Panel names alike")
		}
		db = len(needs)
		d := NeedDraft{Name: "db", Label: "Database", Engines: engines, Parts: map[string]catalogv1.Part{host: catalogv1.Part_PART_HOST}}
		if typed != "" {
			d.Parts[typed], d.Words = catalogv1.Part_PART_ENGINE, words
		}
		needs = append(needs, d)
	}
	if db >= 0 {
		for i := range fields {
			if slices.Contains(onePanelAdmin, fields[i].EnvKey) && !fields[i].Disabled {
				return nil, nil, leave(ClassAnotherApp, "it signs in to another app's database server as its administrator (%s), which Dockwell gives no app of a server it may share", fields[i].EnvKey)
			}
		}
		for v, p := range onePanelParts {
			needs[db].Parts[v] = p
			filled[v] = true
		}
	}
	if redis >= 0 {
		for v, p := range onePanelRedisParts {
			needs[redis].Parts[v] = p
			filled[v] = true
		}
	}
	return needs, filled, nil
}
