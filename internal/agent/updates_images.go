// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/registry"
)

// imageUpdatesTimeout bounds a whole check of one image: a HEAD, a list of
// tags, and a HEAD for each of three newer tags at most, each within
// registry.RequestTimeout (ADR 0079).
const imageUpdatesTimeout = 20 * time.Second

// sourceLabel is the label an image names its source repository with.
const sourceLabel = "org.opencontainers.image.source"

// imageUpdates says what's newer than an image the server runs (ADR 0079): the
// digest its tag points at, with a HEAD, which no registry counts as a pull,
// and for a tag that's a version, the newest tag of each kind of step in its
// track, from one list of the repository's tags. It keeps nothing: the control
// plane keeps what it found.
func (im *images) imageUpdates(ctx context.Context, call *agentv1.ImageUpdatesCall) (*agentv1.ImageUpdatesResult, error) {
	if im.o.Off != "" {
		return nil, errors.New(im.o.Off)
	}
	if im.docker == nil {
		return nil, errors.New("this agent can't read images, as it has no Docker driver")
	}
	ref, err := registry.Parse(call.GetReference())
	if err != nil {
		return nil, err
	}
	// The tag a reference pinned by digest names is what's checked; with none,
	// it's latest.
	ref.Digest = ""
	if ref.Tag == "" {
		ref.Tag = "latest"
	}
	ctx, cancel := context.WithTimeout(ctx, imageUpdatesTimeout)
	defer cancel()
	res := &agentv1.ImageUpdatesResult{}
	if running := call.GetRunning(); running != "" {
		res.Source = im.sourceOf(ctx, running)
	}
	client, _ := im.ready(ctx)
	if im.isSilent(ref.Registry) {
		res.Answer = dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE
		res.Detail = "it didn't answer a moment ago"
		return res, nil
	}
	release, err := im.turn(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	digest, err := client.Head(ctx, ref)
	im.heard(ref.Registry, err)
	res.Answer, res.Digest = answer(err), digest
	if err != nil {
		res.Detail = line(err.Error())
		return res, nil
	}
	if !call.GetTags() || !registry.IsVersion(ref.Tag) {
		return res, nil
	}
	endpoint := ref.Registry
	if ref.Registry == registry.Hub && signedInAs(ctx) == "" {
		endpoint = hubAPIEndpoint
	}
	if im.isSilent(endpoint) {
		res.TagsAnswer = dockwellv1.ImageAnswer_IMAGE_ANSWER_UNREACHABLE
		return res, nil
	}
	tags, err := client.TagList(ctx, ref, registry.TrackWord(ref.Tag))
	im.heard(endpoint, err)
	res.TagsAnswer = answer(err)
	if err != nil {
		if res.Detail == "" {
			res.Detail = line(err.Error())
		}
		return res, nil
	}
	byName := make(map[string]registry.Tag, len(tags))
	names := make([]string, 0, len(tags))
	for _, t := range tags {
		byName[t.Name] = t
		names = append(names, t.Name)
	}
	if t, ok := byName[ref.Tag]; ok && !t.Pushed.IsZero() {
		res.PushTime = timestamppb.New(t.Pushed)
	}
	for _, n := range registry.NewerTags(ref.Tag, names) {
		t := byName[n.Tag]
		newer := &agentv1.NewerTag{Name: n.Tag, Kind: updateKind(n.Kind), Digest: t.Digest}
		if !t.Pushed.IsZero() {
			newer.PushTime = timestamppb.New(t.Pushed)
		}
		// Where the list didn't say what it points at, a HEAD does, while there's time.
		if newer.Digest == "" && ctx.Err() == nil {
			if d, err := client.Head(ctx, registry.Ref{Registry: ref.Registry, Repository: ref.Repository, Tag: n.Tag}); err == nil {
				newer.Digest = d
			}
		}
		res.Newer = append(res.Newer, newer)
	}
	return res, nil
}

// sourceOf is where the image that runs says its source is, from the server's
// own copy, with no network: an https address, or nothing.
func (im *images) sourceOf(ctx context.Context, running string) string {
	img, err := im.docker.InspectImage(ctx, running)
	if err != nil || img.Config == nil {
		return ""
	}
	return httpsOnly(img.Config.Labels[sourceLabel])
}

// httpsOnly is an address an image names, kept only when it's https with a
// host, and short enough to be one.
func httpsOnly(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 512 {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return ""
	}
	return u.String()
}

func updateKind(k registry.Kind) dockwellv1.UpdateKind {
	switch k {
	case registry.KindPatch:
		return dockwellv1.UpdateKind_UPDATE_KIND_PATCH
	case registry.KindMinor:
		return dockwellv1.UpdateKind_UPDATE_KIND_MINOR
	case registry.KindMajor:
		return dockwellv1.UpdateKind_UPDATE_KIND_MAJOR
	case registry.KindNone:
	}
	return dockwellv1.UpdateKind_UPDATE_KIND_UNSPECIFIED
}

// line is an error's first line, at most 200 bytes of it.
func line(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
