// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
)

const (
	// metadataAddress is where every cloud provider's metadata service answers.
	// It's link-local, so a read never leaves the server's own network.
	metadataAddress = "http://169.254.169.254"
	// metadataWait bounds asking all of them. A server on no cloud spends this,
	// on the side, and nothing waits for it.
	metadataWait = 2 * time.Second
	// metadataMost is the most of an answer that's read.
	metadataMost = 512
)

// metadataService is how to ask one provider's metadata service about a server.
type metadataService struct {
	provider dockwellv1.CloudProvider
	// sessionPath asks for what the requests need first, by PUT, for AWS's IMDSv2.
	sessionPath string
	// sessionHeader names the header that carries what that gives.
	sessionHeader string
	// headers go on each request, as a provider refuses others.
	headers map[string]string
	// identify answers on this provider alone. It's the address's path if empty.
	identify string
	// address is the path of its public IPv4 address, for one that gives it.
	address string
}

// metadataServices are the providers an agent can ask. Oracle's gives no public
// address, but says it's Oracle, which tells a person where to look.
var metadataServices = []metadataService{
	{
		provider:    dockwellv1.CloudProvider_CLOUD_PROVIDER_AWS,
		sessionPath: "/latest/api/token", sessionHeader: "X-Aws-Ec2-Metadata-Token",
		headers: map[string]string{"X-Aws-Ec2-Metadata-Token-Ttl-Seconds": "60"},
		address: "/latest/meta-data/public-ipv4",
	},
	{
		provider: dockwellv1.CloudProvider_CLOUD_PROVIDER_GCP,
		headers:  map[string]string{"Metadata-Flavor": "Google"},
		identify: "/computeMetadata/v1/instance/id",
		address:  "/computeMetadata/v1/instance/network-interfaces/0/access-configs/0/external-ip",
	},
	{
		provider: dockwellv1.CloudProvider_CLOUD_PROVIDER_AZURE,
		headers:  map[string]string{"Metadata": "true"},
		identify: "/metadata/instance/compute/vmId?api-version=2021-02-01&format=text",
		address:  "/metadata/instance/network/interface/0/ipv4/ipAddress/0/publicIpAddress?api-version=2021-02-01&format=text",
	},
	{
		provider: dockwellv1.CloudProvider_CLOUD_PROVIDER_DIGITALOCEAN,
		identify: "/metadata/v1/id",
		address:  "/metadata/v1/interfaces/public/0/ipv4/address",
	},
	{
		provider: dockwellv1.CloudProvider_CLOUD_PROVIDER_HETZNER,
		identify: "/hetzner/v1/metadata/instance-id",
		address:  "/hetzner/v1/metadata/public-ipv4",
	},
	{
		provider: dockwellv1.CloudProvider_CLOUD_PROVIDER_ORACLE,
		headers:  map[string]string{"Authorization": "Bearer Oracle"},
		identify: "/opc/v2/instance/id",
	},
}

// Network finds out what this server can of how the internet sees it, beyond its
// interfaces: what a cloud provider's metadata service says, and with lookup, what a
// public STUN server sees. Each is bounded, and with nothing to answer, each says
// nothing. A server with a public IPv4 address of its own has nothing to ask a third
// party. Reading the metadata sends nothing off the server's own network.
func (c *Collector) Network(ctx context.Context, lookup bool) *agentv1.Network {
	looked := make(chan string, 1)
	go func() {
		if !lookup || c.hasPublicIPv4() {
			looked <- ""
			return
		}
		servers := c.STUN
		if servers == nil {
			servers = stunServers
		}
		looked <- stunLookup(ctx, servers)
	}()
	out := &agentv1.Network{}
	if cloud := c.cloud(ctx); cloud != nil {
		out.Provider, out.MetadataAddress = cloud.provider, cloud.address
	}
	out.LookedUpAddress = <-looked
	return out
}

// hasPublicIPv4 reports a public IPv4 address on an interface.
func (c *Collector) hasPublicIPv4() bool {
	addrs, err := c.Addresses()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(ipn.IP); ok && ip.Unmap().Is4() && resource.IsPublicAddress(ip) {
				return true
			}
		}
	}
	return false
}

// cloudSays is what a cloud provider's metadata service says of a server.
type cloudSays struct {
	provider dockwellv1.CloudProvider
	address  string
}

// cloud asks this server's cloud provider what it says of the server, or nil when
// none answers, as on a server that isn't on a cloud. It takes at most metadataWait.
func (c *Collector) cloud(ctx context.Context) *cloudSays {
	ctx, cancel := context.WithTimeout(ctx, metadataWait)
	defer cancel()
	base := c.Metadata
	if base == "" {
		base = metadataAddress
	}
	// Directly, whatever a proxy this server has for the internet says, and
	// never kept open, as this is the once.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	answers := make(chan *cloudSays, len(metadataServices))
	for _, s := range metadataServices {
		go func() { answers <- s.ask(ctx, client, base) }()
	}
	var found *cloudSays
	for range metadataServices {
		if answer := <-answers; answer != nil && found == nil {
			found = answer
			cancel()
		}
	}
	return found
}

// ask is what one provider says, or nil unless it's that provider that answered.
func (s metadataService) ask(ctx context.Context, client *http.Client, base string) *cloudSays {
	headers := s.headers
	if s.sessionPath != "" {
		session, ok := metadataRead(ctx, client, http.MethodPut, base+s.sessionPath, headers)
		if !ok {
			return nil
		}
		headers = map[string]string{s.sessionHeader: session}
	}
	if s.identify != "" {
		if _, ok := metadataRead(ctx, client, http.MethodGet, base+s.identify, headers); !ok {
			return nil
		}
	}
	out := &cloudSays{provider: s.provider}
	if s.address != "" {
		// A server with no public address says so with a 404, and it's still there.
		if address, ok := metadataRead(ctx, client, http.MethodGet, base+s.address, headers); ok {
			if resource.PublicAddresses([]string{address}) != nil {
				out.address = address
			}
		}
	}
	return out
}

// metadataRead is the body of an answer, trimmed, and whether there was one. A
// Google answer has to say it's from Google, as the header's what tells it from
// anything else on the network.
func metadataRead(ctx context.Context, client *http.Client, method, url string, headers map[string]string) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, method, url, http.NoBody)
	if err != nil {
		return "", false
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || (headers["Metadata-Flavor"] != "" && res.Header.Get("Metadata-Flavor") != headers["Metadata-Flavor"]) {
		return "", false
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, metadataMost))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(body)), true
}
