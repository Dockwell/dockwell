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
	opened := make([]*os.File, 0, len(files))
	sizes := make([]int64, 0, len(files))
	defer func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}()
	for _, file := range files {
		f, err := os.Open(file.Path)
		if err != nil {
			return fmt.Errorf("can't read %s for the helper's image: %w", file.Path, err)
		}
		opened = append(opened, f)
		fi, err := f.Stat()
		if err != nil {
			return fmt.Errorf("can't read %s for the helper's image: %w", file.Path, err)
		}
		sizes = append(sizes, fi.Size())
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		var err error
		for i, file := range files {
			if err = tw.WriteHeader(&tar.Header{Name: strings.TrimPrefix(file.Name, "/"), Typeflag: tar.TypeReg, Mode: file.Mode, Size: sizes[i]}); err != nil {
				break
			}
			if _, err = io.Copy(tw, opened[i]); err != nil {
				break
			}
		}
		if err == nil {
			err = tw.Close()
		}
		_ = pw.CloseWithError(err)
	}()
	defer pr.Close()
	return c.ImportImage(ctx, ref, pr)
}
