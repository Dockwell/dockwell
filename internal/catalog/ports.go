// SPDX-License-Identifier: AGPL-3.0-only

package catalog

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"
	"google.golang.org/protobuf/proto"

	catalogv1 "github.com/dockwell/dockwell/gen/dockwell/catalog/v1alpha1"
)

// Every port a catalog app publishes answers on the server alone, 127.0.0.1, but one
// its entry declares public on purpose, as a proxy's 80 and 443: a port input with
// public set, which a person approves in the plan (ADR 0101).

// portInput is a port's host part that is an input whole, as ${HTTP_PORT} or
// ${HTTP_PORT:-80}: the input's name, and its fallback.
var portInput = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([^}]*))?\}$`)

// publishedPort is a port of a service's Compose file as its check and its rewrite
// read it: the address it's published on, its host part as written, and its
// protocol.
type publishedPort struct {
	addr, host, protocol string
	// target is the short form's port in the container, with its "/protocol" as
	// written, for the rewrite to put back.
	target string
}

// readPort reads a port as Compose writes it, in its short form, as
// "127.0.0.1:${PORT}:80/udp", or its long one. ok is false for one that publishes
// nothing on the server, as a port Docker picks alone does.
func readPort(v any) (publishedPort, bool) {
	switch p := v.(type) {
	case string:
		return readShortPort(p)
	case int:
		return publishedPort{}, false
	case map[string]any:
		out := publishedPort{protocol: "tcp"}
		out.addr, _ = p["host_ip"].(string)
		switch published := p["published"].(type) {
		case string:
			out.host = published
		case int:
			out.host = strconv.Itoa(published)
		}
		if protocol, ok := p["protocol"].(string); ok && protocol != "" {
			out.protocol = protocol
		}
		return out, out.host != ""
	}
	return publishedPort{}, false
}

// readShortPort reads a port in Compose's short form, split on the colons outside a
// ${...} and an IPv6 address's brackets.
func readShortPort(s string) (publishedPort, bool) {
	var parts []string
	depth, start := 0, 0
	for i := range len(s) {
		switch {
		case strings.HasPrefix(s[i:], "${"), s[i] == '[':
			depth++
		case s[i] == '}' && depth > 0, s[i] == ']' && depth > 0:
			depth--
		case s[i] == ':' && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	parts = append(parts, s[start:])
	out := publishedPort{protocol: "tcp", target: parts[len(parts)-1]}
	if target, protocol, ok := strings.Cut(parts[len(parts)-1], "/"); ok {
		parts[len(parts)-1], out.protocol = target, protocol
	}
	switch len(parts) {
	case 2:
		out.host = parts[0]
	case 3:
		out.addr, out.host = strings.Trim(parts[0], "[]"), parts[1]
	default:
		return publishedPort{}, false
	}
	return out, out.host != ""
}

// everyAddress reports an address that is every address of the server: none, or one
// written as such.
func everyAddress(addr string) bool {
	return addr == "" || addr == "0.0.0.0" || addr == "::"
}

// publicInputs are the names of an entry's inputs that declare their port public.
func publicInputs(e *catalogv1.Entry) map[string]bool {
	out := map[string]bool{}
	for _, in := range e.GetInputs() {
		if in.GetPublic() {
			out[in.GetName()] = true
		}
	}
	return out
}

// publicInput is the public input a port's host part is, whole, or "".
func publicInput(host string, public map[string]bool) string {
	if m := portInput.FindStringSubmatch(host); len(m) > 1 && public[m[1]] {
		return m[1]
	}
	return ""
}

// checkPublic refuses an entry that declares a port public where it can't be one: on
// an input that isn't a port, or one no service publishes whole as its host port, or
// publishes on an address that isn't every address (ADR 0101).
func checkPublic(e *catalogv1.Entry, cf *composeFile) error {
	public := publicInputs(e)
	if len(public) == 0 {
		return nil
	}
	for _, in := range e.GetInputs() {
		if in.GetPublic() && in.GetType() != catalogv1.InputType_INPUT_TYPE_PORT {
			return fmt.Errorf("the input %s is public, which only a port is. Take public away", in.GetName())
		}
	}
	published := map[string]bool{}
	for _, service := range slices.Sorted(maps.Keys(cf.services)) {
		def, _ := cf.services[service].(map[string]any)
		ports, _ := def["ports"].([]any)
		for _, v := range ports {
			p, ok := readPort(v)
			if !ok {
				continue
			}
			name := publicInput(p.host, public)
			if name == "" {
				continue
			}
			if !everyAddress(p.addr) {
				return fmt.Errorf("the input %s is public, and %s publishes it on %s alone. Publish it as \"${%s}:...\", on every address, or take public away", name, service, p.addr, name)
			}
			published[name] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(public)) {
		if !published[name] {
			return fmt.Errorf("the input %s is public, and no service publishes it as its port. Publish it as \"${%s}:...\", or take public away", name, name)
		}
	}
	return nil
}

// loopbackPorts publishes each port of a Compose file on 127.0.0.1, the server
// alone, but one whose host port is a public input of the entry, whole, which stays
// as it is (ADR 0101). An install and an upgrade both run the file it makes, so a
// port an older release published on every address answers on the server alone
// after the upgrade too.
func loopbackPorts(file string, public map[string]bool) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(file), &doc); err != nil || len(doc.Content) != 1 {
		return "", errors.New("its Compose file isn't YAML")
	}
	changed := false
	services := nodeGet(doc.Content[0], "services")
	for i := 0; services != nil && i+1 < len(services.Content); i += 2 {
		ports := nodeGet(services.Content[i+1], "ports")
		if ports == nil || ports.Kind != yaml.SequenceNode {
			continue
		}
		for _, p := range ports.Content {
			switch p.Kind {
			case yaml.ScalarNode:
				port, ok := readShortPort(p.Value)
				if !ok && !strings.Contains(p.Value, ":") {
					// A container port alone, which Docker publishes on every address,
					// on a port it picks.
					p.Value, p.Tag, p.Style = "127.0.0.1::"+p.Value, "!!str", yaml.DoubleQuotedStyle
					changed = true
					continue
				}
				if !ok || publicInput(port.host, public) != "" || port.addr == "127.0.0.1" {
					continue
				}
				// Made again from its parts, so an address that holds the port's own
				// number, as 10.0.0.80:80:84, goes whole (#752's review, L1).
				p.Value, p.Tag, p.Style = "127.0.0.1:"+port.host+":"+port.target, "!!str", yaml.DoubleQuotedStyle
				changed = true
			case yaml.MappingNode:
				published := nodeGet(p, "published")
				if published == nil || publicInput(published.Value, public) != "" {
					continue
				}
				if ip := nodeGet(p, "host_ip"); ip != nil {
					if ip.Value != "127.0.0.1" {
						ip.Value, changed = "127.0.0.1", true
					}
				} else {
					p.Content = append(p.Content,
						&yaml.Node{Kind: yaml.ScalarNode, Value: "host_ip"},
						&yaml.Node{Kind: yaml.ScalarNode, Value: "127.0.0.1"})
					changed = true
				}
			default:
			}
		}
	}
	if !changed {
		return file, nil
	}
	return encode(&doc)
}

// PublicPorts are the ports an entry's services publish on every address on purpose,
// by service, as its public inputs have them in variables: "80", or "443/udp". A
// fallback stands where variables has no value, as Compose's would.
func PublicPorts(e *catalogv1.Entry, variables map[string]string) map[string][]string {
	public := publicInputs(e)
	if len(public) == 0 {
		return nil
	}
	return ports(e, variables, func(p publishedPort) bool {
		return everyAddress(p.addr) && publicInput(p.host, public) != ""
	})
}

// Published are the ports an entry's services publish on the server, by service, as
// variables have them: "8881", or "443/udp".
func Published(e *catalogv1.Entry, variables map[string]string) map[string][]string {
	return ports(e, variables, func(publishedPort) bool { return true })
}

// EveryAddress are the ports an entry's services publish on every address, by
// service, as variables have them, whether it declares them public or not: what an
// install made before ADR 0101 ran, from its pin's file.
func EveryAddress(e *catalogv1.Entry, variables map[string]string) map[string][]string {
	return ports(e, variables, func(p publishedPort) bool { return everyAddress(p.addr) })
}

// ports are the ports of an entry's services that keep says, by service, as
// variables have them, sorted.
func ports(e *catalogv1.Entry, variables map[string]string, keep func(publishedPort) bool) map[string][]string {
	cf, err := readCompose(e.GetCompose())
	if err != nil {
		return nil
	}
	out := map[string][]string{}
	for service, def := range cf.services {
		d, _ := def.(map[string]any)
		list, _ := d["ports"].([]any)
		for _, v := range list {
			p, ok := readPort(v)
			if !ok || !keep(p) {
				continue
			}
			port := p.host
			if m := portInput.FindStringSubmatch(p.host); m != nil {
				port = variables[m[1]]
				if port == "" {
					port = m[2]
				}
			}
			if p.protocol != "" && p.protocol != "tcp" {
				port += "/" + p.protocol
			}
			out[service] = append(out[service], port)
		}
	}
	for service := range out {
		slices.Sort(out[service])
	}
	return out
}

// loopback has an install's file publish each port on the server alone, but those
// its entry declares public (loopbackPorts).
func loopback(out *Needed) error {
	e, err := Loopback(out.Entry)
	out.Entry = e
	return err
}

// Loopback is an entry whose file publishes each port on the server alone, but those
// it declares public (ADR 0101): as Need makes it, for a path that reads the file
// another way, as a change of companion does.
func Loopback(e *catalogv1.Entry) (*catalogv1.Entry, error) {
	file, err := loopbackPorts(e.GetCompose(), publicInputs(e))
	if err != nil || file == e.GetCompose() {
		return e, err
	}
	out := proto.CloneOf(e)
	out.Compose = file
	return out, nil
}

// checkPortForms refuses a service whose ports the rewrite to the server alone can't
// see, as it reads each port where the service writes it: a ports list that's an
// alias, or a port that is; ports a merge key brings in; and a short port whose host
// part it can't read, as a whole port from a default, "${SPEC:-18123:83}". Each
// would otherwise answer on every address with no person's approval (#752's
// review, H1).
func checkPortForms(file string) error {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(file), &doc); err != nil {
		return fmt.Errorf("its Compose file isn't YAML: %w", err)
	}
	if len(doc.Content) != 1 {
		return errors.New("its Compose file isn't YAML of a Compose file: it's empty")
	}
	services := nodeGet(doc.Content[0], "services")
	if services != nil && services.Kind == yaml.AliasNode {
		return fmt.Errorf("its services are an alias, *%s, whose ports Dockwell can't write on the server alone. Write the services out under services", services.Value)
	}
	for i := 0; services != nil && i+1 < len(services.Content); i += 2 {
		name, def := services.Content[i].Value, services.Content[i+1]
		if def.Kind == yaml.AliasNode {
			return fmt.Errorf("%s is an alias, *%s, whose ports Dockwell can't write on the server alone. Write %s out under services", name, def.Value, name)
		}
		for j := 0; def.Kind == yaml.MappingNode && j+1 < len(def.Content); j += 2 {
			if def.Content[j].Value == "<<" && mergesPorts(def.Content[j+1]) {
				return fmt.Errorf("%s takes its ports through a merge key, <<, which Dockwell can't write on the server alone. Write %s's ports out in the service", name, name)
			}
		}
		ports := nodeGet(def, "ports")
		switch {
		case ports == nil:
			continue
		case ports.Kind == yaml.AliasNode:
			return fmt.Errorf("%s's ports are an alias, which Dockwell can't write on the server alone. Write %s's ports out in the service, not through an alias", name, name)
		case ports.Kind != yaml.SequenceNode:
			continue
		}
		for _, p := range ports.Content {
			switch {
			case p.Kind == yaml.AliasNode:
				return fmt.Errorf("%s publishes a port through an alias, *%s, which Dockwell can't write on the server alone. Write the port out in the service", name, p.Value)
			case p.Kind == yaml.MappingNode:
				// A long port's keys as written: a merge key, or an alias for one of its
				// values, such as host_ip: *ip, the rewrite can't write over.
				for j := 0; j+1 < len(p.Content); j += 2 {
					if p.Content[j].Value == "<<" || p.Content[j+1].Kind == yaml.AliasNode {
						return fmt.Errorf("%s publishes a port whose %s comes through an alias, which Dockwell can't write on the server alone. Write the port's keys out in the service", name, p.Content[j].Value)
					}
				}
				continue
			case p.Kind != yaml.ScalarNode:
				continue
			case portInput.MatchString(p.Value):
				// A whole port from an input, ${PORTSPEC}, whose value may hold an
				// address the rewrite can't see.
				return fmt.Errorf("%s publishes %s, a whole port from an input, which Dockwell can't write on the server alone. Write it as \"${PORT}:80\", with the port on the server an input of its own", name, p.Value)
			case !strings.Contains(p.Value, ":"):
				continue
			}
			if _, ok := readShortPort(p.Value); ok || strings.HasPrefix(p.Value, "127.0.0.1::") {
				continue
			}
			return fmt.Errorf("%s publishes %q, whose port on the server Dockwell can't read to write on the server alone. Write it as \"${PORT}:80\", with the port on the server an input of its own", name, p.Value)
		}
	}
	return nil
}

// mergesPorts says a merge key's value, an alias or a list of them, brings a ports
// key into the service.
func mergesPorts(v *yaml.Node) bool {
	switch v.Kind {
	case yaml.AliasNode:
		return mergesPorts(v.Alias)
	case yaml.SequenceNode:
		return slices.ContainsFunc(v.Content, mergesPorts)
	case yaml.MappingNode:
		for j := 0; j+1 < len(v.Content); j += 2 {
			if v.Content[j].Value == "ports" || v.Content[j].Value == "<<" && mergesPorts(v.Content[j+1]) {
				return true
			}
		}
	default:
	}
	return false
}
