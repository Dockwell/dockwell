// SPDX-License-Identifier: AGPL-3.0-only

package docker

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// ImageFile is a file of the server's that goes into an image ImportFiles makes,
// at Name inside it.
type ImageFile struct {
	// Name is where it is in the image, such as /dockwell.
	Name string
	// Path is the file on the server, such as /proc/self/exe.
	Path string
	// Mode is its mode in the image.
	Mode int64
}

// ImportFiles makes an image of these files alone, copied, not mounted, as a
// helper made of the agent's own static binary runs with nothing else there and
// comes from no registry (ADR 0033, ADR 0099).
func (c *Client) ImportFiles(ctx context.Context, ref string, files []ImageFile) error {
	pr, pw := io.Pipe()
	go func() { _ = pw.CloseWithError(WriteFiles(pw, files)) }()
	defer pr.Close()
	return c.ImportImage(ctx, ref, pr)
}

// WriteFiles writes files as the one layer ImportFiles imports: a tar with no
// times or owners in it, so the same files always write the same bytes, and the
// layer's DiffID, the SHA-256 of them, says what an image holds.
func WriteFiles(w io.Writer, files []ImageFile) error {
	tw := tar.NewWriter(w)
	for _, file := range files {
		f, err := os.Open(file.Path)
		if err != nil {
			return fmt.Errorf("can't read %s for the helper's image: %w", file.Path, err)
		}
		fi, err := f.Stat()
		if err == nil {
			err = tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(file.Name, "/"), Typeflag: tar.TypeReg, Mode: file.Mode, Size: fi.Size()})
		}
		if err == nil {
			_, err = io.Copy(tw, f)
		}
		_ = f.Close()
		if err != nil {
			return fmt.Errorf("can't read %s for the helper's image: %w", file.Path, err)
		}
	}
	return tw.Close()
}
