# Installing Dockwell

One command installs Dockwell on a Linux server. Running it again upgrades in place, and `--uninstall` removes it without touching your workloads.

> `get.dockwell.dev` isn't set up yet. Until it is, use the script's address in this repository instead: `https://raw.githubusercontent.com/Dockwell/dockwell/main/scripts/install.sh`. The commands below work the same with either.

## Install the first server

Run this as root:

```sh
curl -fsSL https://get.dockwell.dev | sh
```

On Alpine, which comes without curl, use `wget -qO- https://get.dockwell.dev | sh`.

The first server becomes your control server. It runs the full build of `dockwell`: the control plane, which serves the panel and API, and an agent of its own. The panel listens on `127.0.0.1:7600`, so only the server itself can reach it. To open it from your computer, forward the port over SSH:

```sh
ssh -L 7600:127.0.0.1:7600 <user>@<your server>
```

The install ends with a one-time link, `http://localhost:7600/claim#…`. Open it through the tunnel to create the owner account with a passkey. It works once, for 24 h; `dockwell claim` on the server prints a new one. Open the panel at `localhost`, not `127.0.0.1`: passkeys need a hostname.

To serve the panel over HTTPS on your own domain instead, point the domain's DNS A record at the server, then set `DOCKWELL_DOMAIN` for the service, as in [Change settings](#change-settings). The panel then also answers on `https://<domain>`, with a certificate from Let's Encrypt, on ports 443 and 80. Passkeys belong to the address they were made on, so add one for the domain after you sign in there. [ADR 0011](adr/0011-claim-and-sign-in.md) explains both ways in.

## Add a server

On the control server, make a join ticket:

```sh
dockwell token create
```

Then run the install on the new server with that ticket. The new server joins and runs only the agent, with the agent build of `dockwell`, which has none of the control plane. It opens no ports, because it dials out to the control server on TCP port 7443, so make sure that port on the control server is open to it.

```sh
curl -fsSL https://get.dockwell.dev | sh -s -- --join <ticket>
```

The `sh -s --` passes the options to the script. You can set `DOCKWELL_JOIN` instead, as long as it is set for `sh` rather than for `curl`: `curl -fsSL https://get.dockwell.dev | DOCKWELL_JOIN=<ticket> sh`. A ticket works once and expires after an hour by default, so it does little harm in your shell history.

## Two builds

Each release has two builds of `dockwell`, from the same source ([ADR 0023](adr/0023-a-full-build-and-an-agent-build.md)). Both are installed as `/usr/local/bin/dockwell`:

- **The full build**, on the control server: the control plane, its own agent, the panel and every command.
- **The agent build**, on every server that joins: only the agent, with `dockwell run`, `join`, `role` and `version`. It links none of the control plane, so it idles in about a third less memory. The commands that manage the control plane, such as `dockwell token create`, `dockwell claim` and `dockwell servers list`, say they're on the control server.

The script picks the build from what the server is:

- Without `--join`, the server becomes a control server, and gets the full build.
- With `--join`, it joins, and gets the agent build. A release from before there were two builds, such as `--version 0.2.0`, has only the full build, which runs the agent too.
- A server that has joined gets the agent build when it upgrades. Servers that joined with v0.1.0 or v0.2.0 run the full build, which runs the agent too, so their next upgrade switches them to the agent build, and the script says so.
- A control server always gets the full build, and a server with the agent build that never joined, run again without `--join`, becomes a control server like any first install.

`dockwell version` says which build is installed.

## What the script does

1. It checks it runs as root on Linux, on amd64 or arm64, and works out the OS family and whether it uses systemd or OpenRC.
2. It finds the release: the latest, or the one you pass with `--version`. It picks the [build](#two-builds) for this server.
3. It verifies the release before it installs anything. It downloads cosign, at the version and SHA-256 pinned in the script, and checks that Dockwell's release workflow signed the release's checksums at that release's tag. Then it checks the archive against them. If any check fails, nothing is installed. [ADR 0010](adr/0010-install-script-verifies-with-pinned-cosign.md) explains why it works this way. Through a mirror, the checks are the same, against a copy of Sigstore's trust root pinned in the script ([below](#install-through-a-mirror)).
4. With `--join`, it runs `dockwell join` before the service ever starts, because a server that starts without an identity becomes a control server. A server that already joined keeps its control plane, and the script says which one; to move it to another, remove `/var/lib/dockwell/agent` and run the script again with a new ticket. `dockwell role` shows which role a server takes.
5. It installs the service and starts it, then waits until the panel answers, or on a server that joined, until the agent is up. If the panel doesn't answer within 30 s, it shows the service's last log lines, which say why.
6. On a control server, it prints a one-time claim link from `dockwell claim`. Once the install has an owner, `dockwell claim` exits with status 3, and the script says where the panel is instead. Any other failure of `dockwell claim` is shown, and the script fails.

It changes only these files:

| Path | What it is |
| --- | --- |
| `/usr/local/bin/dockwell` | The binary |
| `/etc/systemd/system/dockwell.service` | The service, on systemd |
| `/etc/init.d/dockwell` | The service, on Alpine's OpenRC, which logs to `/var/log/dockwell.log` |

Dockwell itself keeps its state in `/var/lib/dockwell`.

The service sets `GOMEMLIMIT=200MiB`, a soft limit on the memory the Go runtime manages. It is sized from the PRD's budget of 250 MB for the control plane and its agent, and leaves the rest for memory Go doesn't manage, such as the binary's own pages and SQLite's cache. A few minutes after starting, an idle agent on its own uses about 19 MB with the agent build, and a control server about 37 MB, most of it pages of the binary, so the limit only comes into play if memory grows far past that.

## Upgrade

Run the same command again. If a newer release is out, the script verifies and installs it, then restarts the service. A control server keeps the full [build](#two-builds), a server that has joined gets the agent build, switching to it if it joined with the full build, and your data stays where it is. If you already have the latest, in the build the server needs, it changes nothing and doesn't restart the service.

Pass `--version 0.2.0` to install a particular release instead of the latest.

## Uninstall

```sh
curl -fsSL https://get.dockwell.dev | sh -s -- --uninstall
```

This stops the service and removes the binary and the service file. Your workloads keep running: Dockwell runs apps as Docker containers and its ingress as a service of its own ([ADR 0005](adr/0005-caddy-ingress.md)), so stopping Dockwell's service stops neither. Your data stays in `/var/lib/dockwell`, so installing again picks up where you left off. To delete it too, run `rm -rf /var/lib/dockwell`. That removes the server's identity, and on a control server, the record of every server that joined it.

## Options

| Option | Environment | What it does |
| --- | --- | --- |
| `--join <ticket>` | `DOCKWELL_JOIN` | Join a control plane with a ticket from `dockwell token create`, and run only the agent |
| `--version <version>` | `DOCKWELL_VERSION` | Install that release, such as `0.1.0`, instead of the latest |
| `--mirror <url>` | `DOCKWELL_MIRROR` | Download Dockwell from a [mirror](#install-through-a-mirror) of its GitHub releases, and check it without reaching Sigstore |
| `--cosign-mirror <url>` | `DOCKWELL_COSIGN_MIRROR` | Download cosign from a mirror of its GitHub releases |
| `--uninstall` | | Remove Dockwell, and keep your workloads and data |
| `--help` | | Show the options |

These settings are for air-gapped servers, mirrors that re-sign releases, and tests:

| Environment | What it does |
| --- | --- |
| `DOCKWELL_RELEASES_URL` | Where releases come from, laid out as GitHub lays them out: `<url>/latest/download/` and `<url>/download/v<version>/`. It defaults to `https://github.com/Dockwell/dockwell/releases`. Unlike `--mirror`, it changes nothing else: cosign still comes from GitHub, and Sigstore's trust root from Sigstore |
| `DOCKWELL_COSIGN` | A cosign binary to use instead of downloading one. It must be the version the script pins, and the script checks its SHA-256 |
| `DOCKWELL_VERIFY_KEY` | A cosign public key to verify releases with, instead of Dockwell's release signature, for a mirror that re-signs them. It skips the transparency log, so the key's holder is all you trust. A key needs no Sigstore trust root, so such a mirror needn't serve `sigstore_trusted_root.json` |

## Change settings

The script replaces the service file on every upgrade, so put your changes somewhere it doesn't touch:

- On systemd, run `systemctl edit dockwell` to add a drop-in. To change the agent listener's address, or the URL other servers use to reach it, add `Environment=DOCKWELL_AGENT_LISTEN=…` or `Environment=DOCKWELL_ADVERTISE_URL=…`. To serve the panel on a domain, add `Environment=DOCKWELL_DOMAIN=panel.example.com`.
- On OpenRC, set them in `/etc/conf.d/dockwell`, with `export`, such as `export DOCKWELL_ADVERTISE_URL=https://203.0.113.7:7443`.

The panel itself always stays on loopback. `DOCKWELL_LISTEN` can move it to another loopback address or port, such as `127.0.0.1:7700` or `[::1]:7600`, and the script reads it from the service, so it waits for the panel there. An address beyond loopback, such as `0.0.0.0:7600`, would serve the panel over plain HTTP to the network, so `dockwell run` refuses to start with it, and the script refuses it before it installs anything. To reach the panel from elsewhere, set `DOCKWELL_DOMAIN` instead: the panel is then served over HTTPS on the domain, and its loopback address keeps working.

The control plane keeps its state in SQLite, in `/var/lib/dockwell/control/state.db`. To keep it in Postgres instead, ahead of running more than one control plane, set `DOCKWELL_DATABASE` to a `postgres://` URL before the first start, such as `postgres://dockwell@db.internal:5432/dockwell?sslmode=verify-full`. [ADR 0014](adr/0014-object-model-and-store.md) says how the two compare.

- Keep the password out of the service file, which anyone on the server can read: put it in `/root/.pgpass`, mode 0600, or set `DOCKWELL_DATABASE` in an `EnvironmentFile=` that only root can read. Dockwell never logs or prints the password.
- Commands you run beside the control plane, such as `dockwell claim`, `dockwell recover` and `dockwell token create`, need the same `DOCKWELL_DATABASE` in your shell.
- Moving an install that has started on SQLite to Postgres isn't supported yet. `dockwell run` refuses to start that way, rather than begin a second install that your servers wouldn't recognize, and it also refuses when `DOCKWELL_DATABASE` goes missing from an install on Postgres.

`dockwell run --help` lists every setting.

## What the script needs

- Debian, Ubuntu, a RHEL-family distribution or Alpine, on amd64 or arm64, with systemd, or OpenRC on Alpine. Other distributions with systemd may work, and the script says when it doesn't know yours.
- Root, and `curl` or `wget`, `tar`, `gzip` and `sha256sum`. When one is missing, the script says how to install it.
- Access to `github.com`, for the release and for cosign, and to `tuf-repo-cdn.sigstore.dev`, for Sigstore's trust root. Or a [mirror](#install-through-a-mirror) instead.
- About 200 MB free in `/var/tmp` while it runs, most of it for cosign, which must be able to run from there. If `/var/tmp` is mounted `noexec`, as hardened servers often have it, set `TMPDIR` to somewhere that allows programs, such as `/root`.

## Install through a mirror

From mainland China, and from other networks where GitHub or Sigstore is slow or out of reach, you can install from a mirror instead. Dockwell runs no mirror and no servers in China, and doesn't recommend any mirror service. You don't have to trust a mirror. The script checks everything it downloads from one, just as it checks a download from GitHub ([ADR 0013](adr/0013-mirrors-for-releases-and-images.md)):

- cosign must match the SHA-256 pinned in the script.
- Sigstore's trust root must match the SHA-256 pinned in the script. cosign checks the signature against it, so the install never contacts Sigstore.
- The checksums must carry the signature of Dockwell's release workflow at that release's tag, and the archive must match them.

If the mirror changed any of these, nothing is installed. What a mirror can still do is fail to serve you, serve an out-of-date release, or serve an older release as the latest. To rule out the last, pin a release with `--version`.

### What a mirror serves

A mirror of Dockwell serves Dockwell's GitHub releases at its own URL, with the same paths after it:

| On the mirror | Copied from |
| --- | --- |
| `<mirror>/download/v<version>/<file>` | `https://github.com/Dockwell/dockwell/releases/download/v<version>/<file>` |
| `<mirror>/latest/download/<file>` | The same files for the newest release |

Each release has these files. The mirror needs all of them, except an archive for an architecture or a build none of your servers use:

- `dockwell_<version>_linux_amd64.tar.gz` and `dockwell_<version>_linux_arm64.tar.gz`, the full build, for control servers;
- `dockwell-agent_<version>_linux_amd64.tar.gz` and `dockwell-agent_<version>_linux_arm64.tar.gz`, the agent build, for servers that join. Releases before it, v0.1.0 and v0.2.0, have none;
- `checksums.txt` and its signature, `checksums.txt.sigstore.json`;
- `sigstore_trusted_root.json`, the copy of Sigstore's trust root that the release's script pins. When a release lacks it, or its copy doesn't match the pin, the script takes the latest release's copy instead, so the mirror needs it at least in `latest/download/`;
- `install.sh`, the install script.

cosign comes from its own GitHub releases, at `https://github.com/sigstore/cosign/releases/download/v3.1.3/cosign-linux-amd64` and `…/cosign-linux-arm64`. A mirror of those, with the same paths after its URL, goes in `--cosign-mirror`. You can instead copy cosign to the server and point `DOCKWELL_COSIGN` at it.

The mirror can be any web server or object storage bucket you copy the files to. A service that proxies GitHub downloads works too, as long as it keeps GitHub's paths after its own URL.

### Install from it

Run the install script from the mirror's latest release, and pass the mirror to it:

```sh
curl -fsSL https://<mirror>/latest/download/install.sh | sh -s -- \
  --mirror https://<mirror> --cosign-mirror https://<cosign mirror>
```

Add `--join <ticket>` to add a server, as above. To upgrade, run the same command again: the script doesn't remember the mirror.

Use the script from the same mirror. Each release's script pins the trust root that release carries, so a script from elsewhere may pin another one. When it does, the script says so and installs nothing.

If you can reach GitHub but not Sigstore, name GitHub itself as the mirror: `--mirror https://github.com/Dockwell/dockwell/releases`.

### Run your own mirror

Copy a release, and cosign, to a directory your web server or bucket serves. The checksums list every file of the release but their own signature, so this copies both builds:

```sh
v=0.2.0
release=https://github.com/Dockwell/dockwell/releases/download/v$v
mkdir -p mirror/download/v$v mirror/latest/download cosign/download/v3.1.3
(
  cd mirror/download/v$v
  curl -fsSLO "$release/checksums.txt"
  curl -fsSLO "$release/checksums.txt.sigstore.json"
  for f in $(awk '{ print $2 }' checksums.txt); do curl -fsSLO "$release/$f"; done
)
cp mirror/download/v$v/* mirror/latest/download/
for arch in amd64 arm64; do
  curl -fsSL -o cosign/download/v3.1.3/cosign-linux-$arch \
    https://github.com/sigstore/cosign/releases/download/v3.1.3/cosign-linux-$arch
done
```

When a new release comes out, copy it the same way, and replace what's in `latest/download/`. Keep older releases, so servers can still install them with `--version`.

### Container images

Dockwell starts running apps as Docker containers in M3. Until then, Docker on your servers pulls images the way it is set up there. To pull Docker Hub images through a registry mirror, add it to `/etc/docker/daemon.json`:

```json
{
  "registry-mirrors": ["https://<registry mirror>"]
}
```

Then run `systemctl reload docker`. Docker reloads this setting without restarting, so running containers keep running. `docker info` lists it under "Registry Mirrors". The setting covers Docker Hub only: Docker has no setting to mirror other registries, such as `ghcr.io`. Dockwell's Docker driver will add one, and it pulls each image by digest, so a mirror can't change what runs ([ADR 0013](adr/0013-mirrors-for-releases-and-images.md#container-images-from-m3)).

## Read it first

You can read the script before you run it:

```sh
curl -fsSL https://get.dockwell.dev -o install.sh
less install.sh
sh install.sh
```

Everything runs from one function call on its last line, so a download that is cut short runs nothing.

## How it is tested

The Install workflow ([`.github/workflows/install.yml`](../.github/workflows/install.yml)) runs on every pull request against a release snapshot built from that commit. The snapshot is laid out as releases and signed with a throwaway key, so no published release is needed.

- **Containers**: stock Debian 13, Ubuntu 26.04, AlmaLinux 9 and Alpine 3.24 images, on amd64 and arm64. Each one installs an older build, refuses a tampered archive and a bad signature, upgrades in place to the full build, runs again with nothing to do, and uninstalls. Then it joins a control plane, beside the key a failed first start as a control server leaves, gets the agent build and runs only the agent. Last, it joins with the older build, which stands in for a release from before the agent build and has only the full build, and upgrades: the server switches to the agent build, its agent connects again, and running the script again keeps it. systemd isn't running in these containers, so the unit is checked with `systemd-analyze verify` and the binary started the way the unit says. Alpine runs the OpenRC service for real, and uses busybox `wget` instead of curl.
- **Mirror**: in the same containers, the install and upgrade run again through a mirror on a second URL, with GitHub and Sigstore cut off. A cosign, archive or signature changed on the mirror is refused, and so is a version the mirror lacks. The test releases are signed with a key, which needs no trust root, so the upgrade comes from a mirror that carries none. The script's own steps that find the trust root on a mirror are tested on their own, through a mirror that answers 404 for a missing file, one that answers 403 as storage buckets do, and a `file://` path: a release without its own copy takes the latest release's, a changed copy is refused, and the latest release's copy is fetched once. A keyless signature from Sigstore's public instance verifies offline against the pinned trust root.
- **systemd**: the same steps on the runner's own Ubuntu 24.04, where systemd is running, so the service really starts, restarts and stops.
- **Install time**: every night, an install to a running panel with the install and the service held to one CPU, which fails at 60 s. It runs once with main's snapshot, and once with the latest published release, checked as a server would check it.

The Go tests, which `make check` and CI run, test the script's own steps that pick the build ([`internal/cli/install_script_test.go`](../internal/cli/install_script_test.go)): against data directories of each kind of server, with a stand-in for the installed binary that is run once, and against `dockwell run`'s own test for a control plane.

Run the container tests locally with `make install-test`, which needs Docker and cosign v3. The scenario itself is [`scripts/install-test/run.sh`](../scripts/install-test/run.sh).

The Sigstore trust root workflow ([`.github/workflows/trust-root.yml`](../.github/workflows/trust-root.yml)) checks that the copy the script pins is the one Sigstore publishes through TUF now, and that it verifies keyless signatures with cosign cut off from the network. Once there is a published release, that includes Dockwell's latest release. Sigstore changes its trust root on its own schedule, so the check doesn't run on every pull request, where one change would fail them all. It runs nightly, on pull requests that change the install script, its tests, the pin, or the workflows that use them, and in the release workflow before a release is published. A failed nightly run opens an issue, or comments on the one already open.
