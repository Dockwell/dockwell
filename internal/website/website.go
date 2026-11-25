// SPDX-License-Identifier: AGPL-3.0-only

// Package website writes what serves a website from what a person chose for it:
// its route, and the services in its project's environment. A website is a project
// of its own, and Project.website is the source of what it serves, so the control
// plane writes the rest each time. See
// docs/adr/0039-websites-as-projects-of-their-own.md.
package website

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/ingress/caddy"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/route"
)

const (
	// WebService is the service that serves a website, when it needs one.
	WebService = "web"
	// RouteName is the route that gives a website its domain.
	RouteName = "site"
	// SiteNetwork is the network of a website's own that its web server joins, so
	// it isn't on Docker's default bridge with whatever else is there, believing
	// what they say of who a visitor is.
	SiteNetwork = "site"
	// Memory is the most the web server may use, as one website mustn't starve
	// another's.
	Memory = "128M"
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
	// Folder serves the files in a folder on the server.
	Folder Kind = "folder"
	// PHP runs the PHP in a folder on the server.
	PHP Kind = "php"
)

// FolderTarget is where a website's folder is in its web server's container.
const FolderTarget = "/srv"

// KindOf says what a website does. Empty for one that says nothing.
func KindOf(w *resourcev1.Website) Kind {
	if p := w.GetProxy(); p != nil {
		if p.GetPort() != 0 {
			return ProxyToPort
		}
		return ProxyToURL
	}
	if w.GetFolder() != nil {
		return Folder
	}
	if w.GetPhp() != nil {
		return PHP
	}
	return ""
}

// Check refuses a website that can't be served, saying what to give instead. It
// normalises what it can, such as a URL's trailing slash, so equal settings compare
// equal.
func Check(w *resourcev1.Website) error {
	var err error
	switch {
	case w == nil:
		return errors.New("a website needs to say what it serves, such as a URL to send its requests to")
	case w.GetProxy() != nil:
		err = checkProxy(w.GetProxy())
	case w.GetFolder() != nil:
		err = checkFolder(w.GetFolder())
	case w.GetPhp() != nil:
		err = checkPHP(w.GetPhp())
	default:
		return errors.New("a website needs to say what it serves, such as a URL to send its requests to")
	}
	if err != nil {
		return err
	}
	// Its other domains, as far as they can be checked without its own, which the
	// route holds: however it's written, as a project's edit can write it too.
	if err := CheckRedirects(w, ""); err != nil {
		return err
	}
	switch w.GetPauseMode() {
	case resourcev1.WebsitePauseMode_WEBSITE_PAUSE_MODE_UNSPECIFIED,
		resourcev1.WebsitePauseMode_WEBSITE_PAUSE_MODE_MAINTENANCE,
		resourcev1.WebsitePauseMode_WEBSITE_PAUSE_MODE_PLAIN_503:
	default:
		return errors.New("pause mode must be maintenance page or plain 503")
	}
	if err := checkRules(w); err != nil {
		return err
	}
	if KindOf(w) == ProxyToPort && strings.TrimSpace(w.GetCustomCaddy()) != "" {
		return errors.New("a port on the server is served straight from the ingress without a web server of its own, so custom Caddy config applies to URL, folder and PHP websites")
	}
	custom, err := CheckCustomCaddy(w.GetCustomCaddy(), AllowedRoot(w))
	if err != nil {
		return err
	}
	w.CustomCaddy = custom
	return nil
}

// MaxRedirects is how many other domains a website sends to itself, which covers
// www and a few old names with room to spare.
const MaxRedirects = 20

// CheckRedirects refuses other domains a website at domain can't be sent from,
// saying why. It writes them lower-case and trimmed, as a route's domain is.
func CheckRedirects(w *resourcev1.Website, domain string) error {
	if len(w.GetRedirectFrom()) > MaxRedirects {
		return fmt.Errorf("a website takes up to %d other domains, and this names %d. Keep the ones visitors still use", MaxRedirects, len(w.GetRedirectFrom()))
	}
	seen := map[string]bool{}
	for i, d := range w.GetRedirectFrom() {
		d = strings.ToLower(strings.TrimSpace(d))
		w.RedirectFrom[i] = d
		if err := route.CheckDomain(d); err != nil {
			return err
		}
		switch {
		case d == domain:
			return fmt.Errorf("%s is the website's own domain, so it can't redirect to itself. Take it out of the other domains", d)
		case seen[d]:
			return fmt.Errorf("%s is named twice in the other domains. Name it once", d)
		}
		seen[d] = true
	}
	return nil
}

// RedirectName is the name of the route that sends from to a website, before any
// number that keeps it apart from another, as a project's is: www-example-com. A
// domain has a dot, so it's never the site's own route's name.
func RedirectName(from string) string {
	return ProjectName(from)
}

// indexRE is a file name an index can have: nothing that could leave its place in
// a config, or lead out of the folder.
var indexRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// folderRE is what a folder's path may hold, as it goes into a mount and a plan.
var folderRE = regexp.MustCompile(`^(/[A-Za-z0-9._@+~-]+)+/?$`)

func checkFolder(f *resourcev1.WebsiteFolder) error {
	root := strings.TrimSpace(f.GetRoot())
	switch {
	case root == "":
		return errors.New("a website that serves a folder needs the folder, as an absolute path on the server, such as /var/www/blog")
	case !strings.HasPrefix(root, "/"):
		return fmt.Errorf("%s isn't an absolute path. Start it with /, such as /var/www/blog", root)
	case root == "/":
		return errors.New("a website can't serve the whole server. Give the folder that holds its files, such as /var/www/blog")
	case !folderRE.MatchString(root):
		return fmt.Errorf("%q has a character a folder's path can't hold here. Use letters, digits, -, ., _, @, +, ~ and /", root)
	}
	for _, part := range strings.Split(strings.Trim(root, "/"), "/") {
		if part == "." || part == ".." {
			return fmt.Errorf("%s has a %s in it. Give the folder it leads to", root, part)
		}
	}
	f.Root = path.Clean(root)
	if o := strings.TrimSpace(f.GetOwner()); o != "" {
		uid, gid, err := resource.ParseOwner(o)
		if err != nil {
			return fmt.Errorf("the folder's owner: %w", err)
		}
		f.Owner = resource.FormatOwner(uid, gid)
	} else {
		f.Owner = ""
	}
	seen := map[string]bool{}
	var index []string
	for _, name := range f.GetIndex() {
		name = strings.TrimSpace(name)
		switch {
		case name == "" || seen[name]:
			continue
		case !indexRE.MatchString(name):
			return fmt.Errorf("%q isn't a file name an index can have. Give a name such as index.html, with no folder in it", name)
		}
		seen[name] = true
		index = append(index, name)
	}
	f.Index = index
	return nil
}

// Index is the files a folder's website looks for in a folder, in order.
func Index(f *resourcev1.WebsiteFolder) []string {
	if len(f.GetIndex()) > 0 {
		return f.GetIndex()
	}
	return []string{"index.html", "index.htm"}
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
		if slices.Contains(dockerPorts, n) {
			return "", fmt.Errorf("port %d is Docker's API, which runs anything as root on the server it answers for, so a website can't send to it on any host. Send to the app's own port", n)
		}
	}
	under := strings.TrimRight(u.EscapedPath(), "/")
	if under != "" {
		if !pathRE.MatchString(under) {
			return "", fmt.Errorf("the path %s has a character a website can't send on. Use letters, digits, -, ., _, ~ and /", u.EscapedPath())
		}
		for _, seg := range strings.Split(under[1:], "/") {
			if seg == "." || seg == ".." {
				return "", fmt.Errorf("the path %s has a %s in it. Give the path it leads to", u.EscapedPath(), seg)
			}
		}
	}
	u.Host = strings.ToLower(u.Host)
	u.Path, u.RawPath = "", ""
	return u.Scheme + "://" + u.Host + under, nil
}

// metadataNames are what clouds name their metadata services by, which hand the
// server's own credentials to whatever asks.
var metadataNames = []string{"metadata", "metadata.google.internal", "metadata.goog", "instance-data", "instance-data.ec2.internal"}

// metadataAddrs are metadata services' addresses outside link-local ranges:
// Alibaba Cloud's, and AWS's over IPv6.
var metadataAddrs = []netip.Addr{netip.MustParseAddr("100.100.100.200"), netip.MustParseAddr("fd00:ec2::254")}

// dockerPorts are Docker's API, which runs anything as root on the server it
// answers for, plain and over TLS.
var dockerPorts = []int{2375, 2376}

const metadataWords = "where a cloud keeps its metadata service, which hands the server's own credentials to whatever asks. A website can't send there"

// checkHost takes a DNS name or an address, but not this container's own loopback,
// which is what a container reaches by those names, nor a cloud's metadata
// service. A name that resolves to one is the operator's to keep away: Dockwell
// sees only the name.
func checkHost(host string) error {
	if addr, err := netip.ParseAddr(host); err == nil {
		addr = addr.Unmap()
		switch {
		case addr.IsLoopback():
			return fmt.Errorf("%s is the web server's own container, not the server. To send to a port on the server itself, give the port in place of a URL", host)
		case addr.IsUnspecified():
			return fmt.Errorf("%s is no address to send to. Give the address of what you send to", host)
		case addr.Zone() != "":
			return fmt.Errorf("%s names a network interface, which a container doesn't have. Give the address alone", host)
		case addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast():
			return fmt.Errorf("%s is a link-local address, %s", host, metadataWords)
		case slices.Contains(metadataAddrs, addr):
			return fmt.Errorf("%s is %s", host, metadataWords)
		}
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return fmt.Errorf("%s is the web server's own container, not the server. To send to a port on the server itself, give the port in place of a URL", host)
	}
	if slices.Contains(metadataNames, strings.TrimSuffix(host, ".")) {
		return fmt.Errorf("%s is the name %s", host, metadataWords)
	}
	if len(host) > 253 || !hostRE.MatchString(host) {
		return fmt.Errorf("%s isn't a host name. Use letters, digits, dashes and dots, such as backend.example.com", host)
	}
	return nil
}

// Serving is what a website's environment holds: its route, with what it serves
// filled in, and the services and networks that serve it, by name.
type Serving struct {
	Route *resourcev1.Route
	// Redirects are the routes that send its other domains to it, by domain.
	Redirects map[string]*resourcev1.Route
	Services  map[string]*resourcev1.Service
	// Networks are those its services share, by name.
	Networks map[string]*resourcev1.Network
}

// Front is how a website's domain is reached: the route's own settings for TLS and
// who it believes, which the domain flow sets, and who it lets in (ADR 0087), which
// making it private sets. A change to what it serves keeps them all.
type Front struct {
	TLS            string
	TrustedProxies []string
	PlainForAnyone bool
	Access         *resourcev1.RouteAccess
	// Publish is how its domain is published at Cloudflare (ADR 0104), and Proof the
	// DNS provider that proves its certificate (ADR 0088), if either is set.
	Publish *resourcev1.RoutePublish
	Proof   string
	// CustomCert is a custom PEM certificate and key uploaded for the route, if any.
	CustomCert *resourcev1.RouteCustomCert
	// SubpathMembers are other Dockwell services attached to the website's network
	// for sub-path reverse proxies targeting service_id.
	SubpathMembers []*resourcev1.NetworkMember
}

// FrontOf is the front a route has.
func FrontOf(r *resourcev1.Route) Front {
	return Front{
		TLS: r.GetTls(), TrustedProxies: r.GetTrustedProxies(), PlainForAnyone: r.GetPlainForAnyone(), Access: proto.CloneOf(r.GetAccess()),
		Publish: proto.CloneOf(r.GetPublish()), Proof: r.GetProof(), CustomCert: proto.CloneOf(r.GetCustomCert()),
	}
}

// redirectPublish is how a name that redirects to a website is published: the same
// way as the website's own, taking over nothing of its own.
func redirectPublish(p *resourcev1.RoutePublish) *resourcev1.RoutePublish {
	if p == nil {
		return nil
	}
	out := proto.CloneOf(p)
	out.TakesOver = nil
	return out
}

// Resources writes what serves a website at a domain. Check it first.
func Resources(w *resourcev1.Website, domain string, front Front) Serving {
	r := &resourcev1.Route{
		Domain: domain, Tls: front.TLS, TrustedProxies: front.TrustedProxies, PlainForAnyone: front.PlainForAnyone, Access: front.Access,
		Publish: proto.CloneOf(front.Publish), Proof: front.Proof,
		RateLimit:  RouteRateLimit(w.GetRules().GetRateLimit()),
		CustomCert: proto.CloneOf(front.CustomCert),
	}
	out := Serving{Route: r, Redirects: map[string]*resourcev1.Route{}, Services: map[string]*resourcev1.Service{}, Networks: map[string]*resourcev1.Network{}}
	// Visitors reach a site behind a tunnel over HTTPS too, so only a site served
	// over plain HTTP to anyone sends them to http://.
	scheme := "https://"
	if front.PlainForAnyone {
		scheme = "http://"
	}
	for _, from := range w.GetRedirectFrom() {
		out.Redirects[from] = &resourcev1.Route{
			Domain: from, Tls: front.TLS, TrustedProxies: front.TrustedProxies, PlainForAnyone: front.PlainForAnyone,
			Redirect: &resourcev1.RouteRedirect{To: scheme + domain}, Publish: redirectPublish(front.Publish), Proof: front.Proof,
		}
	}
	if w.GetPaused() {
		r.Service, r.Port = WebService, Port
		web := webService(pausedConfig(w))
		switch KindOf(w) {
		case Folder:
			web.GetApp().Mounts = []*resourcev1.Mount{{
				Source:    &resourcev1.Mount_HostPath{HostPath: w.GetFolder().GetRoot()},
				Target:    FolderTarget,
				ReadOnly:  true,
				MakeOwner: w.GetFolder().GetOwner(),
			}}
		case PHP:
			user := w.GetPhp().GetUser()
			if user == "" {
				user = DefaultPHPUser
			}
			web.GetApp().Mounts = []*resourcev1.Mount{{
				Source:    &resourcev1.Mount_HostPath{HostPath: w.GetPhp().GetRoot()},
				Target:    PHPTarget,
				ReadOnly:  true,
				MakeOwner: user,
			}}
		case ProxyToURL, ProxyToPort:
			// Proxied sites have no host folder to keep mounted while paused.
		}
		out.Services[WebService] = web
		out.Networks[SiteNetwork] = &resourcev1.Network{}
		return out
	}
	switch KindOf(w) {
	case ProxyToPort:
		r.HostPort = w.GetProxy().GetPort()
	case ProxyToURL:
		r.Service, r.Port = WebService, Port
		out.Services[WebService] = webService(proxyConfig(w))
		out.Networks[SiteNetwork] = &resourcev1.Network{}
	case Folder:
		r.Service, r.Port = WebService, Port
		web := webService(folderConfig(w))
		// Read-only, as serving a site writes nothing, and what's served can't be
		// changed through it.
		web.GetApp().Mounts = []*resourcev1.Mount{{
			Source:   &resourcev1.Mount_HostPath{HostPath: w.GetFolder().GetRoot()},
			Target:   FolderTarget,
			ReadOnly: true,
			// Docker makes a folder that isn't there root's, which nobody else can put
			// the site's files in: Dockwell makes it its owner's.
			MakeOwner: w.GetFolder().GetOwner(),
		}}
		out.Services[WebService] = web
		out.Networks[SiteNetwork] = &resourcev1.Network{}
	case PHP:
		phpServing(w, &out)
	}
	if web := out.Services[WebService]; web != nil {
		if env := basicAuthEnv(w); len(env) > 0 {
			web.GetApp().Env = env
		}
	}
	if net := out.Networks[SiteNetwork]; net != nil && len(front.SubpathMembers) > 0 {
		members := make([]*resourcev1.NetworkMember, len(front.SubpathMembers))
		for i, m := range front.SubpathMembers {
			members[i] = proto.CloneOf(m)
		}
		net.Members = members
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
		// Its own network, and the ingress's that its route adds, and not Docker's
		// default bridge.
		Networks:  []string{SiteNetwork},
		Resources: &resourcev1.Resources{Memory: Memory},
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
	if w.GetPaused() {
		return pausedConfig(w)
	}
	switch KindOf(w) {
	case ProxyToURL:
		return proxyConfig(w)
	case Folder:
		return folderConfig(w)
	case PHP:
		return phpConfig(w)
	case ProxyToPort:
		// The ingress sends it straight to the port.
	}
	return ""
}

// writeWellKnown writes the /.well-known/ handler indented by baseIndent.
func writeWellKnown(b *strings.Builder, baseIndent string) {
	fmt.Fprintf(b, "%s@hidden path_regexp ^/\\.well-known/(.*/)?\\.\n", baseIndent)
	fmt.Fprintf(b, "%shandle /.well-known/* {\n", baseIndent)
	fmt.Fprintf(b, "%s\trespond @hidden 404\n", baseIndent)
	fmt.Fprintf(b, "%s\tfile_server {\n%s\t\thide %s\n%s\t}\n", baseIndent, baseIndent, strings.Join(Hidden, " "), baseIndent)
	fmt.Fprintf(b, "%s}\n", baseIndent)
}

// folderConfig serves the folder as it is, with its index for a folder, and for a
// single-page app, the top index for a path with no file.
func folderConfig(w *resourcev1.Website) string {
	f := w.GetFolder()
	var b strings.Builder
	b.WriteString(globals)
	fmt.Fprintf(&b, "\n:%d {\n", Port)
	b.WriteString("\tlog {\n\t\toutput stderr\n\t\tformat console\n\t}\n")
	fmt.Fprintf(&b, "\troot * %s\n", FolderTarget)
	b.WriteString("\tencode zstd gzip\n")
	ordered := needsOrderedAuthRoute(w.GetRules())
	baseIndent := "\t"
	if ordered {
		b.WriteString("\troute {\n")
		baseIndent = "\t\t"
	}
	writeRules(&b, w.GetRules(), baseIndent)
	writeWellKnown(&b, baseIndent)
	fmt.Fprintf(&b, "%shandle {\n", baseIndent)
	custom := strings.TrimSpace(w.GetCustomCaddy()) != ""
	indent := baseIndent + "\t"
	if custom {
		fmt.Fprintf(&b, "%s\troute {\n", baseIndent)
		writeCustomCaddy(&b, w.GetCustomCaddy(), baseIndent+"\t\t")
		indent = baseIndent + "\t\t"
	}
	index := Index(f)
	if f.GetSinglePageApp() {
		fmt.Fprintf(&b, "%stry_files {path} {path}/ /%s\n", indent, index[0])
	}
	fmt.Fprintf(&b, "%sfile_server {\n%s\tindex %s\n%s\thide .* %s\n%s}\n", indent, indent, strings.Join(index, " "), indent, strings.Join(Hidden, " "), indent)
	if custom {
		fmt.Fprintf(&b, "%s\t}\n", baseIndent)
	}
	fmt.Fprintf(&b, "%s}\n", baseIndent)
	if ordered {
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// Hidden are the files no website's folder serves as they are, besides what starts
// with a dot, such as .git and .env: scripts, whose source holds what they keep from
// visitors, such as wp-config.php's password, and the copies editors and tools leave
// of them. A PHP website runs its scripts, and hides the rest.
var Hidden = []string{"*.php", "*.phtml", "*.phar", "*.php~", "*.php.*", "*.bak", "*.save", "*.orig", "*.swp"}

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
func proxyConfig(w *resourcev1.Website) string {
	p := w.GetProxy()
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
	ordered := needsOrderedAuthRoute(w.GetRules())
	baseIndent := "\t"
	if ordered {
		b.WriteString("\troute {\n")
		baseIndent = "\t\t"
	}
	writeRules(&b, w.GetRules(), baseIndent)
	custom := strings.TrimSpace(w.GetCustomCaddy()) != ""
	wrap := ordered || len(w.GetRules().GetSubpathProxies()) > 0 || w.GetRules().GetCors().GetEnabled() || custom
	indent := baseIndent
	if wrap {
		fmt.Fprintf(&b, "%shandle {\n", baseIndent)
		indent = baseIndent + "\t"
	}
	if custom {
		fmt.Fprintf(&b, "%s\troute {\n", baseIndent)
		writeCustomCaddy(&b, w.GetCustomCaddy(), baseIndent+"\t\t")
		indent = baseIndent + "\t\t"
	}
	if prefix != "" {
		fmt.Fprintf(&b, "%srewrite * %s{uri}\n", indent, prefix)
	}
	fmt.Fprintf(&b, "%sreverse_proxy %s", indent, target)
	if p.GetSendTargetHost() {
		fmt.Fprintf(&b, " {\n%s\theader_up Host {upstream_hostport}\n%s}\n", indent, indent)
	} else {
		b.WriteString("\n")
	}
	if custom {
		fmt.Fprintf(&b, "%s\t}\n", baseIndent)
	}
	if wrap {
		fmt.Fprintf(&b, "%s}\n", baseIndent)
	}
	if ordered {
		b.WriteString("\t}\n")
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
