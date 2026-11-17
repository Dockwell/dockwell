// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// addressVariables are the variables of a converted file that name the app's
// address, which each store's own platform fills in, as Runtipi's ${APP_DOMAIN}
// and Coolify's SERVICE_URL_ are, and the converter writes as loopback at web's
// port: each follows the app's address, its domain or the panel's Open address,
// from then on (ADR 0103). A value that names anything else beside the address,
// such as another variable, stays as the file writes it.
func addressVariables(file []byte, web string) []*catalogv1.AddressVariable {
	if web == "" {
		return nil
	}
	doc, err := parseFile(file)
	if err != nil || len(doc.Content) == 0 {
		return nil
	}
	port := "localhost:${" + web + "}"
	var out []*catalogv1.AddressVariable
	for _, name := range serviceNames(doc) {
		// A service on the server's own network, or another container's, reaches the
		// app on loopback from the server's side, which is no browser's address
		// (#779's review, M5).
		if mode := mapGet(service(doc, name), "network_mode"); mode != nil && mode.Value != "" && mode.Value != "bridge" {
			continue
		}
		env := mapGet(service(doc, name), "environment")
		image := ""
		if n := mapGet(service(doc, name), "image"); n != nil {
			image = n.Value
		}
		for _, kv := range serviceEnv(env) {
			if !strings.Contains(kv[1], port) || setOnce[kv[0]] != "" || setOnceIn(image, kv[0]) {
				continue
			}
			value := strings.NewReplacer("http://"+port, "{origin}", "https://"+port, "{origin}").Replace(kv[1])
			value = strings.ReplaceAll(value, port, "{host}")
			if strings.Contains(value, "$") {
				continue
			}
			if aroundAddress.MatchString(value) {
				continue
			}
			if takesOrigin[kv[0]] != "" {
				value = strings.ReplaceAll(value, "{host}", "{origin}")
			}
			// A list names each once, as its http and https items come to one origin.
			if items := strings.Split(value, ","); len(items) > 1 {
				var once []string
				for _, item := range items {
					if !slices.Contains(once, item) {
						once = append(once, item)
					}
				}
				value = strings.Join(once, ",")
			}
			out = append(out, &catalogv1.AddressVariable{Service: name, Variable: kv[0], Value: value})
		}
	}
	slices.SortFunc(out, func(a, b *catalogv1.AddressVariable) int {
		return strings.Compare(a.GetService()+"\x00"+a.GetVariable(), b.GetService()+"\x00"+b.GetVariable())
	})
	return out
}

// serviceEnv is a service's environment as name and value, in a mapping or a list.
func serviceEnv(env *yaml.Node) [][2]string {
	var out [][2]string
	switch {
	case env == nil:
	case env.Kind == yaml.MappingNode:
		for i := 0; i+1 < len(env.Content); i += 2 {
			if v := env.Content[i+1]; v.Kind == yaml.ScalarNode {
				out = append(out, [2]string{env.Content[i].Value, v.Value})
			}
		}
	case env.Kind == yaml.SequenceNode:
		for _, item := range env.Content {
			if name, value, ok := strings.Cut(item.Value, "="); ok && item.Kind == yaml.ScalarNode {
				out = append(out, [2]string{name, value})
			}
		}
	}
	return out
}

// setOnce are variables an app takes as its identity on its first start, which can't
// change after without losing what it made under the old one, by why: each stays
// as the file writes it, and a domain never moves it (#779's review, H3). A
// reviewed list: a variable joins it when its app's docs say it's set once.
var setOnce = map[string]string{
	// A Matrix server's name is in every account and room it makes, and a database
	// made under one name is refused under another.
	"CONDUIT_SERVER_NAME":   "Conduit's server_name",
	"CONDUWUIT_SERVER_NAME": "conduwuit's server_name",
	"TUWUNEL_SERVER_NAME":   "tuwunel's server_name, which can't change without a database wipe",
	"SYNAPSE_SERVER_NAME":   "Synapse's server_name",
	// A federated server's own domain is in every account other servers know.
	"LOCAL_DOMAIN": "Mastodon's LOCAL_DOMAIN, which can't change once set",
	"GTS_HOST":     "GoToSocial's host, which can't change once set",
	// An identity provider's domain is in every token and login it issued.
	"ZITADEL_EXTERNALDOMAIN": "Zitadel's external domain, which its instance is created under",
}

// setOnceByImage are what setOnce is for a name too common to take everywhere, by
// the image that takes it once (#779's check, N4).
var setOnceByImage = map[string]map[string]string{
	// Bonfire is an ActivityPub server: its HOSTNAME is in every account it federates.
	"bonfirenetworks/bonfire": {"HOSTNAME": "Bonfire's HOSTNAME, its federated identity"},
}

// setOnceIn reports a variable that the image takes once.
func setOnceIn(image, variable string) bool {
	repo, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	return setOnceByImage[repo][variable] != ""
}

// takesOrigin are variables a store writes as a host alone that their app reads as
// an address with its scheme, by why: each takes {origin}, so behind HTTPS it says
// https (#779's review, M3). A reviewed list, as setOnce is.
var takesOrigin = map[string]string{
	// TubeArchivist puts http:// before a host with no scheme, and checks a form's
	// origin against it, so behind HTTPS it refuses its own sign-in.
	"TA_HOST": "TubeArchivist's TA_HOST, as its README writes it: http://tubearchivist.local:8000",
}

// aroundAddress is a value that puts its own scheme or port around the address, as
// wss://{origin}/cable, ssh://{host}:23231 or http:{host} do, which no address of
// the app's renders into anything that works: such a value stays as the file writes
// it (#779's review, L1).
var aroundAddress = regexp.MustCompile(`[a-z][a-z0-9+.-]*:(//)?\{(origin|host)\}|\{(origin|host)\}:[0-9]`)
