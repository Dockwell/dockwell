// SPDX-License-Identifier: AGPL-3.0-only

package volume_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// What an agent keeps of a volume has no options, which can hold a share's
// password, and the spec a step applies keeps its own (ADR 0054).
func TestWhatIsKeptOfAVolumeHasNoOptions(t *testing.T) {
	v := &resourcev1.Volume{Driver: "local", Options: map[string]string{"type": "cifs", "o": "username=u,password=hunter2"}, Labels: map[string]string{"team": "web"}}
	got := volume.WithoutOptions(v).(*resourcev1.Volume)
	if len(got.GetOptions()) != 0 || got.GetDriver() != "local" || got.GetLabels()["team"] != "web" {
		t.Errorf("kept %v", got)
	}
	if v.GetOptions()["o"] == "" {
		t.Error("the spec a step applies lost its options")
	}
	for _, other := range []proto.Message{&resourcev1.Volume{Driver: "local"}, &resourcev1.Container{Image: "nginx:1.27"}, nil} {
		if volume.WithoutOptions(other) != other {
			t.Errorf("%v changed", other)
		}
	}
}
