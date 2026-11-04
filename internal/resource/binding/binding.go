// SPDX-License-Identifier: AGPL-3.0-only

// Package binding is how a service gets the values of the variables and
// secrets it uses, on the server it runs on. See
// docs/adr/0022-secrets-encrypted-at-rest.md.
//
// The control plane seals the values for one step to the agent stream's
// delivery key (Seal). The agent opens them in memory (Key.Open) and hands
// them to the driver for that call alone (resource.WithValues). The driver
// builds the container's environment from them (Environment) and records
// on the container which variables and secrets it used, never their values
// (Label), so it can read the service back as its spec (Restore).
//
// The Docker driver (#26) calls Environment when it creates a service's
// container, and gets everything else from the service driver here.
package binding

import (
	"context"
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/variable"
)

// infoPrefix starts the HPKE info, which binds sealed values to one
// server and one resource, so values sealed for one step can't be passed
// off as another's.
const infoPrefix = "dockwell.values.v1"

// The HPKE suite: DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, AES-256-GCM.
var (
	kem  = hpke.DHKEM(ecdh.X25519())
	kdf  = hpke.HKDFSHA256()
	aead = hpke.AES256GCM()
)

func info(server string, ref resource.Ref) []byte {
	return []byte(strings.Join([]string{infoPrefix, server, string(ref.Kind), ref.Scope, ref.Name}, "\x00"))
}

// Key is an agent stream's delivery key. The agent makes one for each
// stream and keeps it in memory only: values sealed to it can't be opened
// once the stream ends, so nothing an agent could keep would open them.
type Key struct {
	private hpke.PrivateKey
	public  []byte
}

// NewKey makes a delivery key.
func NewKey() (*Key, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("can't make a delivery key: %w", err)
	}
	pk, err := hpke.NewDHKEMPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("can't make a delivery key: %w", err)
	}
	return &Key{private: pk, public: priv.PublicKey().Bytes()}, nil
}

// Public is what the agent sends in its hello.
func (k *Key) Public() []byte { return slices.Clone(k.public) }

// ErrNoDeliveryKey means an agent sent no delivery key, as an agent older
// than this build doesn't.
var ErrNoDeliveryKey = errors.New("the agent sent no delivery key")

// Seal seals the values for a resource on a server to the delivery key
// its agent's stream sent.
func Seal(deliveryKey []byte, server string, ref resource.Ref, values map[string]string) ([]byte, error) {
	if len(deliveryKey) == 0 {
		return nil, ErrNoDeliveryKey
	}
	pub, err := kem.NewPublicKey(deliveryKey)
	if err != nil {
		return nil, fmt.Errorf("the agent's delivery key isn't an X25519 key: %w", err)
	}
	plain, err := proto.MarshalOptions{Deterministic: true}.Marshal(&agentv1.Values{Values: values})
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	return hpke.Seal(pub, kdf, aead, info(server, ref), plain)
}

// Open opens values sealed to this key for a resource on this server.
func (k *Key) Open(server string, ref resource.Ref, sealed []byte) (map[string]string, error) {
	plain, err := hpke.Open(k.private, kdf, aead, info(server, ref), sealed)
	if err != nil {
		return nil, errors.New("can't open the values sent for this step: they weren't sealed for this stream, server and resource")
	}
	defer clear(plain)
	var v agentv1.Values
	if err := proto.Unmarshal(plain, &v); err != nil {
		return nil, fmt.Errorf("can't read the values sent for this step: %w", err)
	}
	return v.GetValues(), nil
}

// Environment returns the environment a service's container runs with:
// its app's own variables, then each variable and secret it uses, under
// the name it sees it as, with the values resource.WithValues gave the
// call. It fails, naming what is missing but never a value, when a value
// wasn't sent.
func Environment(ctx context.Context, svc *resourcev1.Service) (map[string]string, error) {
	env := maps.Clone(svc.GetApp().GetEnv())
	uses := Uses(svc)
	if len(uses) == 0 {
		return env, nil
	}
	if env == nil {
		env = map[string]string{}
	}
	values := resource.ValuesFrom(ctx)
	for _, u := range uses {
		v, ok := values[u.As]
		if !ok {
			return nil, fmt.Errorf("the control plane sent no value for %s, which is %s %s; plan the change again", u.As, u.Kind, u.Name)
		}
		env[u.As] = v
	}
	return env, nil
}

// Uses lists what a service uses, by the name it sees each as, sorted.
func Uses(svc *resourcev1.Service) []resource.Use {
	var out []resource.Use
	add := func(kind resource.Kind, m map[string]string) {
		for as, name := range m {
			if name == "" {
				name = as
			}
			out = append(out, resource.Use{As: as, Kind: kind, Name: name, Revision: int(svc.GetRevisions()[as])})
		}
	}
	add(variable.VariableKind, svc.GetVariables())
	add(variable.SecretKind, svc.GetSecrets())
	slices.SortFunc(out, func(a, b resource.Use) int { return strings.Compare(a.As, b.As) })
	return out
}

// LabelKey is the container label that records what a service uses: each
// name it sees, the kind and name of what it uses, and the revision, such
// as "DATABASE_URL=secret:DATABASE_URL@4,LOG_LEVEL=variable:LOG_LEVEL@2".
// Never a value.
const LabelKey = "dockwell.uses"

// Label is the LabelKey label for a service, or empty when it uses
// nothing.
func Label(svc *resourcev1.Service) string {
	uses := Uses(svc)
	parts := make([]string, len(uses))
	for i, u := range uses {
		parts[i] = fmt.Sprintf("%s=%s:%s@%d", u.As, u.Kind, u.Name, u.Revision)
	}
	return strings.Join(parts, ",")
}

// Restore reads a service back from its container, as Observe returns it:
// it takes the LabelKey label off, and with it the variables it names from
// the environment, and puts them back as what the service uses. A
// container without the label used nothing.
func Restore(c *resourcev1.Container) (*resourcev1.Service, error) {
	svc := &resourcev1.Service{Runs: &resourcev1.Service_App{App: c}}
	label, ok := c.GetLabels()[LabelKey]
	if !ok {
		return svc, nil
	}
	delete(c.Labels, LabelKey)
	if len(c.Labels) == 0 {
		c.Labels = nil
	}
	if label == "" {
		return svc, nil
	}
	for _, part := range strings.Split(label, ",") {
		as, rest, ok1 := strings.Cut(part, "=")
		kind, rest, ok2 := strings.Cut(rest, ":")
		name, rev, ok3 := strings.Cut(rest, "@")
		n, err := strconv.Atoi(rev)
		if !ok1 || !ok2 || !ok3 || err != nil || as == "" || name == "" {
			return nil, fmt.Errorf("the container's %s label, %q, isn't one Dockwell wrote", LabelKey, label)
		}
		switch resource.Kind(kind) {
		case variable.VariableKind:
			if svc.Variables == nil {
				svc.Variables = map[string]string{}
			}
			svc.Variables[as] = name
		case variable.SecretKind:
			if svc.Secrets == nil {
				svc.Secrets = map[string]string{}
			}
			svc.Secrets[as] = name
		default:
			return nil, fmt.Errorf("the container's %s label uses a %q, which isn't a kind a service uses", LabelKey, kind)
		}
		if n > 0 {
			if svc.Revisions == nil {
				svc.Revisions = map[string]int64{}
			}
			svc.Revisions[as] = int64(n)
		}
		delete(c.Env, as)
	}
	if len(c.Env) == 0 {
		c.Env = nil
	}
	return svc, nil
}
