// SPDX-License-Identifier: AGPL-3.0-only

package compose_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/dockwell/dockwell/internal/compose"
)

// parse reads a file, and fails the test if that panics rather than
// returning an error.
func parse(t *testing.T, file string) (err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Parse(%q) panicked: %v", file, r)
		}
	}()
	_, err = compose.Parse(t.Context(), []byte(file), map[string]string{"TAG": "1"})
	return err
}

// A value of the wrong type anywhere Dockwell reads is refused, saying
// where and what Compose takes there, and never panics.
func TestParseRefusesWrongTypes(t *testing.T) {
	for file, want := range map[string]string{
		"services: [web]":                                                       "the Compose file's services is a list, where Compose takes a mapping or nothing",
		"services: {web: [a]}":                                                  "the service web is a list, where Compose takes a mapping",
		"services: {web: {image: [a]}}":                                         "the service web's image is a list, where Compose takes a string",
		"services: {web: {image: a, command: [1]}}":                             "the service web's command[0] is a number, where Compose takes a string",
		"services: {web: {image: a, command: {a: b}}}":                          "the service web's command is a mapping, where Compose takes a string, a list or nothing",
		"services: {web: {image: a, entrypoint: [true]}}":                       "the service web's entrypoint[0] is true, where Compose takes a string. Quote it",
		"services: {web: {image: a, networks: [1]}}":                            "the service web's networks[0] is a number, where Compose takes a string",
		"services: {web: {image: a, networks: 1}}":                              "the service web's networks is a number, where Compose takes a list or a mapping",
		"services: {web: {image: a, depends_on: [1]}}":                          "the service web's depends_on[0] is a number, where Compose takes a string",
		"services: {web: {image: a, depends_on: {db: 1}}}":                      "the service web's depends_on.db is a number, where Compose takes a mapping",
		"services: {web: {image: a, ports: '80:80'}}":                           "the service web's ports is a string, where Compose takes a list",
		"services: {web: {image: a, ports: [[80]]}}":                            "the service web's ports[0] is a list, where Compose takes a string, a number or a mapping",
		"services: {web: {image: a, ports: [{target: [1]}]}}":                   "the service web's ports[0].target is a list, where Compose takes a string or a number",
		"services: {web: {image: a, volumes: '/a:/b'}}":                         "the service web's volumes is a string, where Compose takes a list",
		"services: {web: {image: a, volumes: [1]}}":                             "the service web's volumes[0] is a number, where Compose takes a string or a mapping",
		"services: {web: {image: a, volumes: [{target: 1}]}}":                   "the service web's volumes[0].target is a number, where Compose takes a string",
		"services: {web: {image: a, environment: [1]}}":                         "the service web's environment[0] is a number, where Compose takes a string",
		"services: {web: {image: a, environment: {A: [1]}}}":                    "the service web's environment.A is a list, where Compose takes a string, a number, true, false or nothing",
		"services: {web: {image: a, environment: 1}}":                           "the service web's environment is a number, where Compose takes a list or a mapping",
		"services: {web: {image: a, labels: {a: {b: c}}}}":                      "the service web's labels.a is a mapping",
		"services: {web: {image: a, restart: 1}}":                               "the service web's restart is a number, where Compose takes a string",
		"services: {web: {image: a, user: 1000}}":                               "the service web's user is a number, where Compose takes a string. Quote it",
		"services: {web: {image: a, working_dir: 1}}":                           "the service web's working_dir is a number",
		"services: {web: {image: a, network_mode: 1}}":                          "the service web's network_mode is a number",
		"services: {web: {image: a, expose: [[1]]}}":                            "the service web's expose[0] is a list, where Compose takes a string or a number",
		"services: {web: {image: a, healthcheck: [a]}}":                         "the service web's healthcheck is a list, where Compose takes a mapping",
		"services: {web: {image: a, healthcheck: {test: [1]}}}":                 "the service web's healthcheck.test[0] is a number, where Compose takes a string",
		"services: {web: {image: a, healthcheck: {interval: 5}}}":               "the service web's healthcheck.interval is a number, where Compose takes a string",
		"services: {web: {image: a, healthcheck: {retries: [1]}}}":              "the service web's healthcheck.retries is a list",
		"services: {web: {image: a, healthcheck: {disable: [1]}}}":              "the service web's healthcheck.disable is a list",
		"services: {web: {image: a}}\nvolumes: [data]":                          "the Compose file's volumes is a list, where Compose takes a mapping or nothing",
		"services: {web: {image: a}}\nvolumes: {data: 1}":                       "the volume data is a number, where Compose takes a mapping or nothing",
		"services: {web: {image: a}}\nvolumes: {data: {driver_opts: {a: [1]}}}": "the volume data's driver_opts.a is a list, where Compose takes a string or a number",
		"services: {web: {image: a}}\nnetworks: {n: {internal: [1]}}":           "the network n's internal is a list, where Compose takes a string, true or false",
		"services: {web: {image: a}}\nnetworks: {n: {labels: 1}}":               "the network n's labels is a number",
		"name: [a]\nservices: {web: {image: a}}":                                "the Compose file's name is a list, where Compose takes a string",
		"services: {web: {image: 2026-11-04}}":                                  `the service web's image is a date, where Compose takes a string. Quote it, as "2026-11-04"`,
		"services: {web: ~}":                                                    "the service web is empty, where Compose takes a mapping. Write it as Compose does, such as {image: nginx:1.27}",
		"services: {web: {image: a, command: [2026-11-04]}}":                    "the service web's command[0] is a date",
		"services: {1: {image: a}}":                                             `the Compose file's services names something 1, which isn't a string. Quote it, as "1"`,
	} {
		t.Run(file, func(t *testing.T) {
			err := parse(t, file)
			if !errors.Is(err, compose.ErrInvalid) || !strings.Contains(compose.Message(err), want) {
				t.Errorf("Parse = %v, want %q", err, want)
			}
		})
	}
}

// Whatever a file holds, reading it returns specs or an error, and never
// panics.
func FuzzParse(f *testing.F) {
	f.Add(shop)
	for _, s := range []string{
		"services: {web: {image: a, networks: [1]}}",
		"services: {web: {image: a, depends_on: [1]}}",
		"services: {web: {image: a, command: [1]}}",
		"services: {web: {image: a, healthcheck: {test: [1]}}}",
		"services: {web: {image: 'a:${TAG:-${X}}', ports: ['${P:-80}:80'], volumes: ['/a:/b:ro,z']}}",
		"services: {web: {image: a, environment: {A: ~, B: 1}, labels: [a=b]}}\nvolumes: {v: ~}\nnetworks: {n: ~}",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, file string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Parse(%q) panicked: %v", file, r)
			}
		}()
		_, _ = compose.Parse(t.Context(), []byte(file), map[string]string{"TAG": "1"})
	})
}
