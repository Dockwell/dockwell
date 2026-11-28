// SPDX-License-Identifier: AGPL-3.0-only

package probe

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dockwell/dockwell/internal/netguard"
)

const customWait = 5 * time.Second

// StepCustom is a credential-free reachability check.
const StepCustom = "custom"

// Custom is a single HTTPS GET or TCP connect. It holds no connection values.
type Custom struct {
	HTTPS string `json:"https,omitempty"`
	Host  string `json:"host,omitempty"`
	Port  uint32 `json:"port,omitempty"`
}

var customHost = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?\.?$`)

func validCustomHost(host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Zone() == ""
	}
	return len(host) <= 253 && customHost.MatchString(host)
}

// Validate checks the target without making a request or echoing the input.
func (c Custom) Validate() error {
	switch {
	case c.HTTPS != "" && c.Host == "" && c.Port == 0:
		u, err := url.Parse(c.HTTPS)
		if err == nil && u.Scheme == "https" && validCustomHost(u.Hostname()) && u.User == nil &&
			u.RawQuery == "" && !u.ForceQuery && !strings.Contains(c.HTTPS, "#") && !strings.HasSuffix(u.Host, ":") && len(c.HTTPS) <= 2048 {
			if port := u.Port(); port == "" {
				return nil
			} else if n, err := strconv.ParseUint(port, 10, 16); err == nil && n > 0 {
				return nil
			}
		}
		return errors.New("give an HTTPS URL with a host and no credentials, query or fragment")
	case c.HTTPS == "" && validCustomHost(c.Host) && c.Port > 0 && c.Port <= 65535:
		return nil
	default:
		return errors.New("choose one check with an HTTPS URL or a TCP host and port")
	}
}

func checkCustom(ctx context.Context, c Custom, guard netguard.Options) Result {
	ctx, cancel := context.WithTimeout(ctx, customWait)
	defer cancel()
	guard.Timeout = customWait
	host := c.Host
	if c.HTTPS != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.HTTPS, http.NoBody)
		if err != nil {
			return Result{Step: StepCustom, Code: CodeUnreachable}
		}
		host = req.URL.Hostname()
		if netguard.Forbidden(host, guard) != "" {
			return Result{Step: StepCustom, Code: CodeRefused}
		}
		reply, err := netguard.Client(guard).Do(req)
		if err != nil {
			return Result{Step: StepCustom, Code: connectCode(err)}
		}
		_ = reply.Body.Close()
		return Result{Passed: reply.StatusCode >= 200 && reply.StatusCode < 300, Step: StepCustom, Status: reply.StatusCode}
	}
	if netguard.Forbidden(host, guard) != "" {
		return Result{Step: StepCustom, Code: CodeRefused}
	}
	conn, err := netguard.Dialer(guard)(ctx, "tcp", net.JoinHostPort(host, strconv.FormatUint(uint64(c.Port), 10)))
	if err != nil {
		return Result{Step: StepCustom, Code: connectCode(err)}
	}
	_ = conn.Close()
	return Result{Passed: true, Step: StepCustom}
}
