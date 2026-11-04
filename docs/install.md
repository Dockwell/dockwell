# Installing Dockwell

One command installs Dockwell on a Linux server. Running it again upgrades in place, and `--uninstall` removes it without touching your workloads.

> `get.dockwell.dev` isn't set up yet. Until it is, use the script's address in this repository instead: `https://raw.githubusercontent.com/Dockwell/dockwell/main/scripts/install.sh`. The commands below work the same with either.

## Install the first server

Run this as root:

```sh
curl -fsSL https://get.dockwell.dev | sh
```

On Alpine, which comes without curl, use `wget -qO- https://get.dockwell.dev | sh`.

The first server becomes your control server. It runs the control plane, which serves the panel and API, and an agent of its own. The panel listens on `127.0.0.1:7600`, so only the server itself can reach it. To open it from your computer, forward the port over SSH:

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

Then run the install on the new server with that ticket. The new server joins and runs only the agent. It opens no ports, because it dials out to the control server on TCP port 7443, so make sure that port on the control server is open to it.

```sh
curl -fsSL https://get.dockwell.dev | sh -s -- --join <ticket>
```

The `sh -s --` passes the options to the script. You can set `DOCKWELL_JOIN` instead, as long as it is set for `sh` rather than for `curl`: `curl -fsSL https://get.dockwell.dev | DOCKWELL_JOIN=<ticket> sh`. A ticket works once and expires after an hour by default, so it does little harm in your shell history.

## What the script does

1. It checks it runs as root on Linux, on amd64 or arm64, and works out the OS family and whether it uses systemd or OpenRC.
2. It finds the release: the latest, or the one you pass with `--version`.
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

The service sets `GOMEMLIMIT=200MiB`, a soft limit on the memory the Go runtime manages. It is sized from the PRD's budget of 250 MB for the control plane and its agent, and leaves the rest for memory Go doesn't manage, such as the binary's own pages and SQLite's cache. At idle, an agent on its own uses about 20 MB and a control server about 26 MB, so the limit only comes into play if memory grows far past that.

## Upgrade

Run the same command again. If a newer release is out, the script verifies and installs it, then restarts the service. Your data stays where it is. If you already have the latest, it changes nothing and doesn't restart the service.

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
| `DOCKWELL_VERIFY_KEY` | A cosign public key to verify releases with, instead of Dockwell's release signature, for a mirror that re-signs them. It skips the transparency log, so the key's holder is all you trust |

## Change settings

The script replaces the service file on every upgrade, so put your changes somewhere it doesn't touch:

- On systemd, run `systemctl edit dockwell` to add a drop-in. To change the agent listener's address, or the URL other servers use to reach it, add `Environment=DOCKWELL_AGENT_LISTEN=…` or `Environment=DOCKWELL_ADVERTISE_URL=…`. To serve the panel on a domain, add `Environment=DOCKWELL_DOMAIN=panel.example.com`.
- On OpenRC, set them in `/etc/conf.d/dockwell`, with `export`, such as `export DOCKWELL_ADVERTISE_URL=https://203.0.113.7:7443`.

The panel itself always stays on loopback. `DOCKWELL_LISTEN` can move it to another loopback address or port, such as `127.0.0.1:7700` or `[::1]:7600`, and the script reads it from the service, so it waits for the panel there. An address beyond loopback, such as `0.0.0.0:7600`, would serve the panel over plain HTTP to the network, so `dockwell run` refuses to start with it, and the script refuses it before it installs anything. To reach the panel from elsewhere, set `DOCKWELL_DOMAIN` instead: the panel is then served over HTTPS on the domain, and its loopback address keeps working.

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

Each release has these files. The mirror needs all of them, except an archive for an architecture none of your servers use:

- `dockwell_<version>_linux_amd64.tar.gz` and `dockwell_<version>_linux_arm64.tar.gz`;
- `checksums.txt` and its signature, `checksums.txt.sigstore.json`;
- `sigstore_trusted_root.json`, the copy of Sigstore's trust root that the release's script pins;
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

Copy a release, and cosign, to a directory your web server or bucket serves:

```sh
v=0.1.0
mkdir -p mirror/download/v$v mirror/latest/download cosign/download/v3.1.3
for f in checksums.txt checksums.txt.sigstore.json sigstore_trusted_root.json install.sh \
  dockwell_${v}_linux_amd64.tar.gz dockwell_${v}_linux_arm64.tar.gz; do
  curl -fsSL -o mirror/download/v$v/$f https://github.com/Dockwell/dockwell/releases/download/v$v/$f
done
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

- **Containers**: stock Debian 13, Ubuntu 26.04, AlmaLinux 9 and Alpine 3.24 images, on amd64 and arm64. Each one installs an older build, refuses a tampered archive and a bad signature, upgrades in place, runs again with nothing to do, uninstalls, and joins a control plane to run only the agent.
- **Mirror**: in the same containers, the install and upgrade run again through a mirror on a second URL, with GitHub and Sigstore cut off. A cosign, trust root, archive or signature changed on the mirror is refused. A keyless signature from Sigstore's public instance verifies offline against the pinned trust root. systemd isn't running in these containers, so the unit is checked with `systemd-analyze verify` and the binary started the way the unit says. Alpine runs the OpenRC service for real, and uses busybox `wget` instead of curl.
- **systemd**: the same steps on the runner's own Ubuntu 24.04, where systemd is running, so the service really starts, restarts and stops.
- **Sigstore trust root**: the copy the script pins is the one Sigstore publishes through TUF now, and it verifies keyless signatures with cosign cut off from the network. Once there is a published release, that includes Dockwell's latest release.
- **Install time**: every night, an install to a running panel with the install and the service held to one CPU, which fails at 60 s. It runs once with main's snapshot, and once with the latest published release, checked as a server would check it.

Run the container tests locally with `make install-test`, which needs Docker and cosign v3. The scenario itself is [`scripts/install-test/run.sh`](../scripts/install-test/run.sh).
