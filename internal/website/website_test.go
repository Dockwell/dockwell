// SPDX-License-Identifier: AGPL-3.0-only

package website

import (
	"slices"
	"strings"
	"testing"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

func proxy(url string, port uint32) *resourcev1.Website {
	return &resourcev1.Website{Serves: &resourcev1.Website_Proxy{Proxy: &resourcev1.WebsiteProxy{Url: url, Port: port}}}
}

// A URL is written back as it's used, and anything that could leave its place in a
// config, or that a container can't reach as the person meant, is refused, saying
// what to give instead.
func TestAURLToSendTo(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://backend.example.com", "https://backend.example.com"},
		{"HTTPS://Backend.Example.com/", "https://backend.example.com"},
		{"  http://10.0.0.5:8080  ", "http://10.0.0.5:8080"},
		{"http://10.0.0.5:8080/app/", "http://10.0.0.5:8080/app"},
		{"https://[2001:db8::5]:8443/a/b", "https://[2001:db8::5]:8443/a/b"},
		{"http://backend/%7Euser", "http://backend/%7Euser"},
	} {
		w := proxy(tc.in, 0)
		if err := Check(w); err != nil {
			t.Errorf("Check(%q) = %v", tc.in, err)
			continue
		}
		if got := w.GetProxy().GetUrl(); got != tc.want {
			t.Errorf("Check(%q) wrote %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, says string }{
		{"backend.example.com", "no http:// or https://"},
		{"ftp://backend.example.com", "over http or https alone"},
		{"https://user:pass@backend.example.com", "user name or password"},
		{"https://backend.example.com/?a=1", "has a query"},
		{"https://backend.example.com/#top", "never leaves a browser"},
		{"http://127.0.0.1:3000", "give the port in place of a URL"},
		{"http://localhost:3000", "give the port in place of a URL"},
		{"http://[::1]:3000", "give the port in place of a URL"},
		{"http://0.0.0.0", "no address"},
		{"http://back_end.example.com", "isn't a host name"},
		{"http://backend.example.com:0", "isn't one"},
		{"http://backend.example.com:70000", "isn't one"},
		{"http://backend.example.com/a b", "a space"},
		{"http://backend.example.com/{path}", "a character a URL can't hold"},
		{"http://backend.example.com/a\n}", "a space"},
		{"http://backend.example.com/a;b", "a character a website can't send on"},
		{"http://backend.example.com/a/../b", "the path it leads to"},
		{"https://", "names no host"},
		{"http://169.254.169.254/latest/meta-data", "metadata service"},
		{"http://[fe80::1]:8080", "link-local"},
		{"http://metadata.google.internal/computeMetadata/v1", "metadata service"},
		{"http://metadata/computeMetadata/v1", "metadata service"},
		{"http://100.100.100.200/latest/meta-data", "metadata service"},
		{"http://[fd00:ec2::254]/latest/meta-data", "metadata service"},
		{"http://[::ffff:169.254.169.254]/", "link-local"},
		{"http://172.17.0.1:2375", "Docker's API"},
		{"https://docker.example.com:2376", "Docker's API"},
	} {
		err := Check(proxy(tc.in, 0))
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("Check(%q) = %v, want it to say %q", tc.in, err, tc.says)
		}
	}
}

func TestWhatAProxyNeeds(t *testing.T) {
	for _, tc := range []struct {
		w    *resourcev1.Website
		says string
	}{
		{nil, "needs to say what it serves"},
		{&resourcev1.Website{}, "needs to say what it serves"},
		{proxy("", 0), "needs somewhere to send them"},
		{proxy("https://backend.example.com", 3000), "not both"},
		{proxy("", 70000), "isn't a port"},
		{&resourcev1.Website{Serves: &resourcev1.Website_Proxy{Proxy: &resourcev1.WebsiteProxy{Port: 3000, SendTargetHost: true}}}, "no name of its own"},
	} {
		err := Check(tc.w)
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("Check(%v) = %v, want it to say %q", tc.w, err, tc.says)
		}
	}
	if err := Check(proxy("", 3000)); err != nil {
		t.Errorf("a port on the server: %v", err)
	}
}

// A port on the server is a route alone, and a URL is the web server with a route to
// it. Either keeps what's in front of the domain as the route had it.
func TestWhatServesEachKind(t *testing.T) {
	front := Front{TLS: "off", TrustedProxies: []string{"127.0.0.1"}}
	port := Resources(proxy("", 3000), "app.example.com", front)
	if len(port.Services) != 0 {
		t.Errorf("a port on the server has services: %v", port.Services)
	}
	if r := port.Route; r.GetHostPort() != 3000 || r.GetService() != "" || r.GetPort() != 0 || r.GetDomain() != "app.example.com" || r.GetTls() != "off" || r.GetTrustedProxies()[0] != "127.0.0.1" {
		t.Errorf("a port on the server's route is %v", r)
	}

	url := Resources(proxy("https://backend.example.com/app", 0), "app.example.com", Front{})
	if r := url.Route; r.GetService() != WebService || r.GetPort() != Port || r.GetHostPort() != 0 || r.GetTls() != "" {
		t.Errorf("a URL's route is %v", r)
	}
	web := url.Services[WebService].GetApp()
	if web == nil {
		t.Fatalf("a URL has no web server: %v", url.Services)
	}
	if web.GetImage() != Image || !strings.Contains(web.GetImage(), "@sha256:") {
		t.Errorf("the web server runs %s, want the pinned %s", web.GetImage(), Image)
	}
	if web.GetUser() != "65532" || !slices.Contains(web.GetCapDrop(), "NET_RAW") || !slices.Contains(web.GetCapDrop(), "SETUID") {
		t.Errorf("the web server runs as %q, dropping %v", web.GetUser(), web.GetCapDrop())
	}
	// Caddy's binary carries this one as a file capability, and won't start without it.
	if slices.Contains(web.GetCapDrop(), "NET_BIND_SERVICE") || slices.Contains(web.GetCapDrop(), "ALL") || len(web.GetCapAdd()) != 0 {
		t.Errorf("the web server drops %v and adds %v, and Caddy's file capability needs NET_BIND_SERVICE kept, which adding would make host access", web.GetCapDrop(), web.GetCapAdd())
	}
	if !slices.Equal(web.GetNetworks(), []string{SiteNetwork}) || url.Networks[SiteNetwork] == nil {
		t.Errorf("the web server joins %v of %v, want a network of its own, off Docker's default bridge", web.GetNetworks(), url.Networks)
	}
	if web.GetResources().GetMemory() != Memory {
		t.Errorf("the web server may use %q of memory, want %s", web.GetResources().GetMemory(), Memory)
	}
	if len(port.Networks) != 0 {
		t.Errorf("a port on the server has networks: %v", port.Networks)
	}
	if web.GetHealthcheck().GetTcp().GetPort() != Port {
		t.Errorf("the web server's check is %v, want a connection to %d", web.GetHealthcheck(), Port)
	}
	if len(web.GetCommand()) != 1 || web.GetCommand()[0] != Config(proxy("https://backend.example.com/app", 0)) {
		t.Errorf("the web server's command is %q", web.GetCommand())
	}
	if len(web.GetReferences()) != 0 {
		t.Errorf("the config's $ would be read as a value: %v", web.GetReferences())
	}
}

func TestTheProxysConfig(t *testing.T) {
	got := Config(proxy("https://backend.example.com/app", 0))
	for _, want := range []string{"admin off", "auto_https off", "trusted_proxies static private_ranges", "\n:8080 {", "rewrite * /app{uri}", "reverse_proxy https://backend.example.com\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("the config has no %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "header_up") {
		t.Errorf("the config sends another Host when it wasn't asked to:\n%s", got)
	}
	host := &resourcev1.Website{Serves: &resourcev1.Website_Proxy{Proxy: &resourcev1.WebsiteProxy{Url: "https://backend.example.com", SendTargetHost: true}}}
	if got := Config(host); !strings.Contains(got, "header_up Host {upstream_hostport}") || strings.Contains(got, "rewrite") {
		t.Errorf("the config for the target's own name:\n%s", got)
	}
	if got := Config(proxy("", 3000)); got != "" {
		t.Errorf("a port on the server has a config:\n%s", got)
	}
}

func TestAWebsitesProjectName(t *testing.T) {
	for in, want := range map[string]string{
		"blog.example.com":                       "blog-example-com",
		"Shop.Example.COM":                       "shop-example-com",
		"1password.example":                      "site-1password-example",
		strings.Repeat("a", 60) + ".example.com": strings.Repeat("a", 58),
	} {
		if got := ProjectName(in); got != want {
			t.Errorf("ProjectName(%q) = %q, want %q", in, got, want)
		}
	}
}
