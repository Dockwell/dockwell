// SPDX-License-Identifier: AGPL-3.0-only

package secrets

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/variable"
	"github.com/dockwell/dockwell/internal/scope"
	"github.com/dockwell/dockwell/internal/store"
)

func toolboxValueKind(b *v1.ToolboxBinding) resource.Kind {
	if b.GetSecret() {
		return variable.SecretKind
	}
	return variable.VariableKind
}

func (k *Keeper) toolboxValue(ctx context.Context, tx *store.Tx, b *v1.ToolboxBinding) (store.Resource, string, error) {
	kind := toolboxValueKind(b)
	r, err := tx.GetResource(ctx, string(kind), b.GetEnvironmentId(), b.GetSource())
	if err != nil {
		return r, "", err
	}
	rev := b.GetRevision()
	if rev == 0 {
		if r.Spec == nil {
			return r, "", store.ErrNotFound
		}
		rev = int64(r.Revision)
	}
	value, err := k.valueAt(ctx, tx, b.GetEnvironmentId(), resource.Use{
		As: b.GetName(), Kind: kind, Name: b.GetSource(), Revision: int(rev),
	})
	if err == nil && (!utf8.ValidString(value) || strings.ContainsRune(value, 0) || len(value) > 32<<10) {
		err = errors.New("a Dockwell environment value can't be written to systemd")
	}
	return r, value, err
}

func toolboxValueKey(id string, b *v1.ToolboxBinding) string {
	return fmt.Sprintf("toolbox-value:%s:%d:%s", id, b.GetRevision(), b.GetName())
}

func toolboxLiteralPurpose(b *v1.ToolboxBinding) string {
	return "toolbox-literal:" + b.GetName() + ":" + hex.EncodeToString(b.GetValueDigest())
}

// SealHostToolboxLiteral replaces request plaintext with install-key ciphertext
// before the operation is planned or written to the change history.
func (k *Keeper) SealHostToolboxLiteral(b *v1.ToolboxBinding) error {
	value := b.GetLiteralValue()
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) || len(value) > 32<<10 {
		return errors.New("a direct environment value can't be written to systemd")
	}
	key := rand.Text()
	b.ValueDigest = resource.ToolboxLiteralDigest(key, value)
	plain := make([]byte, 0, len(key)+1+len(value))
	plain = append(plain, key...)
	plain = append(plain, 0)
	plain = append(plain, value...)
	defer clear(plain)
	b.SealedLiteral = k.key.Seal(toolboxLiteralPurpose(b), plain)
	b.LiteralValue = nil
	b.Secret = true
	return nil
}

func (k *Keeper) openToolboxLiteral(b *v1.ToolboxBinding, values map[string]string) error {
	plain, err := k.key.Open(toolboxLiteralPurpose(b), b.GetSealedLiteral())
	if err != nil {
		return errors.New("a direct environment value couldn't be opened. Review a new plan")
	}
	defer clear(plain)
	key, value, ok := strings.Cut(string(plain), "\x00")
	if !ok || !hmac.Equal(b.GetValueDigest(), resource.ToolboxLiteralDigest(key, value)) {
		return errors.New("a direct environment value changed. Review a new plan")
	}
	values[b.GetName()] = value
	values[resource.ToolboxLiteralKey(b.GetName())] = key
	return nil
}

// PinHostToolboxValue reads and pins the exact value in the same store view as
// the environment label. Only plain variables are copied into the plan.
func (k *Keeper) PinHostToolboxValue(ctx context.Context, tx *store.Tx, b *v1.ToolboxBinding) error {
	env, err := tx.GetResourceByID(ctx, b.GetEnvironmentId())
	if err != nil {
		return err
	}
	if env.Kind != "environment" || env.Spec == nil {
		return store.ErrNotFound
	}
	r, value, err := k.toolboxValue(ctx, tx, b)
	if err != nil {
		return err
	}
	b.Revision = int64(r.Revision)
	spec := &resourcev1.Environment{}
	if err := proto.Unmarshal(env.Spec, spec); err != nil {
		return err
	}
	b.EnvironmentName = spec.GetDisplayName()
	if b.EnvironmentName == "" {
		b.EnvironmentName = env.Name
	}
	b.EnvironmentProtected = scope.Protected(env.Name, spec)
	if !b.GetSecret() {
		b.ReviewedValue = value
	}
	b.ValueDigest = k.Digest(toolboxValueKey(r.ID, b), []byte(value))
	return nil
}

// HostToolboxValues opens only the values pinned by the reviewed plan. A later
// revision cannot replace them, and an external source that changed must be
// reviewed again. The session seals the result to the target agent.
func (k *Keeper) HostToolboxValues(ctx context.Context, bindings []*v1.ToolboxBinding) (map[string]string, error) {
	values := make(map[string]string, len(bindings))
	err := k.db.View(ctx, func(tx *store.Tx) error {
		for _, b := range bindings {
			if b.LiteralValue != nil {
				return errors.New("a direct environment value wasn't sealed. Review a new plan")
			}
			if len(b.GetSealedLiteral()) != 0 {
				if err := k.openToolboxLiteral(b, values); err != nil {
					return err
				}
				continue
			}
			if b.GetRevision() <= 0 {
				return errors.New("an environment value wasn't pinned. Review a new plan")
			}
			r, value, err := k.toolboxValue(ctx, tx, b)
			if err != nil {
				return err
			}
			if !hmac.Equal(b.GetValueDigest(), k.Digest(toolboxValueKey(r.ID, b), []byte(value))) ||
				(!b.GetSecret() && value != b.GetReviewedValue()) {
				return errors.New("a service environment value changed. Review a new plan")
			}
			values[b.GetName()] = value
		}
		return nil
	})
	if err != nil {
		clear(values)
		return nil, err
	}
	return values, nil
}
