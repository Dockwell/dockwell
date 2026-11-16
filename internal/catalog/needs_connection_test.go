// SPDX-License-Identifier: AGPL-3.0-only

package catalog_test

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// requiredStorage is withStorage's app with storage it can't run without, which
// has no word for whether it's met.
func requiredStorage(t *testing.T) *catalogv1.Entry {
	t.Helper()
	e := withStorage(t)
	e.Needs[1].Optional = false
	e.Needs[1].Sets = e.Needs[1].Sets[1:]
	return e
}

// A connection the project may use is a way to meet a need for S3 storage, after the
// others: an optional need still installs unset unless a person picks it, and one the
// app can't run without takes the only one there is, or the one named for the app's
// environment, or asks which (ADR 0099).
func TestAConnectionMeetsAStorageNeed(t *testing.T) {
	uploads := catalog.ConnectionWay{Name: "uploads", Label: "Uploads", Prefix: "site/"}
	media := catalog.ConnectionWay{Name: "media", Label: "Media", Prefix: "site/"}

	e := withStorage(t)
	ways := catalog.WithConnections(e, catalog.Ways(e, lookupBuiltIn(t)), []catalog.ConnectionWay{uploads})
	var choices []string
	for _, w := range ways["storage"] {
		choices = append(choices, w.Choice())
	}
	if !slices.Equal(choices, []string{catalog.ChoiceUnset, "connection:uploads"}) {
		t.Fatalf("storage's ways are %v", choices)
	}
	if len(ways["db"]) != len(catalog.Ways(e, lookupBuiltIn(t))["db"]) {
		t.Error("a connection is offered for the database need")
	}
	if met, err := catalog.Meet(e, ways, nil, nil); err != nil || !met["storage"].Unset {
		t.Errorf("an optional need with a connection there is met by %+v, %v; want unset", met["storage"], err)
	}
	if met, err := catalog.Meet(e, ways, map[string]string{"storage": "connection:uploads"}, nil); err != nil || met["storage"].Connection == nil || met["storage"].Connection.Name != "uploads" {
		t.Errorf("asked for uploads, storage is met by %+v, %v", met["storage"], err)
	}
	if _, err := catalog.Meet(e, ways, map[string]string{"storage": "connection:media"}, nil); err == nil {
		t.Error("a connection the project may not use is taken")
	}

	required := requiredStorage(t)
	meet := func(conns ...catalog.ConnectionWay) (map[string]catalog.Way, error) {
		return catalog.Meet(required, catalog.WithConnections(required, catalog.Ways(required, lookupBuiltIn(t)), conns), nil, nil)
	}
	if met, err := meet(uploads); err != nil || met["storage"].Connection.Name != "uploads" {
		t.Errorf("the only connection: %+v, %v", met["storage"], err)
	}
	if _, err := meet(uploads, media); err == nil || !strings.Contains(err.Error(), "more than one connection may meet it, so nothing was planned. Pick one: connection:uploads or connection:media") {
		t.Errorf("two connections: %v", err)
	}
	// Asked for a new one, it names the connections that meet it (the slice 1 review, L2).
	if _, err := catalog.Meet(required, catalog.WithConnections(required, catalog.Ways(required, lookupBuiltIn(t)), []catalog.ConnectionWay{uploads}), map[string]string{"storage": "new"}, nil); err == nil ||
		!strings.Contains(err.Error(), "which Dockwell can't make beside it yet, so nothing was planned. Pick a connection that meets it: connection:uploads") {
		t.Errorf("asked for a new bucket with a connection there: %v", err)
	}
	named := catalog.ConnectionWay{Name: "media-production", Prefix: "site/", Named: true}
	if met, err := meet(uploads, named); err != nil || met["storage"].Connection.Name != "media-production" {
		t.Errorf("two connections, one named for the environment: %+v, %v", met["storage"], err)
	}
	if _, err := meet(); err == nil || !strings.Contains(err.Error(), "so nothing was planned. Add a connection on Connections that this project may use") {
		t.Errorf("no connection: %v", err)
	}

	// An app that reads its storage as a URL can't take one yet, and says why.
	url := proto.CloneOf(required)
	url.Needs[1].Sets = append(url.Needs[1].Sets, &catalogv1.NeedSet{Name: "S3_URL", Part: catalogv1.Part_PART_URL})
	url.Compose = strings.Replace(url.GetCompose(), "S3_BUCKET: ${S3_BUCKET}", "S3_BUCKET: ${S3_BUCKET}\n      S3_URL: ${S3_URL}", 1)
	urlWays := catalog.WithConnections(url, catalog.Ways(url, lookupBuiltIn(t)), []catalog.ConnectionWay{uploads})
	if w := urlWays["storage"]; len(w) != 1 || !strings.Contains(w[0].Why, "as a URL") {
		t.Errorf("an app that reads a URL is offered %+v", w)
	}
}

// Met by a connection, a need links to it by name: the link sets each name that
// reads a value of the connection's, a key as a secret, each an input of no meaning
// the install drops for the value the link reads, while the app's word for met, its
// path style and the prefix are plain. The pin keeps the connection and the prefix,
// so an upgrade keeps both, though the project is named otherwise by then.
func TestANeedMetByAConnectionLinksToIt(t *testing.T) {
	e := withStorage(t)
	uploads := catalog.ConnectionWay{Name: "uploads", Label: "Uploads", Prefix: "site/", PathStyle: true}
	ways := catalog.WithConnections(e, catalog.Ways(e, lookupBuiltIn(t)), []catalog.ConnectionWay{uploads})
	met, err := catalog.Meet(e, ways, map[string]string{"storage": "connection:uploads"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	needed, err := catalog.Need(e, met)
	if err != nil {
		t.Fatal(err)
	}

	l := needed.Links["storage"]
	if l.GetConnection() != "uploads" || l.GetKind() != resourcev1.LinkKind_LINK_KIND_S3 || l.GetPrefix() != "site/" || l.GetLabel() != "Storage" || l.GetService() != "" ||
		!slices.Equal(l.GetFeatures(), []resourcev1.LinkFeature{resourcev1.LinkFeature_LINK_FEATURE_PRESIGNED_URLS}) {
		t.Errorf("the link is %v", l)
	}
	sets := map[string]resourcev1.LinkPart{}
	for _, s := range l.GetSets() {
		sets[s.GetName()] = s.GetPart()
	}
	want := map[string]resourcev1.LinkPart{
		"S3_ENDPOINT": resourcev1.LinkPart_LINK_PART_ENDPOINT, "S3_REGION": resourcev1.LinkPart_LINK_PART_REGION,
		"S3_BUCKET": resourcev1.LinkPart_LINK_PART_BUCKET, "S3_ACCESS_KEY_ID": resourcev1.LinkPart_LINK_PART_ACCESS_KEY_ID,
		"S3_SECRET_ACCESS_KEY": resourcev1.LinkPart_LINK_PART_SECRET_KEY,
	}
	if len(sets) != len(want) {
		t.Errorf("the link sets %v, want %v", sets, want)
	}
	for name, part := range want {
		r, ok := needed.Read[name]
		secret := name == "S3_ACCESS_KEY_ID" || name == "S3_SECRET_ACCESS_KEY"
		if sets[name] != part || !ok || r.Connection != "uploads" || r.Link != "storage" || r.Part != part || r.Secret != secret {
			t.Errorf("%s: the link sets it as %v and it's read as %+v", name, sets[name], r)
		}
	}
	inputs := map[string]*catalogv1.Input{}
	for _, in := range needed.Entry.GetInputs() {
		inputs[in.GetName()] = in
	}
	for name, value := range map[string]string{"STORAGE_DRIVER": "s3", "S3_FORCE_PATH_STYLE": "true", "S3_ENDPOINT": ""} {
		if in := inputs[name]; in == nil || in.GetType() != catalogv1.InputType_INPUT_TYPE_TEXT || in.GetDefaultValue() != value {
			t.Errorf("%s is the input %v, want %q", name, in, value)
		}
	}
	for _, name := range []string{"S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY"} {
		if in := inputs[name]; in == nil || in.GetType() != catalogv1.InputType_INPUT_TYPE_SECRET {
			t.Errorf("the key %s is the input %v, want a secret", name, in)
		}
		if slices.Contains(needed.Plain, name) {
			t.Errorf("the key %s is plain", name)
		}
	}
	if !strings.Contains(needed.Entry.GetCompose(), "S3_SECRET_ACCESS_KEY: ${S3_SECRET_ACCESS_KEY}") {
		t.Errorf("the file lost its storage names:\n%s", needed.Entry.GetCompose())
	}
	pin := needed.Pinned["storage"]
	if pin.GetConnection() != "uploads" || pin.GetPrefix() != "site/" || pin.GetUnset() || pin.GetEngine() != 0 {
		t.Fatalf("the pin is %v", pin)
	}

	// An upgrade keeps the connection and the prefix the install chose.
	moved := catalog.WithConnections(e, catalog.Ways(e, lookupBuiltIn(t)), []catalog.ConnectionWay{{Name: "uploads", Prefix: "renamed/"}})
	again, err := catalog.Meet(e, moved, nil, needed.Pinned)
	if err != nil {
		t.Fatal(err)
	}
	if c := again["storage"].Connection; c == nil || c.Name != "uploads" || c.Prefix != "site/" {
		t.Errorf("upgraded, storage is met by %+v", again["storage"])
	}
	if next, err := catalog.Need(e, again); err != nil || next.Links["storage"].GetPrefix() != "site/" {
		t.Errorf("upgraded, the link is %v, %v", next.Links["storage"], err)
	}
	// One the project can't use any more holds the install on its release.
	gone := catalog.WithConnections(e, catalog.Ways(e, lookupBuiltIn(t)), nil)
	if _, err := catalog.Meet(e, gone, nil, needed.Pinned); err == nil || !strings.Contains(err.Error(), "the connection uploads, which this project can't use any more") {
		t.Errorf("upgraded with the connection out of reach: %v", err)
	}
}

// A connection another app reads already serves it alone (ADR 0099): it's offered
// with why it can't meet a need, never taken by default, and an app that can't keep
// to a prefix is told so. An install it meets already keeps it through an upgrade
// (the slice 1 review, H3).
func TestAConnectionThatServesAnotherAppIsNoWay(t *testing.T) {
	busy := catalog.ConnectionWay{Name: "uploads", Prefix: "site/", Serves: true}
	required := requiredStorage(t)
	ways := catalog.WithConnections(required, catalog.Ways(required, lookupBuiltIn(t)), []catalog.ConnectionWay{busy})
	if w := ways["storage"]; len(w) != 1 || !strings.Contains(w[0].Why, "it serves another app already, and this app can't keep to a prefix of the bucket") {
		t.Fatalf("a connection another app reads is offered as %+v", w)
	}
	if _, err := catalog.Meet(required, ways, nil, nil); err == nil {
		t.Error("a connection another app reads is taken by default")
	}
	if _, err := catalog.Meet(required, ways, map[string]string{"storage": "connection:uploads"}, nil); err == nil || !strings.Contains(err.Error(), "it serves another app already") {
		t.Errorf("asked for a connection another app reads: %v", err)
	}
	// With a key prefix to keep to, it's still one app's.
	prefixed := proto.CloneOf(required)
	prefixed.Needs[1].Sets = append(prefixed.Needs[1].Sets, &catalogv1.NeedSet{Name: "S3_PREFIX", Part: catalogv1.Part_PART_KEY_PREFIX})
	prefixed.Compose = strings.Replace(prefixed.GetCompose(), "S3_BUCKET: ${S3_BUCKET}", "S3_BUCKET: ${S3_BUCKET}\n      S3_PREFIX: ${S3_PREFIX}", 1)
	pw := catalog.WithConnections(prefixed, catalog.Ways(prefixed, lookupBuiltIn(t)), []catalog.ConnectionWay{busy})
	if w := pw["storage"]; len(w) != 1 || w[0].Why != "it serves another app already, and a connection serves one app. Add a connection of its own on Connections" {
		t.Errorf("with a key prefix, the connection is offered as %+v", w)
	}

	// An install the connection meets already keeps it, though another app reads it now.
	free := catalog.WithConnections(required, catalog.Ways(required, lookupBuiltIn(t)), []catalog.ConnectionWay{{Name: "uploads", Prefix: "site/"}})
	met, err := catalog.Meet(required, free, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	needed, err := catalog.Need(required, met)
	if err != nil {
		t.Fatal(err)
	}
	again, err := catalog.Meet(required, ways, nil, needed.Pinned)
	if err != nil || again["storage"].Connection.Name != "uploads" || again["storage"].Why != "" {
		t.Errorf("upgraded beside another app on the connection: %+v, %v", again["storage"], err)
	}
}

// A need a Disconnect left on the app's own values keeps them: an upgrade links it
// to nothing, writes none of the names a connection gave it, and keeps out of the
// file the keys a Disconnect below an admin took away (the slice 1 review, M1).
func TestANeedOnItsOwnValuesKeepsThem(t *testing.T) {
	e := withStorage(t)
	pinned := map[string]*resourcev1.PinnedNeed{"storage": {Own: true, Prefix: "site/", LeftOut: []string{"S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY"}}}
	ways := catalog.WithConnections(e, catalog.Ways(e, lookupBuiltIn(t)), []catalog.ConnectionWay{{Name: "uploads", Prefix: "site/"}})
	met, err := catalog.Meet(e, ways, nil, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if !met["storage"].Own || met["storage"].Choice() != catalog.ChoiceOwn {
		t.Fatalf("storage is met by %+v", met["storage"])
	}
	needed, err := catalog.Need(e, met)
	if err != nil {
		t.Fatal(err)
	}
	if needed.Links["storage"] != nil || len(needed.Read) != 0 {
		t.Errorf("on its own values, the need links %v and reads %v", needed.Links["storage"], needed.Read)
	}
	if !maps.Equal(needed.Kept, map[string]bool{"S3_ENDPOINT": false, "S3_REGION": false, "S3_BUCKET": false}) {
		t.Errorf("it keeps %v", needed.Kept)
	}
	file := needed.Entry.GetCompose()
	if strings.Contains(file, "S3_ACCESS_KEY_ID") || strings.Contains(file, "S3_SECRET_ACCESS_KEY") || !strings.Contains(file, "S3_BUCKET: ${S3_BUCKET}") {
		t.Errorf("on its own values, the file is:\n%s", file)
	}
	if p := needed.Pinned["storage"]; !p.GetOwn() || p.GetPrefix() != "site/" || len(p.GetLeftOut()) != 2 || p.GetConnection() != "" {
		t.Errorf("the pin is %v", p)
	}
	if ran, err := catalog.MetFile(e.GetCompose(), needed.Pinned); err != nil || strings.Contains(ran, "S3_SECRET_ACCESS_KEY") {
		t.Errorf("from its pin, the install ran:\n%s, %v", ran, err)
	}
}
