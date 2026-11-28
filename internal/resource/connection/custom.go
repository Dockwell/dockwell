// SPDX-License-Identifier: AGPL-3.0-only

package connection

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/probe"
	"github.com/dockwell/dockwell/internal/resource"
)

// MaxCustomFields bounds a Custom connection and its app mappings.
const MaxCustomFields = 32

const maxCustomValue = 64 << 10

var customFieldName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)
var (
	envWords   = regexp.MustCompile(`[^A-Za-z0-9]+`)
	envAcronym = regexp.MustCompile(`([A-Z]+)([A-Z][a-z])`)
	envCamel   = regexp.MustCompile(`([a-z0-9])([A-Z])`)
)

// ValidCustomFieldName defines the names links can read from a Custom connection.
func ValidCustomFieldName(name string) bool { return customFieldName.MatchString(name) }

// DefaultEnv names a field under a prefix derived from the connection name.
func DefaultEnv(name, field string) string {
	words := envAcronym.ReplaceAllString(name+"_"+field, "${1}_${2}")
	words = envCamel.ReplaceAllString(words, "${1}_${2}")
	env := strings.ToUpper(strings.Trim(envWords.ReplaceAllString(words, "_"), "_"))
	if env != "" && env[0] >= '0' && env[0] <= '9' {
		return "_" + env
	}
	return env
}

// CustomField returns the saved field by name.
func CustomField(c *resourcev1.ConnectionSpec, name string) *resourcev1.ConnectionCustomField {
	i := slices.IndexFunc(c.GetCustom().GetFields(), func(f *resourcev1.ConnectionCustomField) bool { return f.GetName() == name })
	if i < 0 {
		return nil
	}
	return c.GetCustom().GetFields()[i]
}

// CustomSecret reports whether the field must be bound as a secret.
func CustomSecret(f *resourcev1.ConnectionCustomField) bool {
	return f.GetType() == resourcev1.ConnectionCustomType_CONNECTION_CUSTOM_TYPE_SECRET
}

func validateCustom(c *resourcev1.ConnectionCustom) error {
	if c == nil || len(c.GetFields()) == 0 || len(c.GetFields()) > MaxCustomFields {
		return fmt.Errorf("a Custom connection needs 1 to %d fields", MaxCustomFields)
	}
	c.Initial = strings.TrimSpace(c.GetInitial())
	if len([]rune(c.Initial)) > 3 || strings.ContainsFunc(c.Initial, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) {
		return errors.New("use up to three letters or digits for the connection's initials")
	}
	seen := map[string]bool{}
	for _, f := range c.GetFields() {
		name := f.GetName()
		if !ValidCustomFieldName(name) {
			return errors.New("use up to 64 letters, digits, underscores or dashes for a field name, starting with a letter")
		}
		alias := DefaultEnv("field", name)
		if seen[alias] {
			return fmt.Errorf("the field %s has the same default environment name as another field. Rename one", name)
		}
		seen[alias] = true
		if err := validateCustomValue(f); err != nil {
			return err
		}
	}
	if c.GetCheck() != nil {
		if err := CustomProbe(c.GetCheck()).Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateCustomValue(f *resourcev1.ConnectionCustomField) error {
	value := f.GetValue()
	if CustomSecret(f) {
		if value != "" {
			return fmt.Errorf("give %s in its secret value field", f.GetName())
		}
		value = f.GetSecretValue()
	} else if f.SecretValue != nil {
		return fmt.Errorf("only a secret field takes a secret value. Check %s", f.GetName())
	}
	if len(value) > maxCustomValue || strings.ContainsRune(value, 0) {
		return fmt.Errorf("%s needs a value up to %d KiB with no zero byte", f.GetName(), maxCustomValue>>10)
	}
	switch f.GetType() {
	case resourcev1.ConnectionCustomType_CONNECTION_CUSTOM_TYPE_TEXT, resourcev1.ConnectionCustomType_CONNECTION_CUSTOM_TYPE_SECRET:
		return nil
	case resourcev1.ConnectionCustomType_CONNECTION_CUSTOM_TYPE_URL:
		u, err := url.Parse(value)
		if value == "" || err == nil && u.IsAbs() && u.Hostname() != "" && u.User == nil {
			return nil
		}
		return fmt.Errorf("%s needs an absolute URL with a host. Use a secret field for a URL with credentials", f.GetName())
	case resourcev1.ConnectionCustomType_CONNECTION_CUSTOM_TYPE_PORT:
		port, err := strconv.ParseUint(value, 10, 16)
		if value == "" || err == nil && port > 0 {
			return nil
		}
		return fmt.Errorf("%s needs a port between 1 and 65535", f.GetName())
	case resourcev1.ConnectionCustomType_CONNECTION_CUSTOM_TYPE_NUMBER:
		number, err := strconv.ParseFloat(value, 64)
		if value == "" || err == nil && !math.IsNaN(number) && !math.IsInf(number, 0) {
			return nil
		}
		return fmt.Errorf("%s needs a finite number", f.GetName())
	default:
		return fmt.Errorf("choose a type for %s", f.GetName())
	}
}

// CustomProbe converts a check target into the agent's credential-free request.
func CustomProbe(c *resourcev1.ConnectionCustomCheck) probe.Custom {
	return probe.Custom{HTTPS: c.GetHttpsUrl(), Host: c.GetTcp().GetHost(), Port: c.GetTcp().GetPort()}
}

func keepCustom(was, is *resourcev1.ConnectionSpec) resource.Kept {
	restates := false
	kept := false
	for _, f := range is.GetCustom().GetFields() {
		if !CustomSecret(f) {
			continue
		}
		if f.SecretValue != nil {
			restates = true
			// An empty value clears the secret. Keep the spec's unset form stable.
			if f.GetSecretValue() == "" {
				f.SecretValue = nil
			}
			continue
		}
		if prev := CustomField(was, f.GetName()); CustomSecret(prev) && prev.SecretValue != nil {
			f.SecretValue = proto.String(prev.GetSecretValue())
			kept = true
		}
	}
	if kept && !restates {
		return resource.Kept{Reason: "kept_key", Words: "keeps the saved secrets where the update left them out"}
	}
	return resource.Kept{}
}

func sameCustomFields(was, is *resourcev1.ConnectionSpec) bool {
	before, after := was.GetCustom().GetFields(), is.GetCustom().GetFields()
	if len(before) != len(after) {
		return false
	}
	for _, field := range before {
		if !proto.Equal(field, CustomField(is, field.GetName())) {
			return false
		}
	}
	return true
}
