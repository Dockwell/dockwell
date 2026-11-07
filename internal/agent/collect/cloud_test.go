// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
)

type answer struct {
	body   string
	header map[string]string
	wants  map[string]string
}

// metadataStandIn answers what a provider's service does, refusing what it would.
func metadataStandIn(t *testing.T, routes map[string]answer) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := routes[r.Method+" "+r.URL.RequestURI()]
		if !ok {
			http.NotFound(w, r)
			return
		}
		for k, v := range a.wants {
			if r.Header.Get(k) != v {
				http.Error(w, "refused", http.StatusForbidden)
				return
			}
		}
		for k, v := range a.header {
			w.Header().Set(k, v)
		}
		_, _ = w.Write([]byte(a.body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func privateServer(metadata string) *Collector {
	return &Collector{
		Root:     fstest.MapFS{},
		Metadata: metadata,
		Addresses: func() ([]net.Addr, error) {
			return []net.Addr{&net.IPNet{IP: net.ParseIP("10.0.0.25"), Mask: net.CIDRMask(24, 32)}}, nil
		},
	}
}

func TestEachProvidersMetadataIsRead(t *testing.T) {
	google := map[string]string{"Metadata-Flavor": "Google"}
	azure := map[string]string{"Metadata": "true"}
	for _, tc := range []struct {
		name     string
		routes   map[string]answer
		provider dockwellv1.CloudProvider
		address  string
	}{
		{
			"AWS, with a token",
			map[string]answer{
				"PUT /latest/api/token":             {body: "tok", wants: map[string]string{"X-Aws-Ec2-Metadata-Token-Ttl-Seconds": "60"}},
				"GET /latest/meta-data/public-ipv4": {body: "198.51.100.4\n", wants: map[string]string{"X-Aws-Ec2-Metadata-Token": "tok"}},
			},
			dockwellv1.CloudProvider_CLOUD_PROVIDER_AWS, "198.51.100.4",
		},
		{
			"AWS, with no public address",
			map[string]answer{"PUT /latest/api/token": {body: "tok"}},
			dockwellv1.CloudProvider_CLOUD_PROVIDER_AWS, "",
		},
		{
			"GCP",
			map[string]answer{
				"GET /computeMetadata/v1/instance/id":                                                {body: "123", wants: google, header: google},
				"GET /computeMetadata/v1/instance/network-interfaces/0/access-configs/0/external-ip": {body: "198.51.100.5", wants: google, header: google},
			},
			dockwellv1.CloudProvider_CLOUD_PROVIDER_GCP, "198.51.100.5",
		},
		{
			"Azure",
			map[string]answer{
				"GET /metadata/instance/compute/vmId?api-version=2021-02-01&format=text":                                         {body: "vm", wants: azure},
				"GET /metadata/instance/network/interface/0/ipv4/ipAddress/0/publicIpAddress?api-version=2021-02-01&format=text": {body: "198.51.100.6", wants: azure},
			},
			dockwellv1.CloudProvider_CLOUD_PROVIDER_AZURE, "198.51.100.6",
		},
		{
			"DigitalOcean",
			map[string]answer{
				"GET /metadata/v1/id":                               {body: "42"},
				"GET /metadata/v1/interfaces/public/0/ipv4/address": {body: "198.51.100.7"},
			},
			dockwellv1.CloudProvider_CLOUD_PROVIDER_DIGITALOCEAN, "198.51.100.7",
		},
		{
			"Hetzner",
			map[string]answer{
				"GET /hetzner/v1/metadata/instance-id": {body: "42"},
				"GET /hetzner/v1/metadata/public-ipv4": {body: "198.51.100.8"},
			},
			dockwellv1.CloudProvider_CLOUD_PROVIDER_HETZNER, "198.51.100.8",
		},
		{
			"Oracle, which says who it is and gives no address",
			map[string]answer{
				"GET /opc/v2/instance/id": {body: "ocid1.instance.oc1", wants: map[string]string{"Authorization": "Bearer Oracle"}},
			},
			dockwellv1.CloudProvider_CLOUD_PROVIDER_ORACLE, "",
		},
		{
			"a private address given as public is dropped",
			map[string]answer{
				"GET /hetzner/v1/metadata/instance-id": {body: "42"},
				"GET /hetzner/v1/metadata/public-ipv4": {body: "10.0.0.5"},
			},
			dockwellv1.CloudProvider_CLOUD_PROVIDER_HETZNER, "",
		},
		{
			"a server that answers without Google's header isn't Google",
			map[string]answer{
				"GET /computeMetadata/v1/instance/id": {body: "123"},
			},
			dockwellv1.CloudProvider_CLOUD_PROVIDER_UNSPECIFIED, "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := privateServer(metadataStandIn(t, tc.routes)).Network(t.Context(), false)
			if got.GetProvider() != tc.provider || got.GetMetadataAddress() != tc.address {
				t.Errorf("read %v, want %v at %q", got, tc.provider, tc.address)
			}
		})
	}
}

// Nothing listens there, as on a server on no cloud, so it says nothing, in time.
func TestNoMetadataServiceSaysNothing(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	url := gone.URL
	gone.Close()
	start := time.Now()
	got := privateServer(url).Network(t.Context(), false)
	if got.GetProvider() != dockwellv1.CloudProvider_CLOUD_PROVIDER_UNSPECIFIED || got.GetMetadataAddress() != "" {
		t.Errorf("read %v", got)
	}
	if took := time.Since(start); took > metadataWait+time.Second {
		t.Errorf("took %s", took)
	}
}

func TestALookupIsOnlyMadeWhenItsWantedAndNeeded(t *testing.T) {
	asked := make(chan struct{}, 4)
	server := stunResponder(t, func(request []byte) []byte {
		asked <- struct{}{}
		return xorMapped(request, netip.MustParseAddr("203.0.113.7"))
	})
	metadata := metadataStandIn(t, nil)

	c := privateServer(metadata)
	c.STUN = []string{server}
	if got := c.Network(t.Context(), true).GetLookedUpAddress(); got != "203.0.113.7" {
		t.Errorf("a server behind NAT found %q", got)
	}
	<-asked

	if got := c.Network(t.Context(), false).GetLookedUpAddress(); got != "" {
		t.Errorf("turned off, it found %q", got)
	}
	public := privateServer(metadata)
	public.STUN = []string{server}
	public.Addresses = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("203.0.113.9"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	if got := public.Network(t.Context(), true).GetLookedUpAddress(); got != "" {
		t.Errorf("a server with a public address of its own found %q", got)
	}
	select {
	case <-asked:
		t.Error("it asked when it had no need to")
	case <-time.After(50 * time.Millisecond):
	}
}

// A test that reached a real STUN server or metadata service would pass or fail by
// where it ran, so one that hasn't said where they are stops right there.
func TestATestCantReachARealServiceByLeavingItUnset(t *testing.T) {
	for _, tc := range []struct {
		name   string
		c      *Collector
		lookup bool
		stops  bool
	}{
		{"no metadata", &Collector{STUN: []string{}}, false, true},
		{"no STUN, for a lookup", &Collector{Metadata: ClosedMetadata}, true, true},
		{"no STUN, with no lookup", &Collector{Metadata: ClosedMetadata}, false, false},
		{"both", &Collector{Metadata: ClosedMetadata, STUN: []string{}}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if stopped := recover() != nil; stopped != tc.stops {
					t.Errorf("stopped: %t", stopped)
				}
			}()
			tc.c.Addresses = func() ([]net.Addr, error) { return nil, nil }
			tc.c.Root = fstest.MapFS{}
			tc.c.Network(t.Context(), tc.lookup)
		})
	}
}

// Rule 2 of ADR 0034: anything on a local network can answer at the metadata address,
// but it can't write what the machine's firmware says.
func TestTheMachineSaysWhatItRunsOn(t *testing.T) {
	dmi := func(files map[string]string) fstest.MapFS {
		root := fstest.MapFS{}
		for name, body := range files {
			root["sys/class/dmi/id/"+name] = &fstest.MapFile{Data: []byte(body + "\n")}
		}
		return root
	}
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  dockwellv1.CloudProvider
	}{
		{"AWS", map[string]string{"sys_vendor": "Amazon EC2"}, dockwellv1.CloudProvider_CLOUD_PROVIDER_AWS},
		{"AWS's older machines", map[string]string{"sys_vendor": "Xen", "bios_version": "4.11.amazon"}, dockwellv1.CloudProvider_CLOUD_PROVIDER_AWS},
		{"GCP", map[string]string{"sys_vendor": "Google", "product_name": "Google Compute Engine"}, dockwellv1.CloudProvider_CLOUD_PROVIDER_GCP},
		{"Azure", map[string]string{"sys_vendor": "Microsoft Corporation", "chassis_asset_tag": "7783-7084-3265-9085-8269-3286-77"}, dockwellv1.CloudProvider_CLOUD_PROVIDER_AZURE},
		{"Hyper-V on someone's desk", map[string]string{"sys_vendor": "Microsoft Corporation", "chassis_asset_tag": "1234"}, dockwellv1.CloudProvider_CLOUD_PROVIDER_UNSPECIFIED},
		{"DigitalOcean", map[string]string{"sys_vendor": "DigitalOcean"}, dockwellv1.CloudProvider_CLOUD_PROVIDER_DIGITALOCEAN},
		{"Hetzner", map[string]string{"sys_vendor": "Hetzner"}, dockwellv1.CloudProvider_CLOUD_PROVIDER_HETZNER},
		{"Oracle", map[string]string{"chassis_asset_tag": "OracleCloud.com"}, dockwellv1.CloudProvider_CLOUD_PROVIDER_ORACLE},
		{"a machine at home", map[string]string{"sys_vendor": "ASUSTeK COMPUTER INC."}, dockwellv1.CloudProvider_CLOUD_PROVIDER_UNSPECIFIED},
		{"no tables at all, as in a container", nil, dockwellv1.CloudProvider_CLOUD_PROVIDER_UNSPECIFIED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := privateServer(ClosedMetadata)
			c.Root = dmi(tc.files)
			if got := c.Network(t.Context(), false).GetMachineProvider(); got != tc.want {
				t.Errorf("it says %v", got)
			}
		})
	}
}

// A redirect is no service's answer: what stands in for one on a local network could
// send the request wherever it liked.
func TestAMetadataRedirectIsntFollowed(t *testing.T) {
	var followed atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Store(true) }))
	defer elsewhere.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer redirect.Close()
	c := privateServer(redirect.URL)
	if got := c.Network(t.Context(), false).GetProvider(); got != dockwellv1.CloudProvider_CLOUD_PROVIDER_UNSPECIFIED {
		t.Errorf("it says %v", got)
	}
	if followed.Load() {
		t.Error("it followed the redirect")
	}
}
