// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
	v1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/delivery"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/id"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
	"github.com/dockwell/dockwell/internal/resource/host"
	"github.com/dockwell/dockwell/internal/secrets"
)

func TestReviewedImagePullOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	remote := startImageRegistry(ctx, t)
	const repo = "fixture/pull"
	target, next := remote.address+"/"+repo+":review", remote.address+"/"+repo+":next"

	// Register cleanup before creating aliases or asserting anything about the pull.
	var staged []string
	var pulledIDs []string
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, ref := range append([]string{target, next}, staged...) {
			if err := client.UntagImage(ctx, ref); err != nil {
				t.Errorf("remove fixture image reference %s: %v", ref, err)
			}
		}
		for _, id := range pulledIDs {
			if err := client.RemoveReviewedImage(ctx, id); err != nil && !docker.IsNotFound(err) {
				t.Errorf("remove fixture image %s: %v", id, err)
			}
		}
	})
	seed := func(ref, text string) {
		t.Helper()
		payload := writeCleanupPayload(t, t.TempDir(), ref+text)
		if err := client.ImportFiles(ctx, ref, []docker.ImageFile{{Name: "/payload", Path: payload, Mode: 0o600}}); err != nil {
			t.Fatalf("import fixture image %s: %v", ref, err)
		}
		image, err := client.InspectImage(ctx, ref)
		if err != nil {
			t.Fatalf("inspect fixture image %s: %v", ref, err)
		}
		pulledIDs = append(pulledIDs, image.ID)
		remote.run(ctx, t, "push", ref)
		if err := client.UntagImage(ctx, ref); err != nil {
			t.Fatalf("untag fixture image %s after pushing: %v", ref, err)
		}
	}
	seed(target, "first image")
	seed(next, "replacement image")

	c := cleanupCalls(t, client)
	c.cfg.images = newImages(client, ImageReads{}, time.Now)
	c.cfg.images.newClient = remote.client
	t.Cleanup(c.cfg.images.forget)
	var err error
	c.delivery, err = delivery.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	authenticated := registry.WithAuth(ctx, remote.auth)
	catalog, err := c.cfg.images.read(authenticated, &agentv1.ImageCall{Ask: &agentv1.ImageCall_Repositories{
		Repositories: &v1.ListRegistryRepositoriesRequest{Registry: remote.address},
	}})
	rows := catalog.GetRepositories().GetRepositories()
	if err != nil || len(rows) != 1 || rows[0].GetName() != repo {
		t.Fatalf("fixture registry catalog = %v, %v", rows, err)
	}
	info, err := client.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ref := resource.Ref{Kind: host.Kind, Scope: c.server, Name: host.Name}
	plan := func() *v1.DockerMaintenance {
		t.Helper()
		p := &v1.DockerMaintenance{ImageAction: &v1.DockerImageAction{
			Pull:           &v1.DockerImagePull{Reference: target},
			ApprovedLogins: []*v1.DockerImageLoginRef{{Name: remote.auth.Name, Host: remote.address}},
		}}
		if err := c.previewDockerImage(ctx, ref, p, sealImageFixture(t, c, ref, p, remote.auth)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	apply := func(p *v1.DockerMaintenance, protection *agentv1.DockerCleanupProtection) (*v1.DockerImageReceipt, error) {
		t.Helper()
		p.ChangeId = id.New(id.Change)
		return c.applyImageAction(ctx, ref, p, sealImageFixture(t, c, ref, p, remote.auth), protection)
	}
	pull := func(p *v1.DockerMaintenance, previous string) string {
		t.Helper()
		descriptor := p.GetImageAction().GetDescriptor_()
		engineID := descriptor.GetConfigDigest()
		if info.ContainerdImageStore() {
			engineID = descriptor.GetManifestDigest()
		}
		pulledIDs = append(pulledIDs, engineID)
		staged = append(staged, remote.address+"/"+repo+"@"+descriptor.GetManifestDigest())
		receipt, err := apply(p, &agentv1.DockerCleanupProtection{})
		if err != nil || receipt.GetOutcome() != v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_APPLIED ||
			receipt.GetImageKey().GetImageId() != engineID || receipt.GetPreviousImageId() != previous ||
			receipt.GetDigest() != descriptor.GetManifestDigest() {
			t.Fatalf("pull receipt = %v, %v", receipt, err)
		}
		return engineID
	}

	first := plan()
	if !first.GetImageAction().GetBindings()[0].GetExpectAbsent() {
		t.Fatal("the fixture wasn't cleared before the pull")
	}
	firstID := pull(first, "")
	if current := plan().GetImageAction(); !resource.DockerImageMatchesDigest(current.GetBindings()[0].GetLocal(), current.GetDescriptor_()) {
		t.Fatalf("pulled image isn't up to date: %v", current)
	}

	remote.moveManifest(ctx, t, repo, "next", "review")
	replacement := plan()
	action := replacement.GetImageAction()
	if !resource.DockerImageNeedsPasskey(action) || action.GetBindings()[0].GetImageId() != firstID {
		t.Fatalf("the replacement didn't review the existing binding: %v", action)
	}
	refused, err := apply(replacement, &agentv1.DockerCleanupProtection{Images: []string{target}})
	if err == nil || refused.GetReason() != v1.DockerImageReason_DOCKER_IMAGE_REASON_PROTECTED {
		t.Fatalf("protected alias was retargeted: %v, %v", refused, err)
	}
	if kept, err := client.InspectImage(ctx, target); err != nil || kept.ID != firstID {
		t.Fatalf("protected binding changed: %v, %v", kept, err)
	}
	secondID := pull(replacement, firstID)
	if image, err := client.InspectImage(ctx, target); err != nil || image.ID != secondID || secondID == firstID {
		t.Fatalf("replacement wasn't installed at its reviewed alias: %v, %v", image, err)
	}
	if _, err := client.InspectImage(ctx, firstID); err != nil {
		t.Fatalf("pulling a replacement discarded the previous image: %v", err)
	}
}

func TestReviewedTagMoveKeepsBothImagesOnRealDocker(t *testing.T) {
	client := dockertest.Real(t)
	c := cleanupCalls(t, client)
	repo := "docker.io/" + cleanupRealRepo(t)
	alias, other, next := repo+":move", repo+":keep", repo+":new"
	before := importCleanupImage(t, client, alias, "previous image")
	want := importCleanupImage(t, client, next, "next image")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, tag := range []string{alias, other} {
			if err := client.UntagImage(ctx, tag); err != nil {
				t.Errorf("remove fixture tag %s: %v", tag, err)
			}
		}
	})
	if err := client.TagImage(t.Context(), before, other); err != nil {
		t.Fatal(err)
	}

	ref := resource.Ref{Kind: host.Kind, Scope: "srv_cleanup", Name: host.Name}
	plan := &v1.DockerMaintenance{
		ImageAction: &v1.DockerImageAction{Tag: &v1.DockerImageTag{
			SourceKey:   &v1.DockerImageSourceKey{ServerId: ref.Scope, ImageId: want},
			Destination: alias, MoveExisting: true,
		}},
	}
	if err := c.previewDockerImage(t.Context(), ref, plan, nil); err != nil {
		t.Fatal(err)
	}
	plan.ChangeId = id.New(id.Change)
	receipt, err := c.applyImageAction(t.Context(), ref, plan, nil, &agentv1.DockerCleanupProtection{})
	if err != nil || receipt.GetOutcome() != v1.DockerImageOutcome_DOCKER_IMAGE_OUTCOME_APPLIED || receipt.GetPreviousImageId() != before {
		t.Fatalf("move receipt = %v, %v", receipt, err)
	}
	for name, expected := range map[string]string{alias: want, other: before, before: before, want: want} {
		if found, err := c.localImage(t.Context(), name); err != nil || imageID(found) != expected {
			t.Errorf("image %s = %s, %v; want %s", name, imageID(found), err, expected)
		}
	}
}

func sealImageFixture(t *testing.T, c *calls, ref resource.Ref, plan *v1.DockerMaintenance, auth registry.Auth) []byte {
	t.Helper()
	b, err := secrets.SealValues(c.delivery.Public(), c.server, resource.LoginRef(resource.DockerImageAuthRef(ref, plan)), auth.Values())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type imageRegistryFixture struct {
	address, socket, cliConfig string
	auth                       registry.Auth
	http                       *http.Client
}

func startImageRegistry(ctx context.Context, t *testing.T) *imageRegistryFixture {
	t.Helper()
	socket := os.Getenv(dockertest.EnvReal)
	if socket == "1" {
		socket = docker.DefaultSocket
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	defer l.Close()
	f := &imageRegistryFixture{
		address: address, socket: socket, cliConfig: t.TempDir(),
		auth: registry.Auth{Name: "fixture registry", Registry: address, Username: "fixture", Password: "fixture-registry-credential"},
	}
	// Docker's loopback exception allows it to connect to this throwaway
	// certificate. The agent-side client below explicitly trusts that cert.
	dir := t.TempDir()
	f.http = imageFixtureTLS(t, dir)
	hash, err := bcrypt.GenerateFromPassword([]byte(f.auth.Password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "htpasswd"), []byte(f.auth.Username+":"+string(hash)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(f.auth.Username + ":" + f.auth.Password))
	config, err := json.Marshal(map[string]any{"auths": map[string]any{address: map[string]string{"auth": encoded}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.cliConfig, "config.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	name := "dockwell-" + strings.ToLower(dockertest.Scope(t))
	registryImage := "registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if out, err := f.command(ctx, "rm", "-fv", name); err != nil && !strings.Contains(string(out), "No such container") {
			t.Errorf("remove fixture registry: %v: %s", err, out)
		}
	})
	f.run(ctx, t, "create", "--network", "host", "--name", name,
		"-e", "REGISTRY_HTTP_ADDR="+address,
		"-e", "REGISTRY_HTTP_TLS_CERTIFICATE=/tmp/fixture/cert.pem",
		"-e", "REGISTRY_HTTP_TLS_KEY=/tmp/fixture/key.pem",
		"-e", "REGISTRY_AUTH=htpasswd",
		"-e", "REGISTRY_AUTH_HTPASSWD_REALM=fixture",
		"-e", "REGISTRY_AUTH_HTPASSWD_PATH=/tmp/fixture/htpasswd", registryImage)
	// Copy through Engine so these files also reach the isolated dind daemon.
	f.run(ctx, t, "cp", dir, name+":/tmp/fixture")
	_ = l.Close()
	f.run(ctx, t, "start", name)
	client := f.client(registry.Options{})
	for {
		if err := client.Challenge(ctx, f.auth); err == nil {
			return f
		}
		select {
		case <-ctx.Done():
			logCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			out, _ := f.command(logCtx, "logs", name)
			cancel()
			t.Fatalf("fixture TLS registry did not start: %s", out)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func imageFixtureTLS(t *testing.T, dir string) *http.Client {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, public, private)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for file, data := range map[string][]byte{"cert.pem": certPEM, "key.pem": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})} {
		if err := os.WriteFile(filepath.Join(dir, file), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("could not trust the fixture certificate")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: registry.RequestTimeout}
}

func (f *imageRegistryFixture) command(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", append([]string{"--host", "unix://" + f.socket}, args...)...)
	cmd.Env = append(os.Environ(), "DOCKER_CONFIG="+f.cliConfig)
	return cmd.CombinedOutput()
}

func (f *imageRegistryFixture) run(ctx context.Context, t *testing.T, args ...string) {
	t.Helper()
	if out, err := f.command(ctx, args...); err != nil {
		t.Fatalf("docker %s: %v: %s", args[0], err, out)
	}
}

func (f *imageRegistryFixture) client(o registry.Options) *registry.Client {
	o.Insecure = func(string) bool { return false }
	o.Secure = f.http
	return registry.New(o)
}

// moveManifest changes the registry tag without changing the local Engine's binding.
func (f *imageRegistryFixture) moveManifest(ctx context.Context, t *testing.T, repo, from, to string) {
	t.Helper()
	request := func(method, tag, contentType string, body io.Reader) *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, "https://"+f.address+"/v2/"+repo+"/manifests/"+tag, body)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(f.auth.Username, f.auth.Password)
		req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json")
		req.Header.Set("Content-Type", contentType)
		res, err := f.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	old := request(http.MethodGet, from, "", nil)
	data, err := io.ReadAll(io.LimitReader(old.Body, registry.MaxBody+1))
	_ = old.Body.Close()
	if old.StatusCode != http.StatusOK || err != nil || len(data) > registry.MaxBody {
		t.Fatalf("read fixture manifest: HTTP %d, %v", old.StatusCode, err)
	}
	next := request(http.MethodPut, to, old.Header.Get("Content-Type"), bytes.NewReader(data))
	_ = next.Body.Close()
	if next.StatusCode != http.StatusCreated {
		t.Fatalf("move fixture manifest: HTTP %d", next.StatusCode)
	}
}
