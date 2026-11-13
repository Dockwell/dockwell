// SPDX-License-Identifier: AGPL-3.0-only

package variable_test

import (
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/variable"
)

func urlTemplate() *resourcev1.ValueTemplate {
	return &resourcev1.ValueTemplate{
		Text: "postgres://${DB_USER}:${DB_PASSWORD}@${DB_ADDRESS}/${DB_NAME}?sslmode=$${off}",
		Names: []*resourcev1.TemplateName{
			{Name: "DB_USER", Encoding: resourcev1.Encoding_ENCODING_USERINFO},
			{Name: "DB_PASSWORD", Secret: true, Encoding: resourcev1.Encoding_ENCODING_USERINFO},
			{Name: "DB_ADDRESS"},
			{Name: "DB_NAME", Encoding: resourcev1.Encoding_ENCODING_PATH},
		},
	}
}

func fromTemplate(t *resourcev1.ValueTemplate) *resourcev1.ValueFrom {
	return &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Template{Template: t}}
}

// A URL's user and password are written so they read as themselves: Azure's
// app@server, and a password with @, /, # and :.
func TestWriteEncodesEachPartForItsPlace(t *testing.T) {
	got, err := variable.Write(urlTemplate(), map[string]string{
		"DB_USER": "app@server", "DB_PASSWORD": "p@ss/w#rd:1", "DB_ADDRESS": "db:5432", "DB_NAME": "my db",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "postgres://app%40server:p%40ss%2Fw%23rd%3A1@db:5432/my%20db?sslmode=${off}"
	if got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	if _, err := variable.Write(urlTemplate(), map[string]string{"DB_USER": "a"}); err == nil || !strings.Contains(err.Error(), "no value of") {
		t.Errorf("a missing value: %v", err)
	}
}

func TestASecretFromATemplate(t *testing.T) {
	s := &resourcev1.Secret{From: fromTemplate(urlTemplate())}
	if err := (variable.SecretType{}).Validate(s); err != nil {
		t.Fatal(err)
	}
	uses := (variable.SecretType{}).Uses(s)
	if len(uses) != 4 || uses[1].Kind != variable.SecretKind || uses[0].Kind != variable.VariableKind || uses[0].Scope != "" {
		t.Fatalf("uses %+v", uses)
	}
	(variable.SecretType{}).Pin(s, map[string]int{"DB_USER": 2, "DB_PASSWORD": 5})
	if s.GetRevisions()["DB_PASSWORD"] != 5 || (variable.SecretType{}).Uses(s)[1].Revision != 5 {
		t.Errorf("pinned %v", s.GetRevisions())
	}
	if variable.FromOf(s) == nil || variable.FromOf(&resourcev1.Secret{Value: "x"}) != nil {
		t.Error("FromOf")
	}
}

func TestFromRefuses(t *testing.T) {
	cases := map[string]struct {
		secret bool
		from   *resourcev1.ValueFrom
		value  string
	}{
		"not both":         {true, fromTemplate(urlTemplate()), "x"},
		"is empty":         {true, fromTemplate(&resourcev1.ValueTemplate{}), ""},
		"without listing":  {true, fromTemplate(&resourcev1.ValueTemplate{Text: "${A}${B}", Names: []*resourcev1.TemplateName{{Name: "A"}}}), ""},
		"doesn't use":      {true, fromTemplate(&resourcev1.ValueTemplate{Text: "${A}", Names: []*resourcev1.TemplateName{{Name: "A"}, {Name: "B"}}}), ""},
		"twice":            {true, fromTemplate(&resourcev1.ValueTemplate{Text: "${A}", Names: []*resourcev1.TemplateName{{Name: "A"}, {Name: "A"}}}), ""},
		"$$":               {true, fromTemplate(&resourcev1.ValueTemplate{Text: "$A", Names: nil}), ""},
		"Make it a secret": {false, fromTemplate(urlTemplate()), ""},
		"names none":       {true, &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Value{Value: &resourcev1.ValueRef{Environment: "env_1", Name: "X", Secret: true}}}, ""},
		"by its ID":        {true, &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Value{Value: &resourcev1.ValueRef{Environment: "shared", Name: "X", Secret: true, Link: "db"}}}, ""},
	}
	for want, c := range cases {
		var err error
		if c.secret {
			err = (variable.SecretType{}).Validate(&resourcev1.Secret{From: c.from, Value: c.value})
		} else {
			err = (variable.Type{}).Validate(&resourcev1.Variable{From: c.from, Value: c.value})
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
}

// A link's value from another environment is used there, by the scope it names.
func TestAValueFromAnotherEnvironment(t *testing.T) {
	s := &resourcev1.Secret{From: &resourcev1.ValueFrom{Source: &resourcev1.ValueFrom_Value{Value: &resourcev1.ValueRef{
		Environment: "env_shared", Name: "BLOG_DB_PASSWORD", Secret: true, Link: "db",
	}}}}
	if err := (variable.SecretType{}).Validate(s); err != nil {
		t.Fatal(err)
	}
	uses := resource.Uses(variable.SecretType{}, s)
	if len(uses) != 1 || uses[0].Scope != "env_shared" || uses[0].Name != "BLOG_DB_PASSWORD" || uses[0].As != variable.FromUse {
		t.Fatalf("uses %+v", uses)
	}
}
