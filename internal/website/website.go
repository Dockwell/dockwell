// SPDX-License-Identifier: AGPL-3.0-only

// Package website writes what serves a website from what a person chose for it:
// its route, and the services in its project's environment. A website is a project
// of its own, and Project.website is the source of what it serves, so the control
// plane writes the rest each time. See
// docs/adr/0037-websites-as-projects-of-their-own.md.
package website

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/ingress/caddy"
)

const (
	// WebService is the service that serves a website, when it needs one.
	WebService = "web"
	// RouteName is the route that gives a website its domain.
	RouteName = "site"
	// Port is what WebService listens on in its container.
	Port = 8080
	// Image is the web server's image: the one the ingress pins, which a server
	// with a route has already, by digest, so whatever serves it, Docker checks it
	// is this one.
	Image = caddy.Repository + ":" + caddy.Tag + "@" + caddy.Digest
	// user is who the web server runs as: nobody's, as the ingress's Caddy is.
	user = "65532"
)

// dropped is every capability Docker gives a container but NET_BIND_SERVICE, which
// the image's Caddy carries as a file capability: the kernel refuses to start a
// binary whose file capabilities its bounding set can't grant, so dropping ALL
// would stop it starting. Adding one back would be host access.
var dropped = []string{
	"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD",
	"NET_RAW", "SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT",
}

// Kind is what a website does, in a word the API and the panel share.
type Kind string

const (
	// ProxyToURL sends a website's requests on to a URL.
	ProxyToURL Kind = "proxy_url"
	// ProxyToPort sends them to a port on the server itself.
	ProxyToPort Kind = "proxy_port"
)

// KindOf says what a website does. Empty for one that says nothing.
func KindOf(w *resourcev1.Website) Kind {
	if p := w.GetProxy(); p != nil {
		if p.GetPort() != 0 {
			return ProxyToPort
		}
		return ProxyToURL
	}
	return ""
}

// Check refuses a website that can't be served, saying what to give instead. It
// normalises what it can, such as a URL's trailing slash, so equal settings compare
// equal.
func Check(w *resourcev1.Website) error {
	switch {
	case w == nil:
		return errors.New("a website needs to say what it serves, such as a URL to send its requests to")
	case w.GetProxy() != nil:
		return checkProxy(w.GetProxy())
	}
	return errors.New("a website needs to say what it serves, such as a URL to send its requests to")
}

func checkProxy(p *resourcev1.WebsiteProxy) error {
	raw := strings.TrimSpace(p.GetUrl())
	switch {
	case raw != "" && p.GetPort() != 0:
		return errors.New("a website sends its requests to a URL or to a port on the server, not both. Give one of them")
	case raw == "" && p.GetPort() == 0:
		return errors.New("a website that sends its requests on needs somewhere to send them: a URL, such as https://backend.example.com, or a port on the server, such as 3000")
	case p.GetPort() > 65535:
		return fmt.Errorf("%d isn't a port. Use one from 1 to 65535", p.GetPort())
	case p.GetPort() != 0 && p.GetSendTargetHost():
		return errors.New("a port on the server has no name of its own to send as Host, so the website's domain goes. Turn send_target_host off")
	case p.GetPort() != 0:
		return nil
	}
	clean, err := checkURL(raw)
	if err != nil {
		return err
	}
	p.Url = clean
	return nil
}

// pathRE is what a URL's path may hold: segments of letters, digits and -._~, and
// percent escapes. Nothing that could end its place in a config.
var pathRE = regexp.MustCompile(`^(/([A-Za-z0-9._~-]|%[0-9A-Fa-f]{2})+)+$`)

// hostRE is a DNS name's labels, as a container's resolver takes them.
var hostRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// checkURL reads a URL to send to, and writes it back as it will be used: scheme and
// host in lowercase, with no trailing slash.
func checkURL(raw string) (string, error) {
	example := "such as https://backend.example.com or http://10.0.0.5:8080"
	if strings.ContainsAny(raw, " \t\r\n\"'`{}\\") {
		return "", fmt.Errorf("%q has a space or a character a URL can't hold here. Give a URL %s", raw, example)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return "", fmt.Errorf("%q isn't a URL. Give one %s", raw, example)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		if u.Scheme == "" {
			return "", fmt.Errorf("%q has no http:// or https:// in front. Give a URL %s", raw, example)
		}
		return "", fmt.Errorf("%q starts with %s://, and a website sends on over http or https alone. Give a URL %s", raw, u.Scheme, example)
	case u.User != nil:
		return "", errors.New("the URL has a user name or password in it, which would be in plain sight in the plan. Leave it out: to sign in to what's there, use a route to an app that holds the password as a secret")
	case u.RawQuery != "" || u.ForceQuery:
		return "", fmt.Errorf("%q has a query, which a website can't add to each request. Leave off what follows the ?", raw)
	case u.Fragment != "":
		return "", fmt.Errorf("%q has a #, which never leaves a browser. Leave it off", raw)
	case u.Hostname() == "":
		return "", fmt.Errorf("%q names no host. Give a URL %s", raw, example)
	}
	host := strings.ToLower(u.Hostname())
	if err := checkHost(host); err != nil {
		return "", err
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("%q has the port %s, which isn't one. Use one from 1 to 65535", raw, port)
		}
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	if path != "" {
		if !pathRE.MatchString(path) {
			return "", fmt.Errorf("the path %s has a character a website can't send on. Use letters, digits, -, ., _, ~ and /", u.EscapedPath())
		}
		for _, seg := range strings.Split(path[1:], "/") {
			if seg == "." || seg == ".." {
				return "", fmt.Errorf("the path %s has a %s in it. Give the path it leads to", u.EscapedPath(), seg)
			}
		}
	}
	u.Host = strings.ToLower(u.Host)
	u.Path, u.RawPath = "", ""
	return u.Scheme + "://" + u.Host + path, nil
}

// checkHost takes a DNS name or an address, but not this container's own loopback,
// which is what a container reaches by those names.
func checkHost(host string) error {
	if addr, err := netip.ParseAddr(host); err == nil {
		switch {
		case addr.IsLoopback():
			return fmt.Errorf("%s is the web server's own container, not the server. To send to a port on the server itself, give the port in place of a URL", host)
		case addr.IsUnspecified():
			return fmt.Errorf("%s is no address to send to. Give the address of what you send to", host)
		case addr.Zone() != "":
			return fmt.Errorf("%s names a network interface, which a container doesn't have. Give the address alone", host)
		}
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return fmt.Errorf("%s is the web server's own container, not the server. To send to a port on the server itself, give the port in place of a URL", host)
	}
	if len(host) > 253 || !hostRE.MatchString(host) {
		return fmt.Errorf("%s isn't a host name. Use letters, digits, dashes and dots, such as backend.example.com", host)
	}
	return nil
}

// Serving is what a website's environment holds: its route, with what it serves
// filled in, and the services that serve it, by name.
type Serving struct {
	Route    *resourcev1.Route
	Services map[string]*resourcev1.Service
}

// Front is how a website's domain is reached: the route's own settings for TLS and
// who it believes, which the domain flow sets and a change to what it serves keeps.
type Front struct {
	TLS            string
	TrustedProxies []string
	PlainForAnyone bool
}

// FrontOf is the front a route has.
func FrontOf(r *resourcev1.Route) Front {
	return Front{TLS: r.GetTls(), TrustedProxies: r.GetTrustedProxies(), PlainForAnyone: r.GetPlainForAnyone()}
}

// Resources writes what serves a website at a domain. Check it first.
func Resources(w *resourcev1.Website, domain string, front Front) Serving {
	r := &resourcev1.Route{Domain: domain, Tls: front.TLS, TrustedProxies: front.TrustedProxies, PlainForAnyone: front.PlainForAnyone}
	out := Serving{Route: r, Services: map[string]*resourcev1.Service{}}
	switch KindOf(w) {
	case ProxyToPort:
		r.HostPort = w.GetProxy().GetPort()
	case ProxyToURL:
		r.Service, r.Port = WebService, Port
		out.Services[WebService] = webService(proxyConfig(w.GetProxy()))
	}
	return out
}

// script writes the config the command carries to a file Caddy reads, and starts
// it, as Caddy reads its config from a file and the spec is where it belongs.
const script = `printf '%s\n' "$1" >/tmp/Caddyfile && exec caddy run --config /tmp/Caddyfile --adapter caddyfile`

// webService is the web server, running config.
func webService(config string) *resourcev1.Service {
	return &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image:      Image,
		Entrypoint: []string{"/bin/sh", "-c", script, "web"},
		Command:    []string{config},
		User:       user,
		CapDrop:    dropped,
		// What Caddy writes as it runs, in memory, as nothing of it needs to last.
		Tmpfs: []*resourcev1.Tmpfs{
			{Target: "/tmp", Size: "1M"},
			{Target: "/config", Size: "1M"},
			{Target: "/data", Size: "8M"},
		},
		// A connection, as a request would ask the upstream, and one that was down for
		// a moment would fail the deploy.
		Healthcheck: &resourcev1.Healthcheck{Tcp: &resourcev1.TcpCheck{Port: Port}},
	}}}
}

// Config is the web server's config for a website, as its service runs it. Empty
// for a website that needs no web server.
func Config(w *resourcev1.Website) string {
	if KindOf(w) == ProxyToURL {
		return proxyConfig(w.GetProxy())
	}
	return ""
}

// globals is what every website's web server says first: no admin API, as nothing
// changes it but a deploy; no automatic HTTPS, as TLS ends at the ingress; and the
// ingress's word on who a visitor is, as it reaches the container from a private
// address, and nothing else can reach it from outside the server.
const globals = `{
	admin off
	auto_https off
	persist_config off
	servers {
		trusted_proxies static private_ranges
	}
}
`

// proxyConfig sends every request to a URL, with its path in front.
func proxyConfig(p *resourcev1.WebsiteProxy) string {
	target := p.GetUrl()
	var prefix string
	if u, err := url.Parse(target); err == nil && u.Path != "" {
		prefix = u.EscapedPath()
		target = u.Scheme + "://" + u.Host
	}
	var b strings.Builder
	b.WriteString(globals)
	fmt.Fprintf(&b, "\n:%d {\n", Port)
	b.WriteString("\tlog {\n\t\toutput stderr\n\t\tformat console\n\t}\n")
	if prefix != "" {
		fmt.Fprintf(&b, "\trewrite * %s{uri}\n", prefix)
	}
	fmt.Fprintf(&b, "\treverse_proxy %s", target)
	if p.GetSendTargetHost() {
		b.WriteString(" {\n\t\theader_up Host {upstream_hostport}\n\t}\n")
	} else {
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// ProjectName is the project a website at domain gets, before any number that
// keeps it apart from another: its labels joined with dashes, as a project's name
// is one DNS label, such as blog-example-com.
func ProjectName(domain string) string {
	name := strings.ReplaceAll(strings.ToLower(domain), ".", "-")
	name = strings.Trim(name, "-")
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		name = "site-" + name
	}
	// Room for a number after it, such as -2, under DNS's 63.
	if len(name) > 58 {
		name = strings.TrimRight(name[:58], "-")
	}
	return name
}
