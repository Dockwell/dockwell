// SPDX-License-Identifier: AGPL-3.0-only

package collect

import (
	"context"
	"errors"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// fixture is a small Ubuntu server: 4 CPUs, 8 GiB, one data disk plus
// Docker's overlay and the usual pseudo-filesystems.
func fixture(stat, netdev string) fstest.MapFS {
	return fstest.MapFS{
		"etc/os-release": {Data: []byte(`PRETTY_NAME="Ubuntu 24.04.1 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
ID=ubuntu
`)},
		"proc/sys/kernel/osrelease": {Data: []byte("6.8.0-48-generic\n")},
		"proc/cpuinfo":              {Data: []byte(strings.Repeat("processor\t: 0\nmodel name\t: AMD EPYC 7763 64-Core Processor\n\n", 4))},
		"proc/meminfo":              {Data: []byte("MemTotal:        8388608 kB\nMemFree:          524288 kB\nMemAvailable:    6291456 kB\n")},
		"proc/loadavg":              {Data: []byte("0.52 0.41 0.30 2/345 12345\n")},
		"proc/uptime":               {Data: []byte("86400.42 340000.00\n")},
		"proc/stat":                 {Data: []byte(stat)},
		"proc/net/dev":              {Data: []byte(netdev)},
		"proc/self/mounts": {Data: []byte(`/dev/sda1 / ext4 rw,relatime 0 0
proc /proc proc rw,nosuid 0 0
tmpfs /run tmpfs rw,nosuid 0 0
overlay /var/lib/docker/overlay2/abc/merged overlay rw 0 0
/dev/sda1 /var/lib/docker ext4 rw,relatime 0 0
/dev/sdb1 /mnt/data\040disk xfs rw 0 0
`)},
	}
}

const netHeader = "Inter-|   Receive                            |  Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\n"

func netdev(ethRx, ethTx int) string {
	return netHeader +
		"    lo: 9999 0 0 0 0 0 0 0 9999 0 0 0 0 0 0 0\n" +
		"  eth0: " + strconv.Itoa(ethRx) + " 0 0 0 0 0 0 0 " + strconv.Itoa(ethTx) + " 0 0 0 0 0 0 0\n" +
		"docker0: 5555 0 0 0 0 0 0 0 5555 0 0 0 0 0 0 0\n" +
		"vethab12: 7777 0 0 0 0 0 0 0 7777 0 0 0 0 0 0 0\n"
}

func newFixtureCollector(fsys fstest.MapFS) *Collector {
	return &Collector{
		Root: fsys,
		Statfs: func(path string) (uint64, uint64, error) {
			switch path {
			case "/":
				return 80 << 30, 30 << 30, nil
			case "/mnt/data disk":
				return 500 << 30, 100 << 30, nil
			}
			return 0, 0, errors.New("not mounted")
		},
		Hostname: func() (string, error) { return "hz-1", nil },
		Addresses: func() ([]net.Addr, error) {
			return []net.Addr{&net.IPNet{IP: net.ParseIP("203.0.113.7")}, &net.IPNet{IP: net.IPv4(127, 0, 0, 1)}}, nil
		},
	}
}

func TestInventory(t *testing.T) {
	c := newFixtureCollector(fixture("cpu 1 0 1 8 0 0 0 0 0 0\n", netdev(0, 0)))
	inv := c.Inventory(t.Context())
	if inv.GetOsName() != "Ubuntu 24.04.1 LTS" || inv.GetOsId() != "ubuntu" || inv.GetOsVersion() != "24.04" {
		t.Errorf("OS = %q %q %q", inv.GetOsName(), inv.GetOsId(), inv.GetOsVersion())
	}
	if inv.GetKernel() != "6.8.0-48-generic" || inv.GetCpuCount() != 4 || inv.GetCpuModel() != "AMD EPYC 7763 64-Core Processor" {
		t.Errorf("system = %q, %d × %q", inv.GetKernel(), inv.GetCpuCount(), inv.GetCpuModel())
	}
	if inv.GetMemoryBytes() != 8<<30 || inv.GetHostname() != "hz-1" || inv.GetArchitecture() != runtime.GOARCH {
		t.Errorf("memory %d, host %q, arch %q", inv.GetMemoryBytes(), inv.GetHostname(), inv.GetArchitecture())
	}
	// Two data disks, largest first; the bind mount, overlay and pseudo
	// filesystems are left out, and the escaped space is decoded.
	if len(inv.GetDisks()) != 2 || inv.GetDisks()[0].GetMountPoint() != "/mnt/data disk" || inv.GetDisks()[1].GetMountPoint() != "/" {
		t.Errorf("disks = %v", inv.GetDisks())
	}
	if strings.Join(inv.GetAddresses(), ",") != "203.0.113.7" {
		t.Errorf("addresses = %v", inv.GetAddresses())
	}
	if inv.GetDockerVersion() != "" {
		t.Errorf("Docker version without a socket = %q", inv.GetDockerVersion())
	}
}

func TestStatsRates(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 21, 40, 0, 0, time.UTC)
	fsys := fixture("cpu 100 0 100 800 0 0 0 0 0 0\n", netdev(1000, 2000))
	c := newFixtureCollector(fsys)

	first := c.Stats(t0)
	if first.GetCpuPercent() != 0 || first.GetNetworkRxBytesPerSecond() != 0 {
		t.Error("the first sample has nothing to compare with and should report 0 rates")
	}
	if first.GetMemoryTotalBytes() != 8<<30 || first.GetMemoryUsedBytes() != 2<<30 {
		t.Errorf("memory = %d of %d", first.GetMemoryUsedBytes(), first.GetMemoryTotalBytes())
	}
	if first.GetLoad1() != 0.52 || first.GetLoad15() != 0.30 || first.GetUptime().AsDuration().Round(time.Second) != 24*time.Hour {
		t.Errorf("load %v/%v, uptime %v", first.GetLoad1(), first.GetLoad15(), first.GetUptime().AsDuration())
	}
	if len(first.GetDisks()) != 2 {
		t.Errorf("disk usage = %v", first.GetDisks())
	}

	// 5 s later: 300 of 1000 new jiffies were busy, and eth0 moved 5 KB in, 10 KB out.
	fsys["proc/stat"].Data = []byte("cpu 300 0 200 1500 0 0 0 0 0 0\n")
	fsys["proc/net/dev"].Data = []byte(netdev(6000, 12000))
	second := c.Stats(t0.Add(5 * time.Second))
	if second.GetCpuPercent() != 30 {
		t.Errorf("CPU = %v%%, want 30%%", second.GetCpuPercent())
	}
	if second.GetNetworkRxBytesPerSecond() != 1000 || second.GetNetworkTxBytesPerSecond() != 2000 {
		t.Errorf("network = %d in, %d out per second", second.GetNetworkRxBytesPerSecond(), second.GetNetworkTxBytesPerSecond())
	}

	// A counter that goes backwards (an interface was reset, or the kernel's
	// CPU accounting wobbled) reports 0, not a wrapped huge number.
	fsys["proc/stat"].Data = []byte("cpu 290 0 200 1600 0 0 0 0 0 0\n")
	fsys["proc/net/dev"].Data = []byte(netdev(10, 10))
	third := c.Stats(t0.Add(10 * time.Second))
	if third.GetNetworkRxBytesPerSecond() != 0 {
		t.Errorf("after a reset, rx = %d", third.GetNetworkRxBytesPerSecond())
	}
	if third.GetCpuPercent() != 0 {
		t.Errorf("after busy time went backwards, CPU = %v%%", third.GetCpuPercent())
	}
}

func TestMissingFilesAreHarmless(t *testing.T) {
	c := &Collector{
		Root:      fstest.MapFS{},
		Statfs:    func(string) (uint64, uint64, error) { return 0, 0, errors.New("nope") },
		Hostname:  func() (string, error) { return "", errors.New("nope") },
		Addresses: func() ([]net.Addr, error) { return nil, errors.New("nope") },
	}
	inv := c.Inventory(context.Background())
	if inv.GetCpuCount() == 0 {
		t.Error("CPU count should fall back to the runtime's")
	}
	_ = c.Stats(time.Now())
}

func TestParsers(t *testing.T) {
	kv := parseKeyValues([]byte("# comment\nNAME='Debian GNU/Linux'\nID=debian\nBAD LINE\n"))
	if kv["NAME"] != "Debian GNU/Linux" || kv["ID"] != "debian" || len(kv) != 2 {
		t.Errorf("parseKeyValues = %v", kv)
	}
	if _, _, ok := cpuTimes([]byte("cpu0 1 2 3 4\n")); ok {
		t.Error("cpuTimes must use the aggregate cpu line only")
	}
}

func TestOnThisMachine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the collector reads Linux's /proc")
	}
	c := New()
	inv := c.Inventory(t.Context())
	if inv.GetCpuCount() == 0 || inv.GetMemoryBytes() == 0 || inv.GetKernel() == "" {
		t.Errorf("inventory of this machine looks empty: %v", inv)
	}
	_ = c.Stats(time.Now())
	time.Sleep(50 * time.Millisecond)
	st := c.Stats(time.Now())
	if st.GetMemoryTotalBytes() == 0 || st.GetUptime().AsDuration() <= 0 || st.GetCpuPercent() < 0 || st.GetCpuPercent() > 100 {
		t.Errorf("stats of this machine look wrong: %v", st)
	}
}
