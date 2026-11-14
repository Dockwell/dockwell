// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/docker/dockertest"
	"github.com/dockwell/dockwell/internal/registry"
	"github.com/dockwell/dockwell/internal/resource"
)

// What a person reads when an image won't pull, from what Docker and its
// registries really say, across moby's versions and its containerd image
// store. Each says what happened and what to do, and a fix is only offered
// when it is the right one: a registry's 503 isn't a wrong tag.
func TestPullFailuresInPlainWords(t *testing.T) {
	const ref = "acme/web:1.0"
	denied := ref + " can't be pulled. The name may be wrong, or the image may be private. Check the name, or save a registry login for docker.io, then try again."
	missing := ref + " isn't on its registry, so it can't be pulled. The tag may be wrong. Check the name and tag, then try again."
	network := "This server can't reach the registry " + ref + " is on, so it can't be pulled. Check the server's network and DNS, then try again."
	limited := "The registry is limiting pulls from this server, so " + ref + " can't be pulled for now. Wait a while, then try again."
	trouble := "The registry " + ref + " is on didn't answer properly, so it can't be pulled. That is usually the registry's own trouble. Try again in a while."
	proxy := "The proxy this server pulls through refused " + ref + ". Check Docker's proxy settings on this server, then try again."
	for said, want := range map[string]string{
		"pull access denied for acme/web, repository does not exist or may require 'docker login': denied: requested access to the resource is denied": denied,
		"pull access denied for acme/web, repository does not exist or may require 'docker login'":                                                     denied,
		"unauthorized: authentication required":                                                                                                            denied,
		`Head "https://ghcr.io/v2/acme/web/manifests/1.0": denied`:                                                                                         denied,
		`Head "https://ghcr.io/v2/acme/web/manifests/1.0": unauthorized`:                                                                                   denied,
		"manifest for acme/web:1.0 not found: manifest unknown: manifest unknown":                                                                          missing,
		`failed to resolve reference "docker.io/acme/web:1.0": docker.io/acme/web:1.0: not found`:                                                          missing,
		"no matching manifest for linux/arm64/v8 in the manifest list entries":                                                                             ref + " has no build for this server's platform, so it can't be pulled. Choose another tag or image, then try again.",
		"no matching manifest for linux/arm64/v8 in the manifest list entries: no match for platform in manifest: not found":                               ref + " has no build for this server's platform, so it can't be pulled. Choose another tag or image, then try again.",
		`Get "https://registry-1.docker.io/v2/": dial tcp: lookup registry-1.docker.io: no such host`:                                                      network,
		`Get "https://registry-1.docker.io/v2/": net/http: request canceled while waiting for connection (Client.Timeout exceeded while awaiting headers)`: network,
		// Docker Hub's rate limit, in both its wordings, which read as a URL if only the end of them is kept.
		"toomanyrequests: You have reached your pull rate limit. You may increase the limit by authenticating and upgrading: https://www.docker.com/increase-rate-limit": limited,
		"toomanyrequests: You have reached your unauthenticated pull rate limit. https://www.docker.com/increase-rate-limit":                                             limited,
		// A registry having a bad day isn't a wrong name or tag.
		"received unexpected HTTP status: 503 Service Unavailable":                 trouble,
		`Head "https://registry-1.docker.io/v2/library/nginx/manifests/1.27": EOF`: trouble,
		"unexpected EOF": trouble,
		`Get "https://registry-1.docker.io/v2/": x509: certificate signed by unknown authority`: "This server doesn't trust the certificate of the registry " + ref + " is on, so it can't be pulled. Check the server's clock and its CA certificates, then try again.",
		// A proxy asking for a password isn't a private image.
		`Get "https://registry-1.docker.io/v2/": Proxy Authentication Required`:                                         proxy,
		`Get "https://registry-1.docker.io/v2/": proxyconnect tcp: dial tcp 10.0.0.9:3128: connect: connection refused`: proxy,
		// Offering a prune says what it removes, and only removes what no tag points to.
		"failed to register layer: write /usr/bin/x: no space left on device": "This server is out of disk space, so " + ref + " can't be pulled. Free some, such as with docker image prune, which removes only images no tag points to, then try again.",
		// What it can't place, it says so, with what Docker said beside it.
		"the moon: is in the way.": ref + " can't be pulled, and Dockwell doesn't recognize why. Docker says: the moon: is in the way. Look at what it says, then try again.",
	} {
		plain, detail := resource.Explain(pullFailure(t.Context(), ref, errors.New(said)))
		if plain != want {
			t.Errorf("%s\n reads %q\n want  %q", said, plain, want)
		}
		if !strings.HasSuffix(detail, said) {
			t.Errorf("%s left %q behind it, want all of it", said, detail)
		}
	}
}

// The port a message is about, in every way Docker has worded it, with an
// address before it that may be IPv6, with or without brackets.
func TestPortInMessages(t *testing.T) {
	for said, want := range map[string]string{
		"driver failed programming external connectivity on endpoint x (y): Bind for 0.0.0.0:8080 failed: port is already allocated": "8080",
		"driver failed programming external connectivity on endpoint x (y): Bind for :::8080 failed: port is already allocated":      "8080",
		"driver failed programming external connectivity on endpoint x (y): Bind for ::1:8080 failed: port is already allocated":     "8080",
		"Error starting userland proxy: listen tcp4 0.0.0.0:8443: bind: address already in use":                                      "8443",
		"Error starting userland proxy: listen udp [::]:53: bind: address already in use":                                            "53",
		// moby 28
		"failed to set up container networking: failed to bind host port for 0.0.0.0:9000:172.17.0.2:80/tcp: address already in use":      "9000",
		"failed to set up container networking: failed to bind host port 8081 for 0.0.0.0:8081:172.17.0.2:80/tcp: address already in use": "8081",
		"failed to bind host port for [::1]:9001:[fd00::2]:80/tcp: address already in use":                                                "9001",
		// moby 29
		"failed to set up container networking: driver failed programming external connectivity on endpoint e (a1b2): failed to bind host port 0.0.0.0:8082/tcp: address already in use": "8082",
		"port is already allocated": "",
	} {
		if got := portIn(said); got != want {
			t.Errorf("%s: port %q, want %q", said, got, want)
		}
	}
}

// The same for what Docker says as a container starts. They name what failed,
// and leave out whether the app runs now, which is the engine's to say once it
// has rolled the change back. Nothing a person reads has the long name a
// service's container has in Docker.
func TestStartFailuresInPlainWords(t *testing.T) {
	long := "env_01m6mccc-web-1"
	at := Target{Name: long, Service: "web", Scoped: true}
	inUse := " is already in use on this server. Choose another port, or stop what uses it, then try again."
	for said, want := range map[string]string{
		"driver failed programming external connectivity on endpoint " + long + " (a1b2): Bind for 0.0.0.0:8080 failed: port is already allocated":                                                  "Port 8080" + inUse,
		"failed to set up container networking: driver failed programming external connectivity on endpoint " + long + " (a1b2): failed to bind host port 0.0.0.0:8080/tcp: address already in use": "Port 8080" + inUse,
		"port is already allocated": "A port web publishes" + inUse,
		"error while creating mount source path '/srv/uploads': mkdir /srv/uploads: read-only file system": "web can't mount /srv/uploads (read-only file system). Check the path on this server, then try again.",
		"invalid mount config for type \"bind\": bind source path does not exist: /srv/gone":               "web can't mount /srv/gone (it doesn't exist). Check the path on this server, then try again.",
		"error while mounting volume '': failed to mount local volume: permission denied":                  "web can't mount one of its volumes or paths (permission denied). Check the path on this server, then try again.",
		// All that is left of a mount once docker.Error has taken out its options.
		"failed to create task: error while mounting volume '/x': failed to mount local volume: unknown": "web can't mount one of its volumes or paths. Check the path on this server, then try again.",
		`failed to create task for container: failed to create shim task: OCI runtime create failed: runc create failed: unable to start container process: error during container init: error mounting "/srv/app.conf" to rootfs at "/etc/app.conf": not a directory: unknown: Are you trying to mount a directory onto a file (or vice-versa)? Check if the specified host path exists and is the expected type`: "web can't mount /srv/app.conf, as one side of it is a file and the other a directory. Check the path on this server, then try again.",
		// Each of these reads wrongly as "its command isn't in the image".
		`OCI runtime create failed: runc create failed: unable to start container process: exec: "serve": executable file not found in $PATH: unknown`: "The command serve isn't in the image for web. Check the command and the image, then try again.",
		`OCI runtime create failed: unable to start container process: exec: "/entry.sh": stat /entry.sh: no such file or directory: unknown`:          "The command /entry.sh isn't in the image for web. Check the command and the image, then try again.",
		`OCI runtime create failed: runc create failed: unable to start container process: exec: "/entry.sh": permission denied: unknown`:              "The command /entry.sh can't run in the image for web, as it isn't executable there. Check the command and the image, then try again.",
		// An image for another architecture, which a fleet of x86 and ARM servers will hit.
		"OCI runtime create failed: runc create failed: unable to start container process: exec format error: unknown": "The image for web isn't built for this server's architecture. Choose a build for this server, or another image, then try again.",
		// What it can't place, it says so, whole, without the runtime's wrappers or its code.
		`failed to create task for container: failed to create shim task: OCI runtime create failed: runc create failed: unable to start container process: error during container init: unable to apply apparmor profile: apparmor failed to apply profile: write /proc/self/attr/apparmor/exec: no such file or directory: unknown`: "web didn't start, and Dockwell doesn't recognize why. Docker says: unable to apply apparmor profile: apparmor failed to apply profile: write /proc/self/attr/apparmor/exec: no such file or directory. Look at what it says, then try again.",
		`OCI runtime create failed: runc create failed: unable to start container process: error during container init: open /proc/sys/net/ipv4/ip_unprivileged_port_start: permission denied: unknown`:                                                                                                                               "web didn't start, and Dockwell doesn't recognize why. Docker says: open /proc/sys/net/ipv4/ip_unprivileged_port_start: permission denied. Look at what it says, then try again.",
		`Conflict. The container name "/` + long + `" is already in use by container "abc".`: `web didn't start, and Dockwell doesn't recognize why. Docker says: Conflict. The container name "/web" is already in use by container "abc". Look at what it says, then try again.`,
		"the moon is in the way": "web didn't start, and Dockwell doesn't recognize why. Docker says: the moon is in the way. Look at what it says, then try again.",
	} {
		plain, _ := resource.Explain(startFailure(at, errors.New(said)))
		if plain != want {
			t.Errorf("%s\n reads %q\n want  %q", said, plain, want)
		}
		for _, leak := range []string{"env_", ": unknown", "so web can't", "isn't running"} {
			if strings.Contains(plain, leak) {
				t.Errorf("%s reads %q, which says %q", said, plain, leak)
			}
		}
	}
}

// What running a container fails with is always in plain words: Docker's own,
// and Dockwell's about what the server has. Words already said stay. A wait that
// ended says which, from the step's context: the step's time, a wait inside it with
// the step's time left, or the step called off (#612).
func TestRunFailuresAreExplained(t *testing.T) {
	at := Target{Name: "env_01m6mccc-web-1", Service: "web", Scoped: true}
	expired, cancelExpired := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancelExpired()
	<-expired.Done()
	calledOff, callOff := context.WithCancel(t.Context())
	callOff()
	alive := t.Context()
	timedOut := fmt.Errorf("can't read it: %w", context.DeadlineExceeded)
	for _, tt := range []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{"a port in use", alive, &docker.Error{Status: 500, Message: "Bind for 0.0.0.0:80 failed: port is already allocated"}, "Port 80 is already in use on this server. Choose another port, or stop what uses it, then try again."},
		{"Dockwell's own", alive, errors.New("the network back isn't on this server, so web can't join it. Add it to the environment"), "The network back isn't on this server, so web can't join it. Add it to the environment."},
		{"said already", expired, resource.Plainly("Said already.", errors.New("raw")), "Said already."},
		// A step's time running out, as an agent may answer before the control plane's own deadline.
		{"the step's time", expired, context.DeadlineExceeded, "The step's time ran out before web started, so Dockwell stopped waiting. A long pull may have used it up, so try again."},
		{"the step's time, wrapped", expired, timedOut, "The step's time ran out before web started, so Dockwell stopped waiting. A long pull may have used it up, so try again."},
		{"a wait inside, with the step's time left", alive, timedOut, "web didn't start, as a wait on this server ran out with the step's own time left: can't read it: context deadline exceeded. Check what web waits on, such as Docker, on this server, then try again."},
		{"a wait inside, saying no more", alive, context.DeadlineExceeded, "web didn't start, as a wait on this server ran out with the step's own time left. Check what web waits on, such as Docker, on this server, then try again."},
		{"called off", calledOff, fmt.Errorf("can't read it: %w", context.Canceled), "Dockwell stopped starting web, as the step was called off: its change was cancelled, the control plane stopped waiting, or the agent stopped. Run the change again if you still want it."},
	} {
		if plain, _ := resource.Explain(explained(tt.ctx, at, tt.err)); plain != tt.want {
			t.Errorf("%s: %v reads %q, want %q", tt.name, tt.err, plain, tt.want)
		}
	}
	if err := explained(alive, at, nil); err != nil {
		t.Errorf("no failure reads %v", err)
	}
}

// Docker not answering in time, with the step's own time left, says so in Docker's
// words, with how to check it, never as the step's time running out, as #606's
// Docker run read of a step 30 s into its 10 min (#612).
func TestDockerNotAnsweringWithTheStepsTimeLeftSaysSo(t *testing.T) {
	e := dockertest.New(t)
	e.Intercept(func(r *http.Request) error {
		<-r.Context().Done()
		return r.Context().Err()
	})
	d := &Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond}
	step, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	wait, stop := context.WithTimeout(resource.WithStep(step), 100*time.Millisecond)
	defer stop()
	err := d.Run(wait, Target{Name: "env_01-web-1", Project: "env_01", Service: "web", Scoped: true}, &resourcev1.Container{Image: "web:1"}, resource.Decision{})
	plain, _ := resource.Explain(err)
	if !strings.HasPrefix(plain, "Docker isn't answering on "+e.Socket) || strings.Contains(plain, "The step's time") {
		t.Errorf("reads %q, want Docker's words", plain)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to stay a deadline", err)
	}
}

// A pull the step's time cuts short says so, and its kind stays with it, for
// the agent to pass on.
func TestAPullCutShortReadsAsTimeRunningOut(t *testing.T) {
	e := dockertest.New(t)
	e.Registry["big:1"] = dockertest.Image{Size: 48 << 20, PullTime: 3 * time.Second, Config: docker.Config{Cmd: []string{"serve"}}}
	d := &Docker{Client: e.Client(), Settle: time.Millisecond, Poll: time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	err := d.Run(ctx, Target{Name: "env_01-web-1", Project: "env_01", Service: "web", Scoped: true}, &resourcev1.Container{Image: "big:1"}, resource.Decision{})
	plain, _ := resource.Explain(err)
	if plain != "The step's time ran out before web started, so Dockwell stopped waiting. A long pull may have used it up, so try again." {
		t.Errorf("reads %q", plain)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to stay a deadline", err)
	}
}

// What a failure comes down to goes with it, so whoever shows it can say what to
// do about it where they are. What Dockwell can't place comes down to nothing.
func TestFailuresSayWhatTheyComeDownTo(t *testing.T) {
	at := Target{Name: "env_01-web-1", Service: "web", Scoped: true}
	for said, want := range map[string]string{
		"pull access denied for acme/web, repository does not exist or may require 'docker login'": resource.ReasonImage,
		"manifest for acme/web:1.0 not found: manifest unknown: manifest unknown":                  resource.ReasonImage,
		"no matching manifest for linux/arm64/v8 in the manifest list entries":                     resource.ReasonImage,
		// The name is fine when the network or the registry is what failed, and the
		// registry's own trouble says so, as the same pull may work in a while.
		`Get "https://registry-1.docker.io/v2/": dial tcp: lookup registry-1.docker.io: no such host`: "",
		"received unexpected HTTP status: 503 Service Unavailable":                                    resource.ReasonRegistry,
		"received unexpected HTTP status: 502 Bad Gateway":                                            resource.ReasonRegistry,
		"unexpected EOF": resource.ReasonRegistry,
		`Get "https://registry-1.docker.io/v2/library/nginx/manifests/1.27": EOF`: resource.ReasonRegistry,
		// A limit, a certificate and a refusal aren't the registry's trouble, and a
		// retry won't help them.
		"toomanyrequests: You have reached your pull rate limit.":                               "",
		`Get "https://registry.example.com/v2/": x509: certificate signed by unknown authority`: "",
		"denied: requested access to the resource is denied":                                    resource.ReasonImage,
		"unauthorized: authentication required":                                                 resource.ReasonImage,
		"the moon is in the way":                                                                "",
	} {
		if got := resource.ReasonOf(pullFailure(t.Context(), "acme/web:1.0", errors.New(said))); got != want {
			t.Errorf("pull: %s comes down to %q, want %q", said, got, want)
		}
	}
	for said, want := range map[string]string{
		"Bind for 0.0.0.0:8080 failed: port is already allocated":                                                resource.ReasonPort,
		"error while creating mount source path '/srv/uploads': mkdir /srv/uploads: read-only file system":       resource.ReasonMount,
		`OCI runtime create failed: unable to start container process: exec: "serve": executable file not found`: resource.ReasonCommand,
		`OCI runtime create failed: unable to start container process: exec: "/entry.sh": permission denied`:     resource.ReasonCommand,
		"OCI runtime create failed: unable to start container process: exec format error":                        resource.ReasonImage,
		"the moon is in the way": "",
	} {
		if got := resource.ReasonOf(startFailure(at, errors.New(said))); got != want {
			t.Errorf("start: %s comes down to %q, want %q", said, got, want)
		}
	}
}

// A pull that signed in with a registry login, which the registry refused, names
// the login, and never says to save one (ADR 0086).
func TestAPullWithALoginNamesIt(t *testing.T) {
	ctx := registry.WithAuth(t.Context(), registry.Auth{Name: "ghcr-ci", Registry: "ghcr.io", Username: "ci", Password: "s3cret-token"})
	plain, detail := resource.Explain(pullFailure(ctx, "ghcr.io/acme/web:1.0", errors.New("denied: requested access to the resource is denied")))
	want := "ghcr.io/acme/web:1.0 can't be pulled with the registry login ghcr-ci. The name may be wrong, or the login may not read it. Check the name and the login, then try again."
	if plain != want {
		t.Errorf("reads %q, want %q", plain, want)
	}
	if strings.Contains(plain+detail, "s3cret-token") {
		t.Error("the words hold the login's password")
	}
	plain, _ = resource.Explain(pullFailure(t.Context(), "ghcr.io/acme/web:1.0", errors.New("denied: requested access to the resource is denied")))
	if !strings.Contains(plain, "save a registry login for ghcr.io") {
		t.Errorf("without a login, %q doesn't say which registry a login would be for", plain)
	}
}
