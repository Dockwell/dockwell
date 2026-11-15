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
		"an image Dockwell doesn't know as S3 storage": func(e *catalogv1.Entry) { storage(e).Service = "db" },
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
