// SPDX-License-Identifier: AGPL-3.0-only

package artifact

import (
	"bytes"
	"context"
	"testing"
	"time"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/id"
)

func TestPushReleasesValidationBeforeSending(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data, configID := makeArchive(t, nil)
	const owner = "test/session"
	change := id.New(id.Change)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	info, reviewed, err := s.PreparePush(ctx, owner, bytes.NewReader(data), func(_ *Archive, config string) bool {
		return config == configID
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pin(&agentv1.DockerArtifactPin{ArtifactId: info.GetArtifactId(), ChangeId: change}, owner); err != nil {
		t.Fatal(err)
	}
	if err := s.WithPush(ctx, info.GetArtifactId(), change, info.GetSha256(), func(source *Distribution) error {
		if source.Digest != reviewed.GetManifestDigest() {
			t.Fatal("apply saw a different manifest")
		}
		// A seal can take the validation slot while this push is uploading.
		release, err := s.TakeValidation(ctx)
		if err == nil {
			release()
		}
		return err
	}); err != nil {
		t.Fatalf("push kept the validation slot while sending: %v", err)
	}
}

func TestPushWaitsForSendSlot(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var held []func()
	for range 4 {
		release, err := s.TakePush(ctx, func() { t.Fatal("a send slot should be free") })
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, release)
	}
	queued := false
	release, err := s.TakePush(ctx, func() {
		queued = true
		held[0]()
	})
	for _, done := range held[1:] {
		done()
	}
	if err != nil || !queued {
		t.Fatalf("fifth send should wait for a slot: queued=%t, err=%v", queued, err)
	}
	release()
}
