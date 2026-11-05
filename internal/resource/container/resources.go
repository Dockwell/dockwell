// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
)

// A spec holds its limits as people write them, in one form for each amount, so
// a plan shows "512M" rather than 536870912 and specs that mean the same compare
// equal. Each is checked here rather than where Docker would refuse it, which is
// only once a change is applied.
const (
	// minMemory is the least memory Docker takes as a limit or a reservation.
	minMemory = 6 << 20
	// minCPUMillis is the least of a CPU Docker's scheduler can hold a container
	// to, in thousandths.
	minCPUMillis = 10
	// nanosPerMilli are billionths of a CPU, as Docker takes them, in a
	// thousandth of one.
	nanosPerMilli = 1_000_000
	// maxCPUMillis and maxMemory are far past any server, and keep what they
	// come to within an int64.
	maxCPUMillis = 1 << 40
	maxMemory    = 1 << 60
)

// CanonicalCPUs reads a number of CPUs, such as "0.50", in the one form a spec
// keeps, "0.5". Nothing, or zero, is no limit, and comes back empty.
func CanonicalCPUs(s string) (string, error) {
	millis, err := parseCPUs(s)
	if err != nil {
		return "", err
	}
	return formatCPUs(millis), nil
}

func parseCPUs(s string) (int64, error) {
	bad := func() (int64, error) {
		return 0, fmt.Errorf("%q isn't a number of CPUs: give one such as 0.5 or 2, to three decimal places at most", s)
	}
	if s == "" {
		return 0, nil
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" && !hasFrac || hasFrac && frac == "" || len(frac) > 3 || !digits(whole) || !digits(frac) || len(whole) > 12 {
		return bad()
	}
	var w int64
	if whole != "" {
		var err error
		if w, err = strconv.ParseInt(whole, 10, 64); err != nil {
			return bad()
		}
	}
	f, _ := strconv.ParseInt((frac + "000")[:3], 10, 64)
	millis := w*1000 + f
	switch {
	case millis == 0:
		return 0, nil
	case millis < minCPUMillis:
		return 0, fmt.Errorf("%s of a CPU is less than Docker can hold a container to. Give at least 0.01, or leave the limit out", s)
	case millis > maxCPUMillis:
		return bad()
	}
	return millis, nil
}

func digits(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

func formatCPUs(millis int64) string {
	if millis == 0 {
		return ""
	}
	s := strconv.FormatInt(millis/1000, 10)
	if f := millis % 1000; f > 0 {
		s += "." + strings.TrimRight(fmt.Sprintf("%03d", f), "0")
	}
	return s
}

// cpuNanos is a spec's CPUs as Docker takes them. The spec is valid.
func cpuNanos(s string) int64 {
	millis, _ := parseCPUs(s)
	return millis * nanosPerMilli
}

// memoryUnits are what a memory limit may come in, from the largest, in the
// units of 1024 Compose and Docker read.
var memoryUnits = []struct {
	suffix string
	bytes  int64
}{{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}}

// CanonicalMemory reads an amount of memory, such as "1024m", in the one form a
// spec keeps, "1G". Nothing, or zero, is no limit, and comes back empty. It takes
// what Compose takes: a number of bytes, or one with a unit of k, m, g or t, with
// an optional b or ib.
func CanonicalMemory(s string) (string, error) {
	n, err := parseMemory(s)
	if err != nil {
		return "", err
	}
	return FormatMemory(n), nil
}

func parseMemory(s string) (int64, error) {
	bad := func() (int64, error) {
		return 0, fmt.Errorf("%q isn't an amount of memory: give one such as 512M or 2G", s)
	}
	if s == "" {
		return 0, nil
	}
	// An amount is written in ASCII. Past it, a letter's lowercase can be longer or
	// shorter than it, so the unit would be cut from the wrong place.
	if strings.IndexFunc(s, func(r rune) bool { return r > unicode.MaxASCII }) >= 0 {
		return bad()
	}
	lower := strings.ToLower(s)
	number := strings.TrimRight(lower, "kmgtib ")
	unit := strings.TrimSpace(lower[len(number):])
	unit = strings.TrimSuffix(strings.TrimSuffix(unit, "b"), "i")
	scale := int64(1)
	switch unit {
	case "":
	case "k":
		scale = 1 << 10
	case "m":
		scale = 1 << 20
	case "g":
		scale = 1 << 30
	case "t":
		scale = 1 << 40
	default:
		return bad()
	}
	whole, frac, hasFrac := strings.Cut(number, ".")
	if whole == "" || hasFrac && frac == "" || !digits(whole) || !digits(frac) || len(whole) > 19 || len(frac) > 9 {
		return bad()
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > maxMemory/scale {
		return bad()
	}
	total := w * scale
	if frac != "" {
		f, _ := strconv.ParseInt(frac, 10, 64)
		div := int64(1)
		for range frac {
			div *= 10
		}
		// Whole digits times a unit first: it is exact, as a float isn't.
		total += f * (scale / div)
		total += f * (scale % div) / div
	}
	return total, nil
}

// FormatMemory writes bytes in the largest unit that holds them exactly, such as
// "1536M". Zero is empty.
func FormatMemory(n int64) string {
	if n <= 0 {
		return ""
	}
	for _, u := range memoryUnits {
		if n%u.bytes == 0 {
			return strconv.FormatInt(n/u.bytes, 10) + u.suffix
		}
	}
	return strconv.FormatInt(n, 10)
}

// memoryBytes is a spec's memory as Docker takes it. The spec is valid.
func memoryBytes(s string) int64 {
	n, _ := parseMemory(s)
	return n
}

// validateResources refuses limits Docker would, and puts them in their one
// form. A spec with none has no Resources, so it compares equal to one that
// never had any.
func validateResources(c *resourcev1.Container) error {
	r := c.GetResources()
	if r == nil {
		return nil
	}
	var err error
	if r.Cpus, err = CanonicalCPUs(r.GetCpus()); err != nil {
		return err
	}
	limit, err := parseMemory(r.GetMemory())
	if err != nil {
		return err
	}
	reserve, err := parseMemory(r.GetMemoryReservation())
	if err != nil {
		return err
	}
	switch {
	case limit != 0 && limit < minMemory:
		return fmt.Errorf("a memory limit of %s is less than Docker takes. Give at least 6M, or leave it out", r.GetMemory())
	case reserve != 0 && reserve < minMemory:
		return fmt.Errorf("a memory reservation of %s is less than Docker takes. Give at least 6M, or leave it out", r.GetMemoryReservation())
	case limit != 0 && reserve > limit:
		return fmt.Errorf("the memory reservation of %s is more than the limit of %s. Reserve no more than the limit", r.GetMemoryReservation(), r.GetMemory())
	}
	r.Memory, r.MemoryReservation = FormatMemory(limit), FormatMemory(reserve)
	if r.GetCpus() == "" && r.GetMemory() == "" && r.GetMemoryReservation() == "" {
		c.Resources = nil
	}
	return nil
}

// projectResources reads a container's limits back. Compose reads a number of
// CPUs through a float of 32 bits, so what it asks Docker for can be a little
// off the thousandths a spec holds: to the nearest one, that doesn't read as
// drift.
func projectResources(h *docker.HostConfig) *resourcev1.Resources {
	if h.NanoCPUs == 0 && h.Memory == 0 && h.MemoryReservation == 0 {
		return nil
	}
	return &resourcev1.Resources{
		Cpus:              formatCPUs((h.NanoCPUs + nanosPerMilli/2) / nanosPerMilli),
		Memory:            FormatMemory(h.Memory),
		MemoryReservation: FormatMemory(h.MemoryReservation),
	}
}

// checkCPUs refuses more CPUs than this server has, which Docker refuses only
// when it makes the container, when the old one may be about to go. Control
// planes can't check it: it depends on the server.
func (d *Docker) checkCPUs(ctx context.Context, t Target, spec *resourcev1.Container) error {
	millis, err := parseCPUs(spec.GetResources().GetCpus())
	if err != nil || millis == 0 {
		return err
	}
	info, err := d.Client.Info(ctx)
	if err != nil {
		return fmt.Errorf("can't ask Docker how many CPUs this server has: %w", err)
	}
	if info.NCPU > 0 && millis > int64(info.NCPU)*1000 {
		return fmt.Errorf("%s is limited to %s CPUs, and this server has %d, so Docker wouldn't take it. Give %d or fewer, or run it on a larger server",
			t.Service, formatCPUs(millis), info.NCPU, info.NCPU)
	}
	return nil
}
