// SPDX-License-Identifier: AGPL-3.0-only

// Package probe checks a connection from where an app is: the agent runs its own
// binary's probe subcommand in a container made of that binary alone, on the app's
// networks, and reads what it answers (ADR 0099). Main is what runs in there: it
// reads one Request on its input, secrets and all, checks the one connection
// through the guard, and writes one Result. Runner is the agent's side.
//
// What it answers is codes, never a body: a service's error code, and its message
// where it holds nothing of the request.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"time"

	"github.com/dockwell/dockwell/internal/netguard"
)

// Command is the subcommand the probe's container runs.
const Command = "__probe"

// The bounds each check keeps, connecting and in all (ADR 0099).
const (
	ConnectWait = netguard.DialTimeout
	CheckWait   = 60 * time.Second
)

// maxRequest bounds what Main reads of its input.
const maxRequest = 64 << 10

// Kinds of connection a probe checks.
const (
	KindS3 = "s3"
)

// Request is one check, as the agent writes it on the probe's input.
type Request struct {
	// Kind is what it checks, such as s3.
	Kind string `json:"kind"`
	// S3 is an S3 bucket to check, for KindS3.
	S3 *S3 `json:"s3,omitempty"`
	// Allow are the networks the app is on, which the probe reaches though
	// they're private, as the app does. Refuse are the server's own addresses,
	// which it never reaches.
	Allow  []netip.Prefix `json:"allow,omitempty"`
	Refuse []netip.Prefix `json:"refuse,omitempty"`
	// Private takes any private address too, for a connection an admin added.
	Private bool `json:"private,omitempty"`
}

// Result is what a check found.
type Result struct {
	// Passed says the connection did everything asked of it.
	Passed bool `json:"passed"`
	// Step is where it stopped, such as connect, write, read, presign or delete.
	Step string `json:"step,omitempty"`
	// Code is what the service said, such as an S3 error's Code, or what kept it
	// from answering: refused, timeout, unreachable or tls. Status is its HTTP
	// status, where it had one.
	Code   string `json:"code,omitempty"`
	Status int    `json:"status,omitempty"`
	// Message is the service's own, where it holds nothing of the request.
	Message string `json:"message,omitempty"`
	// Words say in a few words why it couldn't connect, as netguard has them.
	Words string `json:"words,omitempty"`
	// Region is the bucket's region, where the service said another than asked.
	Region string `json:"region,omitempty"`
}

// Main reads one Request from in, checks it, and writes its Result to out, as
// JSON. It's 0 once a check ran, whatever it found, and 1 for a request it can't
// read, which it says on errs.
func Main(in io.Reader, out, errs io.Writer) int {
	var req Request
	if err := json.NewDecoder(io.LimitReader(in, maxRequest)).Decode(&req); err != nil {
		fmt.Fprintf(errs, "the probe can't read what it was asked: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), CheckWait)
	defer cancel()
	res, err := Check(ctx, &req)
	if err != nil {
		fmt.Fprintln(errs, err.Error())
		return 1
	}
	if err := json.NewEncoder(out).Encode(res); err != nil {
		fmt.Fprintf(errs, "the probe can't say what it found: %v\n", err)
		return 1
	}
	return 0
}

// Check runs one check, as Main does in the probe's container, with its own
// guard. An error is a request no check can run.
func Check(ctx context.Context, req *Request) (Result, error) {
	guard := netguard.Options{Allow: req.Allow, Refuse: req.Refuse, Private: req.Private, Timeout: CheckWait}
	if req.Kind != KindS3 {
		return Result{}, fmt.Errorf("the probe doesn't check %q", req.Kind)
	}
	if req.S3 == nil {
		return Result{}, errors.New("the probe was asked of S3 with no bucket")
	}
	return checkS3(ctx, req.S3, guard), nil
}
