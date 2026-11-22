// SPDX-License-Identifier: AGPL-3.0-only

package artifact

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
)

func TestBuildContextRejectsUnsafeTarMembers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []tar.Header
	}{
		{"escape", []tar.Header{{Name: "../payload", Typeflag: tar.TypeReg}}},
		{"absolute", []tar.Header{{Name: "/payload", Typeflag: tar.TypeReg}}},
		{"link", []tar.Header{{Name: "payload", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}},
		{"device", []tar.Header{{Name: "payload", Typeflag: tar.TypeChar}}},
		{"duplicate", []tar.Header{{Name: "./payload", Typeflag: tar.TypeReg}, {Name: "payload", Typeflag: tar.TypeReg}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := tar.NewWriter(&buf)
			for _, h := range tc.items {
				if err := w.WriteHeader(&h); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if err := walkBuildTar(t.Context(), &buf, func(*tar.Header, io.Reader) error { return nil }); err == nil {
				t.Fatal("accepted an unsafe build context")
			}
		})
	}
}

func TestEmptyBuildContextCanUseDockerfileOverride(t *testing.T) {
	var b bytes.Buffer
	if err := tar.NewWriter(&b).Close(); err != nil {
		t.Fatal(err)
	}
	if err := walkBuildTar(t.Context(), &b, func(*tar.Header, io.Reader) error {
		t.Fatal("an empty context has no entries")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
