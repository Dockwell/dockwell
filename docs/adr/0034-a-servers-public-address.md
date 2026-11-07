# 0034. A server's public address

- Status: Accepted
- Date: 2026-11-06

## Context

The panel told someone moving a server to Dockwell to point DNS at its private address. Its public address, which is on no interface, doesn't come from Oracle's metadata service either (#163).

Three facts had been one: what a server's interfaces have, what the internet knows it by, and whether the internet can reach it. Servers come in shapes that pull them apart: a VPS with its public address on an interface, a cloud's 1:1 NAT, a home or office router, and a carrier's NAT.

## Decision

### Three facts, kept apart

A server has its private addresses, from its interfaces; a public address, with how it's known and when; and whether it's reachable from the internet. The API carries the second and third as `Server.network`. Each is said on its own, and knowing an address proves nothing about reaching it.

### One definition of a public address

`resource.IsPublicAddress` is the only one, and it follows the "Globally Reachable" column of IANA's special-purpose registries for every range, with what's reachable inside a range that isn't, and multicast. So `198.18.0.0/15`, which mihomo gives its TUN by default, isn't public, nor is `100.64.0.0/10`. Three documentation ranges count as public against the registry, as this repo's tests and examples use them as a server's address: `198.51.100.0/24`, `203.0.113.0/24` and `2001:db8::/32`.

A test holds the definition to copies of the registries in `internal/resource/testdata/`, at both ends of every range. The panel's `isPrivateAddress` is a second reading of the same ranges, and `internal/resource/testdata/addresses.json` has a row for both ends of each, which Go and the panel both answer. A private address is never named as somewhere to point DNS, and it only appears as a server's address with a Private tag.

A TUN or TAP device is also told by kind, from the `tun_flags` file the kernel gives only those, as a program's device can take any address. DNS checks skip it, and an outbound route through one reads as the server's own address.

### The sources, in the order they win

| Source | What it means | A way in? |
| --- | --- | --- |
| Set by a person | They know, from their provider's console or their router. It's in the store and the audit log, and it's the only source that's kept | Taken at their word |
| On an interface | The internet routes to that address | Yes, as far as an address goes |
| Proved by a domain | A certificate came over HTTP-01 or TLS-ALPN for a name whose one record points there, so the server answered there | Yes |
| Seen by the control plane | A joined agent connected from it | No: its way out |
| Cloud metadata | The provider says so: AWS, GCP, Azure, DigitalOcean and Hetzner | Yes, as the provider's 1:1 NAT |
| Looked up | A public STUN server saw the server's packets come from it | No: its way out |

The control plane works this out from what it hears, in one list, `publicSources` in `internal/control/fleet/network.go`. Another source is another line there. The address reaches the server's agent over its stream, so what the agent says of a domain, and how it reads DNS, go by it. An address with no way in known never makes a record read as pointing elsewhere: the agent can't tell, and says so.

A setting isn't a revision: it changes nothing on a server, only what's said of it, so it goes through `SetServerPublicAddress` and the audit log. What's heard from agents is held in memory, and comes back as they say it again, so "when" for those is since the control plane last started.

### What "reachable" may claim

Yes, no or not checked, and never a guess. Only something outside connecting proves a way in.

- **Yes** is a certificate that came for a domain whose record points at the public address. The agent reads a domain's DNS while it waits for a certificate, and once more if it starts up holding one, so this is what the record said around when the certificate came.
- **No** is the certificate authority failing to connect for such a domain. A certificate for another domain wins over it, as a failure can have other causes.
- **Not checked** is everything else, whatever else is known. Nothing looked up, seen or read from metadata changes it, and a person setting the address doesn't either.

Another joined server connecting to the control server would also prove a way in, to the agent listener's port. It's a true claim, and it isn't built.

### A way out isn't a way in

What the control plane sees and what STUN sees is where a server's packets leave from. Behind a NAT gateway it's another address from the way in, and behind a home or office router it's the router's, shared with everything else there. STUN alone can't tell that from a cloud's 1:1 NAT.

So these two never set reachable, and never make a record pointing elsewhere read as wrong. Where such an address is unproven and the server isn't on a known cloud, the words say so:

> Point an A record at 203.0.113.7, which is how this server reaches the internet. If it's behind a router, that record won't reach it without a forwarded port, and a tunnel needs neither.

The panel doesn't show one as the server's address, as a link to it wouldn't reach the server. Its Network section shows it, as looked up.

### The shapes

`Server.network.shape` is read from what's on a server's interfaces and whether a metadata service answers. An agent always says whether one did, so that isn't taken for no.

| It has | It reads as | What follows |
| --- | --- | --- |
| A public address on an interface | On the internet | A record |
| Private interfaces, and a metadata service answers, Oracle's included | A cloud's NAT | A record, and the provider's firewall named |
| Private interfaces, and none answers | Likely a home or office network | Cloudflare Tunnel first, and a record second, needing a forwarded port |
| Its route to the internet leaves from `100.64.0.0/10`, and none answers | Carrier NAT | A tunnel only, as a forwarded port can't work |

Carrier NAT goes by the address a server's route to the internet leaves from, not by what's on an interface, as Tailscale puts that range on an interface of a server on any network. A metadata service answering wins, as a cloud can use that range inside a VPC. Behind carrier NAT, a way out isn't offered as anywhere to point a record at all. "Likely" is as far as this goes: a server in a datacentre behind a firewall, on no cloud we know, reads as a home or office.

### Asking a third party

The owner decided this, and nothing else asks one: another lookup needs the same.

- A server asks a public STUN server where its packets come from, with one 20-byte binding request over UDP, some seconds after its stream opens and every 6 hours. It's `stun.cloudflare.com:3478`, then `stun.l.google.com:19302` if that doesn't answer, so one operator hears from it. The control server does it too, as its own agent.
- It's on by default, as a server behind NAT otherwise has nothing to say. `--no-stun` or `DOCKWELL_NO_STUN` turns it off.
- A server with a public IPv4 address of its own doesn't ask, as the answer can't change anything.
- It's a few seconds at most, beside the agent's stream, and nothing waits for it. With UDP blocked it says nothing and goes on.
- It needs no module: the wire format is 20 bytes out and one attribute back, and the agent build links nothing new for it.

[The threat model](../security/public-address.md) says what's sent and what an answer can do. Metadata services are link-local, and reading them is no third party.

## Consequences

- Every word about DNS has one source of truth, on the agent, so a change to what's known changes what a domain says within seconds.
- A server with a certificate from before this reads as proved by it as soon as its agent reads the domain's DNS at start. A record that moved after the certificate came, with the route left behind, would read as proved at the new address. The certificate is real, and the claim is as old as that read. A person's setting wins over it.
- A proxy in front that passes HTTP-01 through, other than Cloudflare, whose ranges are known, proves its own address. That's true as far as it goes: the server does answer there.
- A server that sends its UDP through a proxy, as mihomo does when it routes everything, looks up the proxy's address, not its own. It's the lowest source and says it's unproven, so a person's setting or a certificate wins over it.
- Only the providers in the table are recognised. Aliyun has a metadata service at its own address, which isn't asked, so a server there behind 1:1 NAT reads as a home or office network until someone sets its address.
- Docker's end-to-end can't show NAT, so the sources and shapes are tested with fakes. Real STUN servers answered the code by hand, once. No real cloud's metadata service has been read: the paths are from each provider's documentation, and Oracle's is the only one the owner's server can confirm.
