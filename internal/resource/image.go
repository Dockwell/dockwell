// SPDX-License-Identifier: AGPL-3.0-only

package resource

import "context"

// Image is the image a resource runs on its server: its ID there, and what its
// registry calls it, as name@sha256:…, where it came from one. An agent keeps
// it with the resource's desired state, so what it makes again runs the same
// image rather than whatever a tag names by then (ADR 0054).
type Image struct {
	ID     string
	Digest string
}

// Imaged is a driver whose resource runs an image, which it can name.
type Imaged interface {
	// RunsImage is the image the resource runs now, or the zero Image when it
	// runs none, as when it's gone.
	RunsImage(ctx context.Context, ref Ref) (Image, error)
}

// RunsImage is the image ref runs on this server, if its kind runs one.
func (l Local) RunsImage(ctx context.Context, ref Ref) (Image, error) {
	d, err := l.Drivers.Driver(ref.Kind)
	if err != nil {
		return Image{}, err
	}
	if im, ok := d.(Imaged); ok {
		return im.RunsImage(ctx, ref)
	}
	return Image{}, nil
}
