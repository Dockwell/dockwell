// SPDX-License-Identifier: AGPL-3.0-only

package alert

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/netguard"
	"github.com/dockwell/dockwell/internal/resource"
)

// CheckKind is an address Dockwell asks whether it answers (ADR 0066).
const CheckKind resource.Kind = "alert.check"

// What a check takes, and what it gets unless it says.
const (
	// DefaultEvery is how often a check runs unless it says.
	DefaultEvery = time.Minute
	// DefaultCheckTimeout is how long a run waits for an answer unless it says.
	DefaultCheckTimeout = 10 * time.Second
	// MaxCheckTimeout is the longest a run waits.
	MaxCheckTimeout = 30 * time.Second
	// DefaultDownAfter is how many failed runs in a row make a check down unless it says.
	DefaultDownAfter = 2
	// MaxDownAfter is the most failed runs a check may wait for.
	MaxDownAfter = 10
	// MaxKeyword is the longest keyword a check looks for.
	MaxKeyword = 200
	// MaxChecks is the most checks an install keeps: past about 100 checks a
	// minute that all wait out their timeouts, the control plane's 16 runs at
	// once fall behind (ADR 0066).
	MaxChecks = 200
)

// Everies are how often a check may run.
var Everies = []time.Duration{30 * time.Second, time.Minute, 5 * time.Minute, 15 * time.Minute}

// Check is the alert check kind.
type Check struct{}

var (
	_ resource.Type     = Check{}
	_ resource.Unplaced = Check{}
)

// Kind returns "alert.check".
func (Check) Kind() resource.Kind { return CheckKind }

// Level is the organization: a check is the install's, as rules are.
func (Check) Level() resource.Level { return resource.LevelOrganization }

// IDPrefix is "chk".
func (Check) IDPrefix() string { return id.AlertCheck }

// New returns an empty AlertCheck.
func (Check) New() proto.Message { return &resourcev1.AlertCheck{} }

// Unplaced says the control plane runs it.
func (Check) Unplaced() {}

// CheckName takes what fits in a URL and on a command line.
func (Check) CheckName(name string) error {
	return resource.CheckLabel("check", name, "shop-example-com")
}

// Validate refuses a check that can't run, and fills in the defaults, so checks
// that mean the same compare equal.
func (Check) Validate(spec proto.Message) error {
	c := spec.(*resourcev1.AlertCheck) //nolint:forcetypeassert // the registry gives this kind's message
	u, err := url.Parse(strings.TrimSpace(c.GetAddress()))
	switch {
	case c.GetAddress() == "":
		return errors.New("a check needs the address it asks, such as https://shop.example.com/")
	case err != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("a check's address is an http or https URL, such as https://shop.example.com/, not %q", c.GetAddress())
	case u.User != nil:
		return errors.New("a check's address can't hold a user and password: a check sends nothing that signs in")
	}
	c.Address = u.String()
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("a check's address has a port from 1 to 65535, not %s", p)
		}
	}
	if h := u.Hostname(); numeric.MatchString(h) {
		if _, err := netip.ParseAddr(h); err != nil {
			// A resolver may read 2130706433 or 0x7f.1 as an address; a check
			// never asks one that way, so it would never run.
			return fmt.Errorf("%s isn't an address a check can ask: write an address as four numbers, such as 203.0.113.10, or a domain", h)
		}
	}
	if err := reachable(u.Host, c.GetInsideNetwork()); err != nil {
		return err
	}
	every, err := ParseDuration("every", c.GetEvery())
	if err != nil {
		return errors.New("a check runs every 30s, 1m, 5m or 15m")
	}
	if every == 0 {
		every = DefaultEvery
	}
	known := false
	for _, e := range Everies {
		known = known || e == every
	}
	if !known {
		return errors.New("a check runs every 30s, 1m, 5m or 15m")
	}
	c.Every = FormatDuration(every)
	timeout, err := ParseDuration("timeout", c.GetTimeout())
	if err != nil {
		return errors.New("a check's timeout is a duration, such as 10s")
	}
	if timeout == 0 {
		timeout = DefaultCheckTimeout
	}
	if timeout > MaxCheckTimeout || timeout >= every {
		return fmt.Errorf("a check waits %s at most, and less than how often it runs", FormatDuration(MaxCheckTimeout))
	}
	c.Timeout = FormatDuration(timeout)
	if c.DownAfter == 0 {
		c.DownAfter = DefaultDownAfter
	}
	if c.GetDownAfter() > MaxDownAfter {
		return fmt.Errorf("a check reads down after 1 to %d failed runs in a row", MaxDownAfter)
	}
	if c.ExpectFrom == 0 {
		c.ExpectFrom = 200
	}
	if c.ExpectTo == 0 {
		c.ExpectTo = 399
	}
	if c.GetExpectFrom() < 100 || c.GetExpectTo() > 599 || c.GetExpectFrom() > c.GetExpectTo() {
		return errors.New("a check's statuses that count as up run from 100 to 599, lowest first, such as 200 to 399")
	}
	c.Keyword = strings.TrimSpace(c.GetKeyword())
	if len(c.GetKeyword()) > MaxKeyword {
		return fmt.Errorf("a check's keyword is %d bytes at most", MaxKeyword)
	}
	if c.GetKeyword() == "" && c.GetKeywordAbsent() {
		return errors.New("a check that wants a keyword absent needs the keyword")
	}
	return nil
}

// Cap is the most checks an install keeps (resource.Capped).
func (Check) Cap() int { return MaxChecks }

// CapWords says why another check is refused.
func (Check) CapWords(have int) string {
	return fmt.Sprintf("the install has %d checks, and keeps %d at most, as each one is a request every interval. Remove a check you no longer need, then add this one", have, MaxChecks)
}

// numeric is a host of numbers only, in any base a resolver may read.
var numeric = regexp.MustCompile(`(?i)^(0x[0-9a-f]*|\d+)(\.(0x[0-9a-f]*|\d+))*\.?$`)

// reachable refuses, as the plan is made, an address a check can never reach
// from the control plane, written out as one: this server's loopback, a metadata
// service, a link-local address; and a private one, unless the check says it's
// inside the network. A name is judged as it's dialed, each run.
func reachable(host string, inside bool) error {
	why := netguard.Forbidden(host, netguard.Options{Private: inside})
	switch {
	case why == "":
		return nil
	case !inside && netguard.Forbidden(host, netguard.Options{Private: true}) == "":
		return fmt.Errorf("%s is %s, so a check reaches it only from inside the network. Set inside_network on the check (Inside the network, under Change) to check it there", host, why)
	case why == netguard.Forbidden("127.0.0.1", netguard.Options{Private: true}):
		return fmt.Errorf("%s is %s, which a check from the control plane never reaches. Check the address people reach it at, such as its domain", host, why)
	}
	return fmt.Errorf("%s is %s, which a check never reaches", host, why)
}

// Every is how often a validated check runs.
func Every(c *resourcev1.AlertCheck) time.Duration {
	d, _ := ParseDuration("every", c.GetEvery())
	if d == 0 {
		return DefaultEvery
	}
	return d
}

// Timeout is how long a validated check's run waits.
func Timeout(c *resourcev1.AlertCheck) time.Duration {
	d, _ := ParseDuration("timeout", c.GetTimeout())
	if d == 0 {
		return DefaultCheckTimeout
	}
	return d
}
