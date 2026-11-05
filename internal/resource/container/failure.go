// SPDX-License-Identifier: AGPL-3.0-only

package container

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// These say what happened on this server, and what to do about it, and stop
// there. What happens next isn't the driver's to say: the engine rolls a
// failed change back, and only it knows what runs once it has.

// What Docker and its registries say when a pull fails. They word it
// differently from one registry and version to the next, and these words are
// all they have to go on.
var (
	pullProxy    = phrases("proxy authentication required", "proxyconnect")
	pullLimited  = phrases("toomanyrequests", "rate limit", "429 too many requests")
	pullDenied   = phrases("pull access denied", "access to the resource is denied", "unauthorized", "authentication required", "no basic auth credentials", "denied:", "403 forbidden")
	pullMissing  = phrases("manifest unknown", "not found: manifest", "no such manifest", "name unknown", "repository name not known", "manifest for")
	pullPlatform = phrases("no matching manifest", "does not match the specified platform", "no match for platform")
	pullUntrust  = phrases("x509:", "tls: failed to verify", "certificate verify failed")
	pullNetwork  = phrases("no such host", "i/o timeout", "dial tcp", "connection refused", "connection reset",
		"tls handshake", "client.timeout", "server misbehaving", "network is unreachable", "context deadline exceeded")
	pullRegistryTrouble = phrases("unexpected http status: 5", "500 internal server error", "502 bad gateway", "503 service unavailable", "504 gateway timeout", "unexpected eof")
	noSpace             = phrases("no space left on device")
)

// endsWith reports a message that ends with one of the words, as a registry's
// answer often does: ": denied", ": EOF", ": not found".
func endsWith(message string, words ...string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	for _, w := range words {
		if strings.HasSuffix(message, ": "+w) || message == w {
			return true
		}
	}
	return false
}

// What Docker says when a port is taken, as it has worded it from one version
// to the next: "Bind for 0.0.0.0:8080 failed: port is already allocated",
// "listen tcp4 0.0.0.0:8080: bind: address already in use", "failed to bind
// host port for 0.0.0.0:8080:172.17.0.2:80/tcp: address already in use",
// "failed to bind host port 8080 for 0.0.0.0:8080:172.17.0.2:80/tcp: …", and
// "failed to bind host port 0.0.0.0:8080/tcp: …".
var (
	portTaken = phrases("port is already allocated", "address already in use")
	portMark  = regexp.MustCompile(`(?:Bind for|listen (?:tcp|udp)[46]?|host port(?: for)?) (\S+)`)
	portAfter = regexp.MustCompile(`^(?:\d+\.\d+\.\d+\.\d+|\[[^\]]*\]):(\d+)`)
	portLast  = regexp.MustCompile(`:(\d+)$`)
	portOnly  = regexp.MustCompile(`^\d+$`)
)

// portIn finds the port on the server a message is about, or nothing. An
// address comes before the port, and an IPv6 one may have no brackets, as in
// ::1:8080, where the port is what follows the last colon.
func portIn(message string) string {
	m := portMark.FindStringSubmatch(message)
	if m == nil {
		return ""
	}
	token, _, _ := strings.Cut(strings.TrimRight(m[1], ":"), "/")
	if portOnly.MatchString(token) {
		return token
	}
	if p := portAfter.FindStringSubmatch(token); p != nil {
		return p[1]
	}
	if p := portLast.FindStringSubmatch(token); p != nil {
		return p[1]
	}
	return ""
}

// What Docker says when a mount fails, and where it says the path is.
var (
	mountFailed = phrases("error while creating mount source path", "error while mounting volume",
		"failed to mount local volume", "invalid mount config", "bind source path does not exist",
		"error mounting", "are you trying to mount a directory onto a file")
	mountMismatch = phrases("are you trying to mount a directory onto a file", "not a directory")
	mountPath     = []*regexp.Regexp{
		regexp.MustCompile(`mount source path '([^']+)'`),
		regexp.MustCompile(`bind source path does not exist: (\S+)`),
		regexp.MustCompile(`error mounting "([^"]+)"`),
	}
)

// What the runtime says when a container's command can't run: its binary is
// for another architecture, isn't in the image, or can't be run.
var (
	wrongArchitecture = phrases("exec format error")
	commandGone       = regexp.MustCompile(`exec: "([^"]+)": (?:executable file not found|stat [^:]*: no such file or directory|no such file or directory)`)
	commandNotFound   = phrases("executable file not found")
	commandDenied     = regexp.MustCompile(`exec: "([^"]+)": permission denied`)
)

// phrases reports whether a message has any of the phrases, in any case.
func phrases(list ...string) func(string) bool {
	return func(message string) bool {
		message = strings.ToLower(message)
		for _, p := range list {
			if strings.Contains(message, p) {
				return true
			}
		}
		return false
	}
}

// wrappers are what Docker and the runtime put before what happened.
var wrappers = []string{
	"failed to set up container networking: ",
	"failed to create task for container: ",
	"failed to create shim task: ",
	"OCI runtime create failed: ",
	"runc create failed: ",
	"unable to start container process: ",
	"error during container init: ",
}

// dockerSays is what Docker said of a failure, whole, as a clause: without
// the wrappers it puts before what happened, or containerd's ": unknown"
// after it, which is a code, and with no full stop to end on.
func dockerSays(message string) string {
	s := strings.TrimSpace(message)
	s = strings.TrimSpace(strings.TrimSuffix(s, ": unknown"))
	for {
		before := s
		for _, w := range wrappers {
			s = strings.TrimPrefix(s, w)
		}
		if s == before {
			return strings.TrimRight(s, ".")
		}
	}
}

// messageOf is what Docker itself said, leaving out what wraps it here.
func messageOf(err error) string {
	var d *docker.Error
	if errors.As(err, &d) {
		return d.Message
	}
	return err.Error()
}

// mountReason is a failed mount's reason, for a message that may have had a
// mount's options in it: only what is after its last colon, which is what
// docker.Error leaves of them. Empty when that says nothing.
func mountReason(message string) string {
	s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(message), ": unknown"))
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	s = strings.TrimRight(s, ".")
	if strings.Contains(s, "mount") || s == "unknown" {
		return ""
	}
	return s
}

// pullFailure says why an image can't be pulled, and says so if it can't tell.
func pullFailure(ref string, err error) error {
	message := messageOf(err)
	var plain, reason string
	switch {
	case pullPlatform(message):
		reason = resource.ReasonImage
		plain = ref + " has no build for this server's platform, so it can't be pulled. Choose another tag or image, then try again."
	case pullProxy(message):
		plain = fmt.Sprintf("The proxy this server pulls through refused %s. Check Docker's proxy settings on this server, then try again.", ref)
	case pullLimited(message):
		plain = fmt.Sprintf("The registry is limiting pulls from this server, so %s can't be pulled for now. Wait a while, then try again.", ref)
	case pullDenied(message), endsWith(message, "denied"):
		reason = resource.ReasonImage
		plain = ref + " can't be pulled. The name may be wrong, or it may be private, which Dockwell can't pull yet. Check the name, then try again."
	case pullMissing(message), endsWith(message, "not found"):
		reason = resource.ReasonImage
		plain = ref + " isn't on its registry, so it can't be pulled. The tag may be wrong. Check the name and tag, then try again."
	case noSpace(message):
		plain = fmt.Sprintf("This server is out of disk space, so %s can't be pulled. Free some, such as with docker image prune, which removes only images no tag points to, then try again.", ref)
	case pullUntrust(message):
		plain = fmt.Sprintf("This server doesn't trust the certificate of the registry %s is on, so it can't be pulled. Check the server's clock and its CA certificates, then try again.", ref)
	case pullNetwork(message):
		plain = fmt.Sprintf("This server can't reach the registry %s is on, so it can't be pulled. Check the server's network and DNS, then try again.", ref)
	case pullRegistryTrouble(message), endsWith(message, "eof"):
		plain = fmt.Sprintf("The registry %s is on didn't answer properly, so it can't be pulled. That is usually the registry's own trouble. Try again in a while.", ref)
	default:
		plain = fmt.Sprintf("%s can't be pulled, and Dockwell doesn't recognize why. Docker says: %s. Look at what it says, then try again.", ref, dockerSays(message))
	}
	return resource.PlainlyBecause(reason, plain, fmt.Errorf("can't pull %s: %w", ref, err))
}

// startFailure says why a container at t didn't start, from what Docker said
// as it was made or started, and says so if it can't tell. It names what
// failed, and leaves out whether the app runs: the engine rolls it back.
func startFailure(t Target, err error) error {
	message := messageOf(err)
	switch {
	case portTaken(message):
		what := "A port " + t.Service + " publishes"
		if port := portIn(message); port != "" {
			what = "Port " + port
		}
		return resource.PlainlyBecause(resource.ReasonPort, what+" is already in use on this server. Choose another port, or stop what uses it, then try again.", err)
	case mountFailed(message):
		what := "one of its volumes or paths"
		for _, re := range mountPath {
			if m := re.FindStringSubmatch(message); m != nil {
				what = m[1]
				break
			}
		}
		if mountMismatch(message) {
			return resource.PlainlyBecause(resource.ReasonMount, fmt.Sprintf("%s can't mount %s, as one side of it is a file and the other a directory. Check the path on this server, then try again.", t.Service, what), err)
		}
		why := mountReason(message)
		if why == what {
			why = "it doesn't exist" // "bind source path does not exist: /srv/gone" ends in the path
		}
		if why != "" {
			return resource.PlainlyBecause(resource.ReasonMount, fmt.Sprintf("%s can't mount %s (%s). Check the path on this server, then try again.", t.Service, what, why), err)
		}
		return resource.PlainlyBecause(resource.ReasonMount, fmt.Sprintf("%s can't mount %s. Check the path on this server, then try again.", t.Service, what), err)
	case wrongArchitecture(message):
		return resource.PlainlyBecause(resource.ReasonImage, fmt.Sprintf("The image for %s isn't built for this server's architecture. Choose a build for this server, or another image, then try again.", t.Service), err)
	}
	if m := commandGone.FindStringSubmatch(message); m != nil {
		return resource.PlainlyBecause(resource.ReasonCommand, fmt.Sprintf("The command %s isn't in the image for %s. Check the command and the image, then try again.", m[1], t.Service), err)
	}
	if commandNotFound(message) {
		return resource.PlainlyBecause(resource.ReasonCommand, fmt.Sprintf("The command for %s isn't in its image. Check the command and the image, then try again.", t.Service), err)
	}
	if m := commandDenied.FindStringSubmatch(message); m != nil {
		return resource.PlainlyBecause(resource.ReasonCommand, fmt.Sprintf("The command %s can't run in the image for %s, as it isn't executable there. Check the command and the image, then try again.", m[1], t.Service), err)
	}
	says := strings.ReplaceAll(dockerSays(message), t.Name, t.Service)
	return resource.Plainly(fmt.Sprintf("%s didn't start, and Dockwell doesn't recognize why. Docker says: %s. Look at what it says, then try again.", t.Service, says), err)
}

// explained is what running a container at t failed with, in plain words. A
// message of Dockwell's own, about what the server has, reads as it is; so
// does Docker not answering, which says how to fix it. Time running out here,
// which an agent may answer with before the control plane's own deadline,
// says what ran out.
func explained(t Target, err error) error {
	var plain *resource.Failure
	if err == nil || errors.As(err, &plain) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return resource.Plainly(fmt.Sprintf("The step's time ran out before %s started, so Dockwell stopped waiting. A long pull may have used it up, so try again.", t.Service), err)
	}
	if docker.IsUnavailable(err) {
		return asIs(err)
	}
	var d *docker.Error
	if errors.As(err, &d) {
		return startFailure(t, err)
	}
	return asIs(err)
}

// asIs takes a message that is whole already as the plain words, in a
// sentence, as it starts lowercase as Go errors do.
func asIs(err error) error {
	message := strings.TrimRight(strings.TrimSpace(err.Error()), ".")
	return resource.Plainly(capitalize(message)+".", err)
}

func capitalize(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if size == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}
