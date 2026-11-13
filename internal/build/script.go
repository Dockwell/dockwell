// SPDX-License-Identifier: AGPL-3.0-only

package build

import (
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/dockwell/dockwell/internal/resource/service"
)

// What a builder says beside its output, on standard error, each on a line of its
// own that starts with marker: where it is ("phase fetch", "phase build"), the
// commit it checked out ("commit <author> <subject>", each in base64), and what
// stopped it before BuildKit began ("fail fetch", "fail context",
// "fail dockerfile", "fail buildkitd").
const marker = "dockwell:"

// Where things are in a builder.
const (
	cacheMount   = "/var/lib/buildkit"
	secretsMount = "/run/dockwell-secrets" //nolint:gosec // a path, not a credential
	// planMount holds the plan Railpack wrote when it read the commit, read-only.
	planMount = "/run/dockwell-plan"
	planFile  = "railpack-plan.json"
	// registriesMount holds the registry logins a build's base images pull with, as
	// Docker's config.json, read-only (ADR 0086).
	registriesMount = "/run/dockwell-registries"
	sourceDir       = "/src"
	buildkitSock    = "unix:///run/buildkit/buildkitd.sock"
)

// fetchScript fetches the branch or tag the build names, by its full name, and checks
// out the commit only if the branch has it, so a commit someone names can't be one a
// fork pushed, as GitHub serves any commit of a fork through its parent's URL; a
// commit the build names as its ref is fetched as it is. It says what the commit is,
// and finds the build's folder, as ctx, and its Dockerfile, as df, empty when the
// commit hasn't one where the build says. Neither may lead out of the checkout.
const fetchScript = `say phase fetch
mkdir -p ` + sourceDir + ` && cd ` + sourceDir + `
git init -q
git remote add origin "$DW_URL"
on() { git merge-base --is-ancestor "$DW_COMMIT" "FETCH_HEAD^{commit}" 2>/dev/null; }
if [ "$DW_REF" = "$DW_COMMIT" ]; then
  if ! git fetch -q --depth 1 origin "$DW_COMMIT" 2>/tmp/fetch.log; then tail -n 5 /tmp/fetch.log >&2; say fail fetch; exit 3; fi
elif git fetch -q --depth 1 origin "${DW_REF:-HEAD}" 2>/tmp/fetch.log && on; then :
elif git fetch -q --depth 200 origin "${DW_REF:-HEAD}" 2>>/tmp/fetch.log && on; then :
elif git fetch -q --filter=blob:none --unshallow origin "${DW_REF:-HEAD}" 2>>/tmp/fetch.log && on; then :
elif git cat-file -e "FETCH_HEAD^{commit}" 2>/dev/null; then say fail notonref; exit 3
else tail -n 5 /tmp/fetch.log >&2; say fail fetch; exit 3
fi
git -c advice.detachedHead=false checkout -q "$DW_COMMIT"
say commit "$(git log -1 --format=%an | base64 | tr -d '\n')" "$(git log -1 --format=%s | base64 | tr -d '\n')"
ctx="` + sourceDir + `/$DW_CONTEXT"
if [ ! -d "$ctx" ]; then say fail context; exit 3; fi
ctx=$(cd "$ctx" && pwd -P)
case "$ctx" in ` + sourceDir + `|` + sourceDir + `/*) ;; *) say fail context; exit 3;; esac
df=""
if [ -f "$ctx/$DW_DOCKERFILE" ]; then
  dfdir=$(cd "$(dirname "$ctx/$DW_DOCKERFILE")" && pwd -P)
  case "$dfdir" in ` + sourceDir + `|` + sourceDir + `/*) df="$dfdir/$(basename "$DW_DOCKERFILE")";; esac
fi`

// AppArmorProfile is the profile each of a build's steps runs under where the
// server has it loaded: Docker's own for its containers, which Docker loads as
// it starts.
const AppArmorProfile = "docker-default"

// confineScript says what confines each of a build's steps beside seccomp, as aa:
// Docker's AppArmor profile, where the server's kernel has AppArmor on and has the
// profile loaded. The builder reads that itself, so every agent decides alike,
// root or not: an agent that isn't root can't read the server's profiles, and the
// builder, privileged, can, once it has mounted securityfs, which Docker gives no
// container. It says which it is, for the build's log.
//
// BuildKit refuses a profile ("AppArmor is not supported on this host") unless
// /sbin/apparmor_parser is there, which it takes as the sign that profiles can be
// loaded, and its image has none. It never runs it: the server's Docker loaded
// the profile, and runc changes to it by name through /proc. So an empty file
// stands in for it, in the builder's own filesystem, which goes with the builder.
const confineScript = `aa=""
if [ "$(cat /sys/module/apparmor/parameters/enabled 2>/dev/null)" = Y ]; then
  [ -d /sys/kernel/security/apparmor ] || mount -t securityfs securityfs /sys/kernel/security 2>/dev/null || true
  if grep -q '^` + AppArmorProfile + ` ' /sys/kernel/security/apparmor/profiles 2>/dev/null; then
    aa=` + AppArmorProfile + `
    [ -e /sbin/apparmor_parser ] || : >/sbin/apparmor_parser
    say apparmor "$aa"
  else
    say noapparmor
  fi
fi`

// script is what a builder runs, in BuildKit's image, with BusyBox's sh. It checks
// out the commit and says what it is. With the Dockerfile, which must be where the
// build says, it builds with BuildKit's Dockerfile frontend. With Railpack, it
// builds the plan Railpack wrote when the agent read the commit, unprivileged,
// which is in planMount, with Railpack's frontend: nothing of Railpack's runs here,
// outside BuildKit's sandbox (ADR 0046). It starts BuildKit through its image's
// own entrypoint, which gives it a cgroup namespace of its own where it needs one,
// and writes the image to standard out as Docker's image archive. Its arguments are
// buildctl's for the Dockerfile: build arguments, the target and secrets. Railpack
// takes its build arguments and secrets from files: a-NAME and s-NAME in the
// secrets' folder. Every value comes from the environment or a file, never from
// the script's words, so nothing in a build's settings runs as a command.
const script = `set -eu
say() { printf '` + marker + `%s\n' "$*" >&2; }
export GIT_TERMINAL_PROMPT=0 GIT_ASKPASS=/bin/false
` + fetchScript + `
sec=` + secretsMount + `
builder=dockerfile
if [ "$DW_BUILDER" = railpack ]; then
  builder=railpack
  if [ ! -s "` + planMount + `/` + planFile + `" ]; then say fail noplan; exit 3; fi
elif [ -z "$df" ]; then say fail dockerfile; exit 3
fi
say builder "$builder"
say phase start
mkdir -p /etc/buildkit
` + confineScript + `
{
  printf '[worker.oci]\n  gc = true\n  maxUsedSpace = "%s"\n' "$DW_KEEP"
  if [ -n "$aa" ]; then printf '  apparmor-profile = "%s"\n' "$aa"; fi
  printf '[worker.containerd]\n  enabled = false\n'
  if [ -n "${DW_PLAIN_REGISTRY:-}" ]; then printf '[registry."%s"]\n  http = true\n' "$DW_PLAIN_REGISTRY"; fi
} >/etc/buildkit/buildkitd.toml
/usr/bin/buildkitd-entrypoint --addr ` + buildkitSock + ` --config /etc/buildkit/buildkitd.toml >/tmp/buildkitd.log 2>&1 &
bk=$!
i=0
until buildctl --addr ` + buildkitSock + ` debug workers >/dev/null 2>&1; do
  i=$((i+1))
  if [ $i -gt 150 ] || ! kill -0 $bk 2>/dev/null; then tail -n 5 /tmp/buildkitd.log >&2; say fail buildkitd; exit 3; fi
  sleep 0.2
done
if [ -n "${DW_PRUNE_FIRST:-}" ]; then buildctl --addr ` + buildkitSock + ` prune --keep-storage "$DW_KEEP_MB" >/dev/null 2>&1 || true; fi
say phase build
set +e
if [ "$builder" = railpack ]; then
  set -- --opt build-arg:cache-key="$DW_CACHE_KEY" --opt build-arg:secrets-hash="$DW_SECRETS_HASH"
  for f in "$sec"/a-* "$sec"/s-*; do [ -f "$f" ] || continue; n=${f##*/}; set -- "$@" --secret "id=${n#?-},src=$f"; done
  buildctl --addr ` + buildkitSock + ` build --progress rawjson --frontend gateway.v0 --opt source="$DW_RAILPACK_FRONTEND" \
    --local context="$ctx" --local dockerfile=` + planMount + ` --opt filename=` + planFile + ` \
    --output type=docker,name="$DW_IMAGE" "$@"
else
  buildctl --addr ` + buildkitSock + ` build --progress rawjson --frontend dockerfile.v0 \
    --local context="$ctx" --local dockerfile="$(dirname "$df")" --opt filename="$(basename "$df")" \
    --output type=docker,name="$DW_IMAGE" "$@"
fi
status=$?
set -e
say phase prune
if buildctl --addr ` + buildkitSock + ` prune --keep-storage "$DW_KEEP_MB" >/dev/null 2>&1; then say pruned; fi
kill $bk 2>/dev/null || true
wait $bk 2>/dev/null || true
exit $status
`

// env is a builder's environment, which says what it builds. None of it is
// secret: a build's secrets come as files.
func env(req service.BuildRequest, keep int64) []string {
	return []string{
		"DW_URL=" + req.URL,
		"DW_REF=" + req.Ref,
		"DW_COMMIT=" + req.Commit,
		"DW_CONTEXT=" + req.Context,
		"DW_DOCKERFILE=" + dockerfile(req.Dockerfile),
		"DW_IMAGE=" + req.Image,
		"DW_KEEP=" + strconv.FormatInt(keep, 10),
		"DW_KEEP_MB=" + strconv.FormatInt(keep>>20, 10),
		"DW_BUILDER=" + req.Builder,
		"DW_BUILD_CMD=" + req.BuildCommand,
		"DW_START_CMD=" + req.StartCommand,
		"DW_RAILPACK_FRONTEND=" + RailpackFrontend,
	}
}

// dockerfile is the Dockerfile's path in the context, "Dockerfile" when a build
// names none.
func dockerfile(p string) string {
	if p == "" {
		return "Dockerfile"
	}
	return p
}

// args are buildctl's arguments for a build, after what the script gives it:
// each build argument, the target, and each secret, by the file it is in. A build
// with registry logins sets the Dockerfile frontend's cmdline, which keeps it from
// handing the build to a frontend a syntax directive or BUILDKIT_SYNTAX names, and
// does nothing else (BuildKit v0.34.1, frontend/dockerfile/builder): it reads the
// Dockerfile itself, and only Docker's own frontend sees the logins (ADR 0086).
func args(req service.BuildRequest) []string {
	var out []string
	if len(req.Logins) > 0 {
		out = append(out, "--opt", "cmdline=")
	}
	for _, k := range slices.Sorted(mapsKeys(req.Args)) {
		out = append(out, "--opt", "build-arg:"+k+"="+req.Args[k])
	}
	if req.Target != "" {
		out = append(out, "--opt", "target="+req.Target)
	}
	for _, id := range slices.Sorted(mapsKeys(req.Secrets)) {
		out = append(out, "--secret", "id="+id+",src="+path.Join(secretsMount, secretFile(id)))
	}
	return out
}

// secretFile is the name of the file a secret is in, which an ID's own characters
// can't make into a path.
func secretFile(id string) string {
	return "s-" + strings.NewReplacer("/", "_", ".", "_").Replace(id)
}

func mapsKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
