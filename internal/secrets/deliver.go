// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import (
	"crypto/hpke"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	"github.com/dockwell/dockwell/internal/delivery"
	"github.com/dockwell/dockwell/internal/resource"
)

// ErrNoDeliveryKey means an agent sent no delivery key, as an agent older
// than this build doesn't.
var ErrNoDeliveryKey = errors.New("the agent sent no delivery key")

// SealValues seals the values for a resource on a server to the delivery
// key its agent's stream sent, with the suite and info internal/delivery
// says, for the agent to open in memory for that step alone.
func SealValues(deliveryKey []byte, server string, ref resource.Ref, values map[string]string) ([]byte, error) {
	if len(deliveryKey) == 0 {
		return nil, ErrNoDeliveryKey
	}
	kem, kdf, aead := delivery.Suite()
	pub, err := kem.NewPublicKey(deliveryKey)
	if err != nil {
		return nil, fmt.Errorf("the agent's delivery key isn't an X25519 key: %w", err)
	}
	plain, err := proto.MarshalOptions{Deterministic: true}.Marshal(&agentv1.Values{Values: values})
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	return hpke.Seal(pub, kdf, aead, delivery.Info(server, ref), plain)
}
