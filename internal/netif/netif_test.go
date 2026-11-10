// SPDX-License-Identifier: AGPL-3.0-only

package netif

import (
	"testing"
	"testing/fstest"
)

// sysfs is /sys/class/net as a server with every kind of interface has it.
func sysfs() fstest.MapFS {
	dev := &fstest.MapFile{Data: []byte("")}
	link := &fstest.MapFile{Data: []byte("")}
	return fstest.MapFS{
		"eth0/device":         dev,
		"enp3s0/device":       dev,
		"eth1/device":         dev,
		"eth2/device":         dev,
		"ens10/device":        dev, // a cloud's second NIC, its private network
		"br-lan/lower_enp3s0": link,
		"br0/lower_eth0":      link,
		"bond0/lower_eth1":    link,
		"bond0/lower_eth2":    link,
		"eth0.100/lower_eth0": link,
		"macvlan0/lower_eth0": link,
		"docker0/lower_veth1": link,
		"veth1/type":          {Data: []byte("1\n")},
		"mynet/lower_veth2":   link, // a Docker network named with com.docker.network.bridge.name
		"veth2/type":          {Data: []byte("1\n")},
		"podman0/type":        {Data: []byte("1\n")},
		"lxdbr0/type":         {Data: []byte("1\n")},
		"incusbr0/type":       {Data: []byte("1\n")},
		"kube-bridge/type":    {Data: []byte("1\n")},
		"cilium_host/type":    {Data: []byte("1\n")},
		"wg0/type":            {Data: []byte("65534\n")},
		"zt3jnj2kwr/type":     {Data: []byte("1\n")},
		"nebula1/type":        {Data: []byte("65534\n")},
		"Meta/tun_flags":      {Data: []byte("0x1001\n")},
		"Meta/type":           {Data: []byte("65534\n")},
		"ppp0/type":           {Data: []byte("512\n")},
		"loopy/lower_loopier": link,
		"loopier/lower_loopy": link,
		"vbridge/lower_tap0":  link,
		"tap0/tun_flags":      {Data: []byte("0x1002\n")},
	}
}

// What's physical is what sysfs says, whatever it's called: a NIC, or what's built
// on one; a PPP link; and never a bridge of veths or taps, a VPN or a TUN.
func TestPhysicalIsWhatSysfsSays(t *testing.T) {
	sys := sysfs()
	for name, want := range map[string]bool{
		"eth0": true, "ens10": true, "br-lan": true, "br0": true, "bond0": true, "eth0.100": true, "macvlan0": true, "ppp0": true,
		"docker0": false, "mynet": false, "veth1": false, "podman0": false, "lxdbr0": false, "incusbr0": false,
		"kube-bridge": false, "cilium_host": false, "wg0": false, "zt3jnj2kwr": false, "nebula1": false, "Meta": false,
		"loopy": false, "vbridge": false, "lo": false, "../eth0": false,
	} {
		if got := Physical(sys, name); got != want {
			t.Errorf("Physical(%q) = %v, want %v", name, got, want)
		}
	}
}

// Where no interface has a device, as in a container, sysfs can't say, and the
// caller goes by name.
func TestPhysicalsSaysWhenSysfsCant(t *testing.T) {
	got, known := Physicals(sysfs(), []string{"eth0", "docker0", "wg0"})
	if !known || !got["eth0"] || got["docker0"] || got["wg0"] {
		t.Errorf("physical %v, known %v", got, known)
	}
	container := fstest.MapFS{"eth0/type": {Data: []byte("1\n")}}
	if _, known := Physicals(container, []string{"eth0", "lo"}); known {
		t.Error("in a container, with no device anywhere, it says it knows")
	}
	for name, want := range map[string]bool{"eth0": false, "docker0": true, "br-3f2a": true, "dw-ingress": true, "podman0": true, "wg0": true, "lo": true} {
		if got := Virtual(name); got != want {
			t.Errorf("Virtual(%q) = %v, want %v", name, got, want)
		}
	}
}
