// SPDX-License-Identifier: AGPL-3.0-only

// Package dockerdaemon manages an allowlisted overlay of local Engine settings.
package dockerdaemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	spec "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/dockerlog"
	"github.com/dockwell/dockwell/internal/resource/network"
)

var errPools = errors.New("choose canonical IPv4 address pools with enough room for a gateway and containers")

func validMirror(s string) bool {
	u, err := url.Parse(s)
	return err == nil && len(s) <= 512 && u.Scheme == "https" && u.Hostname() != "" && u.User == nil &&
		!u.ForceQuery && u.RawQuery == "" && !strings.Contains(s, "#") && u.Opaque == "" && !strings.ContainsAny(s, "\r\n\t ")
}

func validate(d *spec.DockerDaemon) error {
	if d.GetReviewCurrentFile() {
		if d.GetMirrors() != nil || d.GetLogging() != nil || d.GetAddressPools() != nil {
			return errors.New("review the current Docker file separately from a settings draft")
		}
		return nil
	}
	if d == nil || d.GetMirrors() == nil && d.GetLogging() == nil && d.GetAddressPools() == nil {
		return errors.New("choose at least one Docker setting")
	}
	if m := d.GetMirrors(); m != nil {
		if len(m.Urls) > 16 {
			return errors.New("a server takes at most 16 registry mirrors")
		}
		seen := map[string]bool{}
		for i, s := range m.Urls {
			if !validMirror(s) {
				return fmt.Errorf("mirror %d needs an HTTPS URL without credentials, query or fragment", i+1)
			}
			if seen[s] {
				return fmt.Errorf("mirror %d is listed twice", i+1)
			}
			seen[s] = true
		}
	}
	if d.GetLogging() != nil {
		if err := dockerlog.ValidateLogging(d.GetLogging()); err != nil {
			return err
		}
	}
	if list := d.GetAddressPools(); list != nil {
		if len(list.Pools) == 0 || len(list.Pools) > 16 {
			return errors.New("enter between 1 and 16 default address pools")
		}
		var seen []netip.Prefix
		for _, pool := range list.Pools {
			base, err := netip.ParsePrefix(pool.GetBase())
			if err != nil || !base.Addr().Is4() || base.Masked() != base || pool.GetPrefixLength() > 30 ||
				base.Bits() > int(pool.GetPrefixLength()) || base.Bits() < 8 || network.LinkRange.Overlaps(base) ||
				base.Addr().IsLoopback() || base.Addr().IsMulticast() || base.Addr().IsLinkLocalUnicast() || base.Addr().IsUnspecified() ||
				slices.ContainsFunc(seen, base.Overlaps) {
				return errPools
			}
			seen = append(seen, base)
		}
	}
	return nil
}

type poolEntry struct {
	Base string `json:"base"`
	Size uint32 `json:"size"`
}

type config map[string]json.RawMessage

// overlay returns the groups being changed and leaves all other JSON values opaque.
func overlay(current config, after *spec.DockerDaemon) (config, error) {
	out := make(config, len(current)+4)
	for k, value := range current {
		out[k] = value
	}
	if after == nil {
		return out, nil
	}
	put := func(key string, value any) {
		out[key], _ = json.Marshal(value)
	}
	if m := after.GetMirrors(); m != nil {
		if raw := current["registry-mirrors"]; raw != nil {
			var held []string
			if json.Unmarshal(raw, &held) != nil || slices.ContainsFunc(held, func(url string) bool { return !validMirror(url) }) {
				return nil, errors.New("the existing mirror list contains settings this editor cannot represent")
			}
		}
		put("registry-mirrors", append([]string{}, m.GetUrls()...))
	}
	if l := after.GetLogging(); l != nil {
		var oldDriver string
		_ = json.Unmarshal(out["log-driver"], &oldDriver)
		if oldDriver == "" {
			oldDriver = "json-file"
		}
		opts := make(map[string]string)
		if raw, ok := out["log-opts"]; ok {
			if err := json.Unmarshal(raw, &opts); err != nil || opts == nil {
				return nil, errors.New("the daemon has unsupported logging options")
			}
		}
		if oldDriver != "json-file" && oldDriver != "local" {
			return nil, errors.New("the active log driver is managed outside this editor")
		}
		if oldDriver != l.GetDriver() {
			for k := range opts {
				if k != "max-file" && k != "max-size" {
					return nil, errors.New("the current log driver has options that cannot be carried to the new driver")
				}
			}
		}
		opts["max-size"], opts["max-file"] = strconv.FormatUint(l.GetMaxSizeBytes(), 10), strconv.FormatUint(uint64(l.GetMaxFiles()), 10)
		put("log-driver", l.GetDriver())
		put("log-opts", opts)
	}
	if pools := after.GetAddressPools(); pools != nil {
		next := make([]poolEntry, 0, len(pools.Pools))
		for _, p := range pools.Pools {
			next = append(next, poolEntry{Base: p.Base, Size: p.PrefixLength})
		}
		put("default-address-pools", next)
	}
	return out, nil
}
