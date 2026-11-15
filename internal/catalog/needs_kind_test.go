// SPDX-License-Identifier: AGPL-3.0-only

package catalog_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
	"github.com/dockwell/dockwell/internal/catalog"
)

// withStorage is WordPress given an optional bucket, as ADR 0099's example writes
// one: S3 storage of any kind that takes presigned URLs, which the app switches on
// with its own word, its parts each a whole value of its environment.
func withStorage(t *testing.T) *catalogv1.Entry {
	t.Helper()
	e := proto.CloneOf(wordpressNeeds(t))
	e.Compose = strings.Replace(e.GetCompose(), "WORDPRESS_DB_HOST: ${DB_ADDRESS}", "WORDPRESS_DB_HOST: ${DB_ADDRESS}\n"+
		"      FILE_STORAGE: ${STORAGE_DRIVER}\n"+
		"      S3_ENDPOINT: ${S3_ENDPOINT}\n"+
		"      S3_REGION: ${S3_REGION}\n"+
		"      S3_BUCKET: ${S3_BUCKET}\n"+
		"      S3_ACCESS_KEY_ID: ${S3_ACCESS_KEY_ID}\n"+
		"      S3_SECRET_ACCESS_KEY: ${S3_SECRET_ACCESS_KEY}\n"+
		"      S3_FORCE_PATH_STYLE: ${S3_FORCE_PATH_STYLE}", 1)
	e.Needs = append(e.Needs, &catalogv1.Need{
		Name: "storage", Label: "Storage", Kind: catalogv1.Kind_KIND_S3, Optional: true, Bucket: "uploads", PresignLifetime: "1h",
		Accepts: []*catalogv1.NeedAccept{{Kind: catalogv1.Kind_KIND_S3, Features: []catalogv1.Feature{catalogv1.Feature_FEATURE_PRESIGNED_URLS}}},
		Sets: []*catalogv1.NeedSet{
			{Name: "STORAGE_DRIVER", Part: catalogv1.Part_PART_MET, Words: []*catalogv1.NeedWord{{Value: "met", Word: "s3"}, {Value: "unmet", Word: "local"}}},
			{Name: "S3_ENDPOINT", Part: catalogv1.Part_PART_ENDPOINT},
			{Name: "S3_REGION", Part: catalogv1.Part_PART_REGION},
			{Name: "S3_BUCKET", Part: catalogv1.Part_PART_BUCKET},
			{Name: "S3_ACCESS_KEY_ID", Part: catalogv1.Part_PART_ACCESS_KEY_ID},
			{Name: "S3_SECRET_ACCESS_KEY", Part: catalogv1.Part_PART_SECRET_KEY},
			{Name: "S3_FORCE_PATH_STYLE", Part: catalogv1.Part_PART_PATH_STYLE, Words: []*catalogv1.NeedWord{{Value: "on", Word: "true"}, {Value: "off", Word: "false"}}},
		},
	})
	return e
}

// An optional need for S3 storage, as ADR 0099 writes one, passes the check, and so
// does one that leaves accepts out for any S3 storage.
func TestAnOptionalStorageNeedPasses(t *testing.T) {
	e := withStorage(t)
	if err := catalog.Check(e); err != nil {
		t.Fatal(err)
	}
	e.Needs[1].Accepts = nil
	if err := catalog.Check(e); err != nil {
		t.Errorf("any S3 storage: %v", err)
	}
	// Its secret key is kept as a secret, as a password is.
	if !catalog.SecretPart(e.Needs[1].Sets[5]) || catalog.SecretPart(e.Needs[1].Sets[4]) {
		t.Error("the secret key isn't secret, or the key's ID is")
	}
}

// What a storage need says that it can't is refused, each with why.
func TestCheckRefusesAStorageNeed(t *testing.T) {
	storage := func(e *catalogv1.Entry) *catalogv1.Need { return e.Needs[1] }
	for want, change := range map[string]func(*catalogv1.Entry){
		"which Dockwell doesn't meet yet": func(e *catalogv1.Entry) { storage(e).Kind = catalogv1.Kind_KIND_SMTP },
		"only a database need takes":      func(e *catalogv1.Entry) { storage(e).Database = "uploads" },
		"3 to 63 lowercase":               func(e *catalogv1.Entry) { storage(e).Bucket = "Up_Loads" },
		"from 1s to 168h":                 func(e *catalogv1.Entry) { storage(e).PresignLifetime = "30d" },
		"give accepts the kind KIND_S3": func(e *catalogv1.Entry) {
			storage(e).Accepts[0].Engine = catalogv1.Engine_ENGINE_MYSQL
		},
		"doesn't have": func(e *catalogv1.Entry) { storage(e).Accepts[0].Features[0] = catalogv1.Feature(99) },
		"twice": func(e *catalogv1.Entry) {
			storage(e).Accepts[0].Features = append(storage(e).Accepts[0].Features, catalogv1.Feature_FEATURE_PRESIGNED_URLS)
		},
		"sets too little to reach a bucket": func(e *catalogv1.Entry) {
			storage(e).Sets = storage(e).Sets[:4]
			e.Compose = strings.Replace(strings.Replace(e.GetCompose(), "      S3_SECRET_ACCESS_KEY: ${S3_SECRET_ACCESS_KEY}\n", "", 1), "      S3_FORCE_PATH_STYLE: ${S3_FORCE_PATH_STYLE}", "", 1)
		},
		"isn't a part of what it needs": func(e *catalogv1.Entry) { storage(e).Sets[2].Part = catalogv1.Part_PART_HOST },
		"which only S3 storage has": func(e *catalogv1.Entry) {
			e.Needs[0].Sets[0].Part = catalogv1.Part_PART_BUCKET
		},
		"has no word for unmet": func(e *catalogv1.Entry) {
			storage(e).Sets[0].Words = storage(e).Sets[0].Words[:1]
		},
		"has no word for off": func(e *catalogv1.Entry) {
			storage(e).Sets[6].Words = storage(e).Sets[6].Words[:1]
		},
		"which it doesn't take": func(e *catalogv1.Entry) { storage(e).Sets[0].Words[0].Value = "yes" },
		"isn't optional, so it's always met": func(e *catalogv1.Entry) {
			storage(e).Optional = false
		},
		"is set while unmet already": func(e *catalogv1.Entry) { storage(e).Sets[0].EmptyWhenUnmet = true },
		"so it's never unmet": func(e *catalogv1.Entry) {
			e.Needs[0].Sets[0].EmptyWhenUnmet = true
		},
		"the secret S3_SECRET_ACCESS_KEY is inside a longer value": func(e *catalogv1.Entry) {
			e.Compose = strings.Replace(e.GetCompose(), "S3_SECRET_ACCESS_KEY: ${S3_SECRET_ACCESS_KEY}", "S3_SECRET_ACCESS_KEY: x-${S3_SECRET_ACCESS_KEY}", 1)
		},
		"which Dockwell can't run as a need's yet": func(e *catalogv1.Entry) { storage(e).Service = "db" },
	} {
		e := withStorage(t)
		change(e)
		err := catalog.Check(e)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	// An unmet word may be empty, which leaves the name out while unmet.
	e := withStorage(t)
	e.Needs[1].Sets[0].Words[1].Word = ""
	if err := catalog.Check(e); err != nil {
		t.Errorf("an empty unmet word: %v", err)
	}
}

// An optional need installs unset unless a person asks otherwise: the app runs on
// its own disk, its storage names left out of its environment but the one that says
// so, which reads the app's word for unmet, and the pin keeps it so, as the file it
// ran (ADR 0099).
func TestAnOptionalNeedInstallsUnset(t *testing.T) {
	e := withStorage(t)
	ways := catalog.Ways(e, lookupBuiltIn(t))
	if got := ways["storage"]; len(got) != 1 || !got[0].Unset || got[0].Choice() != catalog.ChoiceUnset {
		t.Fatalf("the storage need's ways are %+v, want unset alone", got)
	}
	met, err := catalog.Meet(e, ways, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !met["storage"].Unset || met["db"].Unset {
		t.Fatalf("by default, storage is %+v and the database %+v", met["storage"], met["db"])
	}
	needed, err := catalog.Need(e, met)
	if err != nil {
		t.Fatal(err)
	}
	file := needed.Entry.GetCompose()
	for _, name := range []string{"S3_ENDPOINT", "S3_REGION", "S3_BUCKET", "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY", "S3_FORCE_PATH_STYLE"} {
		if strings.Contains(file, name) {
			t.Errorf("unset, the file still names %s:\n%s", name, file)
		}
	}
	if !strings.Contains(file, "FILE_STORAGE: ${STORAGE_DRIVER}") || !strings.Contains(file, "WORDPRESS_DB_HOST: ${DB_ADDRESS}") {
		t.Errorf("unset, the file lost what it keeps:\n%s", file)
	}
	var driver *catalogv1.Input
	for _, in := range needed.Entry.GetInputs() {
		if in.GetName() == "STORAGE_DRIVER" {
			driver = in
		}
	}
	if driver == nil || driver.GetDefaultValue() != "local" {
		t.Errorf("unset, STORAGE_DRIVER is %v, want local", driver)
	}
	pin := needed.Pinned["storage"]
	if !pin.GetUnset() || len(pin.GetLeftOut()) != 6 || len(pin.GetPlain()) != 1 || needed.Links["storage"] != nil {
		t.Errorf("unset, the pin is %v and the link %v", pin, needed.Links["storage"])
	}
	// The file the install ran comes back from its pin, as an upgrade reads it.
	ran, err := catalog.MetFile(e.GetCompose(), needed.Pinned)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ran, "S3_ENDPOINT") || !strings.Contains(ran, "${STORAGE_DRIVER}") {
		t.Errorf("from its pin, the install ran:\n%s", ran)
	}
	// An upgrade keeps it unset, and a release that needs it stays on the one it is.
	if again, err := catalog.Meet(e, ways, nil, needed.Pinned); err != nil || !again["storage"].Unset {
		t.Errorf("upgraded, storage is %+v, %v", again["storage"], err)
	}
	required := withStorage(t)
	required.Needs[1].Optional = false
	required.Needs[1].Sets = required.Needs[1].Sets[1:]
	if _, err := catalog.Meet(required, catalog.Ways(required, lookupBuiltIn(t)), nil, needed.Pinned); err == nil || !strings.Contains(err.Error(), "which the install runs without") {
		t.Errorf("a release that needs its storage, upgraded from one without: %v", err)
	}
	// A need the app can't run without can't be met yet, and says why.
	if _, err := catalog.Meet(required, catalog.Ways(required, lookupBuiltIn(t)), nil, nil); err == nil || !strings.Contains(err.Error(), "S3 storage, which Dockwell can't make beside it yet") {
		t.Errorf("a need for storage the app can't run without: %v", err)
	}
}

// Set empty while unmet, a name stays in the file, and an unmet word that's empty
// leaves the switch out too.
func TestUnsetKeepsWhatItsToldTo(t *testing.T) {
	e := withStorage(t)
	e.Needs[1].Sets[0].Words[1].Word = ""
	e.Needs[1].Sets[2].EmptyWhenUnmet = true
	needed, err := catalog.Need(e, mustMeet(t, e, lookupBuiltIn(t)))
	if err != nil {
		t.Fatal(err)
	}
	file := needed.Entry.GetCompose()
	if strings.Contains(file, "STORAGE_DRIVER") || !strings.Contains(file, "S3_REGION: ${S3_REGION}") {
		t.Errorf("unset, the file is:\n%s", file)
	}
}
