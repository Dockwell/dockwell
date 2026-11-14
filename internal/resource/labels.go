// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// A label's key and value are as Kubernetes has them (ADR 0091): a name of at
// most labelName characters, letters, digits, '-', '_' and '.', starting and
// ending with a letter or digit; a key may have a prefix, a DNS subdomain of at
// most labelPrefix characters, and '/'. A value is empty or a name.
const (
	labelName   = 63
	labelPrefix = 253
	// MaxLabels is how many labels a server may have.
	MaxLabels = 64
)

// CheckLabelKey says what's wrong with key as a label's key, or nil.
func CheckLabelKey(key string) error {
	prefix, name, prefixed := strings.Cut(key, "/")
	if !prefixed {
		prefix, name = "", key
	}
	switch {
	case key == "":
		return errors.New("a label needs a key, such as region")
	case prefixed && prefix == "":
		return fmt.Errorf("the label key %q has / with no prefix before it. Write a prefix such as example.com/, or leave / out", key)
	case prefixed && len(prefix) > labelPrefix:
		return fmt.Errorf("the label key %q has a prefix of %d characters, past the %d a prefix may have", key, len(prefix), labelPrefix)
	case prefixed && !dnsSubdomain(prefix):
		return fmt.Errorf("the label key %q has a prefix that isn't a domain: lowercase letters, digits, - and ., such as example.com", key)
	case prefixed && Reserved(prefix):
		return fmt.Errorf("the label key %q has the prefix %s, which Dockwell keeps for its own labels. Use a prefix of your own, such as example.com/", key, prefix)
	}
	if err := checkLabelName(name); err != nil {
		return fmt.Errorf("the label key %q %w", key, err)
	}
	return nil
}

// CheckLabelValue says what's wrong with value as a label's value, or nil.
func CheckLabelValue(key, value string) error {
	if value == "" {
		return nil
	}
	if err := checkLabelName(value); err != nil {
		return fmt.Errorf("the value of the label %s, %q, %w", key, value, err)
	}
	return nil
}

// CheckLabels says what's wrong with a server's labels, or nil, the first key in
// order first.
func CheckLabels(labels map[string]string) error {
	if len(labels) > MaxLabels {
		return fmt.Errorf("a server can have %d labels, and these are %d", MaxLabels, len(labels))
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		if err := CheckLabelKey(k); err != nil {
			return err
		}
		if err := CheckLabelValue(k, labels[k]); err != nil {
			return err
		}
	}
	return nil
}

// checkLabelName says what's wrong with a name, as a key's after its prefix or a
// value, in words that follow the key or value they're about.
func checkLabelName(name string) error {
	switch {
	case name == "":
		return errors.New("has no name after its prefix")
	case len(name) > labelName:
		return fmt.Errorf("is %d characters, past the %d a label may have", len(name), labelName)
	}
	// What it can't hold at all comes before where it may not stand, as the panel
	// says as it's typed.
	for i := range len(name) {
		if c := name[i]; !alnum(c) && c != '-' && c != '_' && c != '.' {
			if c == ' ' {
				return errors.New("has a space, and a label holds letters, digits, -, _ and . alone")
			}
			// The character as a person typed it, not the first byte of it.
			r, _ := utf8.DecodeRuneInString(name[i:])
			return fmt.Errorf("has %q, and a label holds letters, digits, -, _ and . alone", r)
		}
	}
	if !alnum(name[0]) || !alnum(name[len(name)-1]) {
		return errors.New("must start and end with a letter or digit")
	}
	return nil
}

// reservedPrefix is the prefix Dockwell keeps for its own labels, with each of its
// subdomains.
const reservedPrefix = "dockwell.io"

// Reserved reports a key's prefix that Dockwell keeps for its own labels:
// dockwell.io, or one of its subdomains.
func Reserved(prefix string) bool {
	return prefix == reservedPrefix || strings.HasSuffix(prefix, "."+reservedPrefix)
}

func alnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// dnsSubdomain reports a DNS subdomain: lowercase labels of letters, digits and
// '-', each starting and ending with a letter or digit, between dots.
func dnsSubdomain(s string) bool {
	for _, part := range strings.Split(s, ".") {
		if part == "" || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
			return false
		}
		for i := range len(part) {
			if c := part[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}
