// SPDX-License-Identifier: AGPL-3.0-only

package agentapi

import (
	"context"
	"fmt"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

const (
	validateCaddyTimeout = 30 * time.Second
	certActionTimeout    = 45 * time.Second
)

// WebsiteHandy says whether this server's agent supports Caddyfile adapter validation,
// certificate renewal/download, and website folder backups.
func (s *Session) WebsiteHandy() bool { return s.websiteHandy }

// ValidateCaddyfile asks the agent to validate a website Caddyfile through Caddy's own adapter.
func (s *Session) ValidateCaddyfile(ctx context.Context, caddyfile, allowedRoot string) (*agentv1.ValidateCaddyfileResult, error) {
	if !s.websiteHandy {
		return nil, ErrOlderAgent
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_ValidateCaddyfile{ValidateCaddyfile: &agentv1.ValidateCaddyfileCall{
		Caddyfile:   caddyfile,
		AllowedRoot: allowedRoot,
	}}}, validateCaddyTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetValidateCaddyfile(); out != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered Caddyfile validation with something else", s.name)
}

// RenewCertificate asks the agent to renew or retry a domain's certificate on its ingress.
func (s *Session) RenewCertificate(ctx context.Context, domain string, force bool) (*agentv1.RenewCertificateResult, error) {
	if !s.websiteHandy {
		return nil, ErrOlderAgent
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_RenewCertificate{RenewCertificate: &agentv1.RenewCertificateCall{
		Domain: domain,
		Force:  force,
	}}}, certActionTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetRenewCertificate(); out != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered certificate renewal with something else", s.name)
}

// ReadCertificate asks the agent for a domain's active PEM certificate chain and optionally its private key.
func (s *Session) ReadCertificate(ctx context.Context, domain string, includeKey bool) (*agentv1.ReadCertificateResult, error) {
	if !s.websiteHandy {
		return nil, ErrOlderAgent
	}
	r, err := s.call(ctx, &agentv1.Call{Call: &agentv1.Call_ReadCertificate{ReadCertificate: &agentv1.ReadCertificateCall{
		Domain:     domain,
		IncludeKey: includeKey,
	}}}, certActionTimeout, nil)
	if err != nil {
		return nil, err
	}
	if out := r.GetReadCertificate(); out != nil {
		return out, nil
	}
	return nil, fmt.Errorf("%s's agent answered certificate read with something else", s.name)
}
