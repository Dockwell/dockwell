// SPDX-License-Identifier: AGPL-3.0-only

package resource

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/protobuf/proto"
)

// ErrAlreadySo is what a driver's Apply returns when it found nothing to do,
// which only it could find out, as a pull that finds nothing newer does. It
// changed nothing, and what runs is still verified, as a step that reads done
// says it runs, and is healthy.
var ErrAlreadySo = errors.New("already as asked")

// Outcome is what a step found out on its server, for the record.
type Outcome struct {
	// Digest is the image the step leaves running, as its registry knows it.
	// Empty for one no registry knows.
	Digest string
	// Previous is the digest of the image it replaces, where a pull found a newer
	// one.
	Previous string
	// Replacing says a pull found a newer image, and the step is about to replace
	// what runs with it.
	Replacing bool
	// Note says, for people, what the step did where its usual words wouldn't be
	// true, as when it did nothing.
	Note string
	// Untouched says the step failed before it changed anything on the server,
	// as a pull that can't reach its registry does, so what ran there still runs.
	Untouched bool
	// Did is what the step did in the end, where that isn't what its plan says.
	Did *Action
}

// Did is what Outcome.Did holds.
func Did(a Action) *Action { return &a }

type outcomeKey struct{}

// WithOutcome returns a context whose step reports what it finds out to fn, in
// order, before the step ends.
func WithOutcome(ctx context.Context, fn func(Outcome)) context.Context {
	return context.WithValue(ctx, outcomeKey{}, fn)
}

// OutcomeFunc returns what ctx reports outcomes to, or nil.
func OutcomeFunc(ctx context.Context) func(Outcome) {
	fn, _ := ctx.Value(outcomeKey{}).(func(Outcome))
	return fn
}

// Report tells whoever applies the step what it found out.
func Report(ctx context.Context, o Outcome) {
	if fn := OutcomeFunc(ctx); fn != nil {
		fn(o)
	}
}

// ImageNamer is a Type whose spec names an image, so a rollback can say which
// image it means by digest.
type ImageNamer interface {
	// ImageOf is the image a spec runs, as it is written.
	ImageOf(spec proto.Message) string
	// SetImage changes the image a spec runs.
	SetImage(spec proto.Message, image string)
	// WhatRuns is a copy of a spec to compare what runs in it: all of it but its
	// limits, which run nothing, so what's added to a spec later counts until someone
	// says it doesn't.
	WhatRuns(spec proto.Message) proto.Message
}

// BareImage is an image without the digest it may be pinned to.
func BareImage(image string) string {
	bare, _, _ := strings.Cut(image, "@")
	return bare
}

// PinnedImage is an image pinned to a digest, keeping its tag to say what it was.
func PinnedImage(image, digest string) string {
	return BareImage(image) + "@" + digest
}
