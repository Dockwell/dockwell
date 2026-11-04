// SPDX-License-Identifier: AGPL-3.0-only

package compose

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/distribution/reference"
	"google.golang.org/protobuf/proto"

	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/container"
	"github.com/dockwell/dockwell/internal/resource/volume"
)

// The cap is on the file; variables aren't capped, and each ${A}
// copies its value. The API's limit is 8 MiB.
func TestVariablesCantGetRoundTheCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("services:\n  web:\n    image: nginx:1.27\n    command:\n")
	const refs = 4000
	for range refs {
		b.WriteString("      - ${A}\n")
	}
	const size = 64 << 10
	vars := map[string]string{"A": strings.Repeat("x", size)}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := Parse(t.Context(), []byte(b.String()), vars)
	runtime.ReadMemStats(&after)
	file, alloc := b.Len(), after.TotalAlloc-before.TotalAlloc
	t.Logf("a %d KiB file and one %d KiB variable allocated %d MiB", file>>10, size>>10, alloc>>20)
	if err == nil || !strings.Contains(err.Error(), "past 2 MiB once they are expanded") {
		t.Errorf("expanding %d MiB = %v, want it refused", refs*size>>20, err)
	}
	if alloc > uint64(file)*256 {
		t.Errorf("allocated %d MiB for a %d KiB file", alloc>>20, file>>10)
	}
	// Variables in defaults count, and what fits still reads.
	nested := "services:\n  web:\n    image: nginx:1.27\n    command:\n" + strings.Repeat("      - ${NOPE:-${A}}\n", 4000)
	if _, err := Parse(t.Context(), []byte(nested), vars); err == nil {
		t.Error("expanding through a default passes")
	}
	small := "services:\n  web:\n    image: nginx:1.27\n    command:\n" + strings.Repeat("      - ${A}\n", 4)
	if _, err := Parse(t.Context(), []byte(small), vars); err != nil {
		t.Errorf("expanding 256 KiB = %v", err)
	}
	// The variables themselves are capped too, used or not.
	large := map[string]string{"A": strings.Repeat("x", MaxVariables+1)}
	if _, err := Parse(t.Context(), []byte("services:\n  web:\n    image: nginx:1.27\n"), large); err == nil || !strings.Contains(err.Error(), "the variables come to") {
		t.Errorf("variables past the cap = %v", err)
	}
}

// CheckImage against the library Docker reads references with.
func TestImageGrammarAgreesWithDocker(t *testing.T) {
	hex64 := strings.Repeat("a", 64)
	refs := []string{
		"nginx", "nginx:1.27", "library/nginx", "ghcr.io/acme/web:1.4.2", "localhost/foo", "localhost:5000/foo", "registry:5000/foo:1",
		"Acme/web", "ACME.io/web", "ghcr.io/Acme/web", "ghcr.io/acme/Web:1", "nginx@sha256:" + hex64, "nginx:1.27@sha256:" + hex64,
		"ghcr.io:443/acme/web@sha256:" + hex64, "[::1]:5000/foo:1", "[2001:db8::1]/foo", "192.168.1.1:5000/a/b", "a__b/c--d/e.f_g", "a___b", "a..b", "-a", "a-",
		"my_registry:5000/foo", "my-registry.local/foo", "a/b/c/d/e/f", "foo:latest:1", "foo@bar", "foo@sha256:abc", "foo:", ":1", "/foo", "foo/", "foo//bar",
		"foo:" + strings.Repeat("a", 128), "foo:" + strings.Repeat("a", 129), "foo:.a", "foo:-a", "foo:_a", "sha256:" + hex64, hex64,
		"foo@sha256+b64u:" + strings.Repeat("a", 44), "foo@sha512:" + strings.Repeat("a", 128), "foo@md5:" + strings.Repeat("a", 32),
		"1", "1/2", "a.b/c", "a.b:1/c:2", "UPPER", "UPPER:1", "lower/UPPER", "xn--80ak6aa92e.com/a", "EXAMPLE.COM/foo", "example.com:80a/foo",
		"-registry.io/foo", "registry-.io/foo", "registry..io/foo", strings.Repeat("a", 256), strings.Repeat("a/", 127) + "a",
	}
	r := rand.New(rand.NewPCG(1, 2))
	const alphabet = "abZ09._-/:@[]"
	for range 200000 {
		var s strings.Builder
		for range 1 + r.IntN(14) {
			s.WriteByte(alphabet[r.IntN(len(alphabet))])
		}
		refs = append(refs, s.String())
	}
	var tooStrict, tooLoose []string
	for _, ref := range refs {
		_, derr := reference.ParseNormalizedNamed(ref)
		cerr := container.CheckImage(ref)
		switch {
		case derr == nil && cerr != nil:
			tooStrict = append(tooStrict, fmt.Sprintf("%q: %v", ref, cerr))
		case derr != nil && cerr == nil:
			tooLoose = append(tooLoose, fmt.Sprintf("%q: docker says %v", ref, derr))
		}
	}
	for i, s := range tooStrict {
		if i < 15 {
			t.Errorf("refused, and Docker takes it: %s", s)
		}
	}
	for i, s := range tooLoose {
		if i < 15 {
			t.Logf("taken, and Docker refuses it: %s", s)
		}
	}
	t.Logf("%d refs: %d refused that Docker takes, %d taken that Docker refuses", len(refs), len(tooStrict), len(tooLoose))
}

// Every way a file might reach past the rules: each should be refused by
// Parse, or by the spec's Validate, and none should pass as harmless.
func TestComposeRefusals(t *testing.T) {
	svc := func(extra string) string { return "services:\n  s:\n    image: alpine:3\n" + extra }
	vars := map[string]string{"M": "host", "T": "bind", "S": "/", "K": "privileged"}
	for name, file := range map[string]string{
		"devices":        svc("    devices: [/dev/sda]\n"),
		"pid":            svc("    pid: host\n"),
		"ipc":            svc("    ipc: host\n"),
		"userns":         svc("    userns_mode: host\n"),
		"security_opt":   svc("    security_opt: [seccomp=unconfined]\n"),
		"sysctls":        svc("    sysctls: {a: b}\n"),
		"cgroup_parent":  svc("    cgroup_parent: x\n"),
		"cgroup":         svc("    cgroup: host\n"),
		"volumes_from":   svc("    volumes_from: [other]\n"),
		"extends":        svc("    extends: {service: x}\n"),
		"network cont":   svc("    network_mode: \"container:x\"\n"),
		"network svc":    svc("    network_mode: \"service:x\"\n"),
		"network none":   svc("    network_mode: none\n"),
		"tmpfs key":      svc("    tmpfs: [/run]\n"),
		"tmpfs mount":    svc("    volumes:\n      - {type: tmpfs, target: /x}\n"),
		"npipe":          svc("    volumes:\n      - {type: npipe, source: x, target: /x}\n"),
		"cluster":        svc("    volumes:\n      - {type: cluster, source: x, target: /x}\n"),
		"image mount":    svc("    volumes:\n      - {type: image, source: x, target: /x}\n"),
		"create path":    svc("    volumes:\n      - {type: bind, source: /srv, target: /x, bind: {create_host_path: true}}\n"),
		"propagation":    svc("    volumes:\n      - {type: bind, source: /srv, target: /x, bind: {propagation: rshared}}\n"),
		"short shared":   svc("    volumes: [\"/srv:/x:rshared\"]\n"),
		"relative":       svc("    volumes: [\"./x:/x\"]\n"),
		"home":           svc("    volumes: [\"~/x:/x\"]\n"),
		"windows":        svc("    volumes: [\"C:\\\\x:/x\"]\n"),
		"anonymous":      svc("    volumes: [\"/x\"]\n"),
		"root short":     svc("    volumes: [\"/:/x\"]\n"),
		"root dots":      svc("    volumes: [\"/srv/../:/x\"]\n"),
		"root slashes":   svc("    volumes: [\"//:/x\"]\n"),
		"root long":      svc("    volumes:\n      - {type: bind, source: /, target: /x}\n"),
		"root var":       svc("    volumes:\n      - {type: \"${T}\", source: \"${S}\", target: /x}\n"),
		"proc":           svc("    volumes: [\"/proc/1/root:/x\"]\n"),
		"data":           svc("    volumes: [\"/var/lib/dockwell/control:/x:ro\"]\n"),
		"docker":         svc("    volumes: [\"/var/lib/docker/:/x:ro\"]\n"),
		"merge key":      "x-a: &a\n  pid: host\nservices:\n  s:\n    <<: *a\n    image: alpine:3\n",
		"external":       svc("    volumes: [\"v:/x\"]\n") + "volumes:\n  v:\n    external: true\n",
		"named":          svc("    volumes: [\"v:/x\"]\n") + "volumes:\n  v:\n    name: other\n",
		"variable key":   svc("    ${K}: true\n"),
		"secrets":        svc("    secrets: [s]\n"),
		"configs":        svc("    configs: [s]\n"),
		"device":         svc("    device_cgroup_rules: [a]\n"),
		"init":           svc("    init: true\n"),
		"runtime":        svc("    runtime: runc\n"),
		"gpus":           svc("    gpus: all\n"),
		"tmpfs in long":  svc("    volumes:\n      - {type: bind, source: /srv, target: /x, tmpfs: {size: 1}}\n"),
		"volume subpath": svc("    volumes:\n      - {type: volume, source: v, target: /x, volume: {subpath: ../..}}\n") + "volumes:\n  v: {}\n",
		"device opts":    svc("    volumes: [\"v:/x\"]\n") + "volumes:\n  v:\n    driver_opts: {type: none, o: rbind, device: /proc}\n",
	} {
		s, err := Parse(t.Context(), []byte(file), vars)
		if err == nil {
			for _, i := range s.Services {
				if err = container.Validate(i.Spec.GetApp()); err != nil {
					break
				}
			}
		}
		if err == nil {
			for _, v := range s.Volumes {
				d := volume.Driver{}
				if err = d.Validate(v.Spec); err != nil {
					break
				}
			}
		}
		if err == nil {
			t.Errorf("%s: nothing refused it", name)
		}
	}
}

// Docker's own CIFS example carries the password in o. A volume's options
// aren't sensitive, so the plan and the export carry it in the clear.
func TestVolumeOptionsHoldSecrets(t *testing.T) {
	file := "services:\n  s:\n    image: alpine:3\n    volumes: [\"share:/x\"]\nvolumes:\n  share:\n    driver_opts: {type: cifs, device: //nas/share, o: \"username=svc,password=hunter2\"}\n"
	s, err := Parse(t.Context(), []byte(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range resource.Diff(nil, s.Volumes[0].Spec) {
		if strings.Contains(c.To, "hunter2") && !c.Sensitive {
			t.Errorf("the plan shows %s = %q", c.Path, c.To)
		}
	}
	out, err := Export("env_x", s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out.File), "hunter2") {
		t.Errorf("the export holds the password, and omits %v", out.Omitted)
	}
	if !slices.Contains(out.Omitted, "volumes.share.driver_opts") || strings.Contains(string(out.File), "driver_opts") {
		t.Errorf("the export omits %v, and reads:\n%s", out.Omitted, out.File)
	}
	// The field is masked, and still compares.
	other := proto.CloneOf(s.Volumes[0].Spec)
	other.Options["o"] = "username=svc,password=other"
	changes := resource.Diff(s.Volumes[0].Spec, other)
	if resource.Equal(s.Volumes[0].Spec, other) || len(changes) != 1 || !changes[0].Sensitive || changes[0].From != "" || changes[0].To != "" {
		t.Errorf("another password = %+v, want it seen, and masked", changes)
	}
}

// Reading a file takes memory, and a write token can send as many as it
// likes, so they wait their turn, and a caller can stop waiting.
func TestFilesAreReadAFewAtATime(t *testing.T) {
	for range cap(parsing) {
		parsing <- struct{}{}
	}
	defer func() {
		for range cap(parsing) {
			<-parsing
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := Parse(ctx, []byte("services:\n  web:\n    image: nginx:1.27\n"), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Parse with every turn taken = %v, want it to stop waiting", err)
	}
}

// A file may say what Dockwell does anyway, as an export does, and nothing else
// about where logs go.
func TestComposeLoggingTakesRotationAlone(t *testing.T) {
	for file, want := range map[string]string{
		"services: {web: {image: a, logging: {driver: json-file, options: {max-size: 10m, max-file: \"5\", compress: \"true\"}}}}": "",
		"services: {web: {image: a, logging: {options: {max-size: 10m}}}}":                                                         "",
		"services: {web: {image: a, logging: {driver: syslog}}}":                                                                   "the service web logs to syslog",
		"services: {web: {image: a, logging: {driver: json-file, options: {labels: a}}}}":                                          "the service web sets the logging option labels",
		"services: {web: {image: a, logging: {drive: json-file}}}":                                                                 "the service web's logging uses drive",
		"services: {web: {image: a, logging: nonsense}}":                                                                           "logging",
	} {
		_, err := Parse(t.Context(), []byte(file), nil)
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", file, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s = %v, want it refused, saying %s", file, err, want)
		}
	}
}
