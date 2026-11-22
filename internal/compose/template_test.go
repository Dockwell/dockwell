// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"reflect"
	"strings"
	"testing"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

func TestTemplateRefusesPublicCredentialFields(t *testing.T) {
	for _, field := range []string{
		"labels: {db.password: case-fixture}",
		"command: [db-client, --password, case-fixture]",
		"entrypoint: [DB_PASSWORD=case-fixture, db-client]",
	} {
		draft := &spec.ComposeTemplate{ComposeYaml: "services:\n  web:\n    image: nginx:1.27\n    " + field}
		err := ValidateTemplate(t.Context(), draft, nil)
		if err == nil || strings.Contains(err.Error(), "case-fixture") {
			t.Fatalf("credential in a readable field should be refused safely: %v", err)
		}
	}
}

func TestTemplateGuardsEverySecretSink(t *testing.T) {
	const prefix = "services:\n  web:\n    image: nginx:1.27\n"
	for _, source := range []string{
		"    environment: {API_TOKEN: private-value-must-not-escape}\n",
		"    environment: {API_TOKEN: ${API_TOKEN:-private-value-must-not-escape}}\n",
		"    labels: {environment: '${API_TOKEN}'}\n",
		"    environment: {API_TOKEN: [private-value-must-not-escape]}\n",
		"    command: [!!binary YWJj]\n",
	} {
		draft := &spec.ComposeTemplate{ComposeYaml: prefix + source}
		err := ValidateTemplate(t.Context(), draft, nil)
		if err == nil || strings.Contains(err.Error(), "private-value-must-not-escape") {
			t.Fatalf("invalid source yielded %v", err)
		}
	}
}

func TestTemplateNormalizesPublicDefaultsAndSecretNames(t *testing.T) {
	draft := &spec.ComposeTemplate{ComposeYaml: `name: old-workspace
services:
  web:
    image: nginx:${TAG:-1.27}
    ports: ["${WEB_PORT:-8080}:80"]
    environment:
      API_TOKEN:
`}
	if err := ValidateTemplate(t.Context(), draft, nil); err != nil {
		t.Fatal(err)
	}
	want := []spec.ComposeTemplateInputKind{templateSecret, templateImage, templatePort}
	var kinds []spec.ComposeTemplateInputKind
	for _, in := range draft.GetInputs() {
		kinds = append(kinds, in.GetKind())
	}
	if !reflect.DeepEqual(kinds, want) || strings.Contains(draft.GetComposeYaml(), "old-workspace") {
		t.Fatalf("unexpected normalized template: %v", draft)
	}
	if err := ValidateTemplate(t.Context(), draft, nil); err != nil {
		t.Fatal(err)
	}
	if draft.GetInputs()[0].DefaultValue != nil || !draft.GetInputs()[0].GetRequired() ||
		draft.GetInputs()[1].GetDefaultValue() != "1.27" || draft.GetInputs()[2].GetDefaultValue() != "8080" {
		t.Fatalf("incorrect defaults or secret input: %v", draft.GetInputs())
	}
}
