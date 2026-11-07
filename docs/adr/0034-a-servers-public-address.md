# 0034. A server's public address

- Status: Accepted
- Date: 2026-11-06

## Context

The panel told someone moving a server to Dockwell to point DNS at its private address. Its public address, which is on no interface, doesn't come from Oracle's metadata service either (#163).

Three facts had been one: what a server's interfaces have, what the internet knows it by, and whether the internet can reach it. Servers come in shapes that pull them apart: a VPS with its public address on an interface, a cloud's 1:1 NAT, a home or office router, and a carrier's NAT.

## Decision

### Three facts, kept apart

A server has its private addresses, from its interfaces; a public address for each of IPv4 and IPv6, with how each is known and when; and whether it's reachable from the internet. The API carries the second and third as `Server.network`. Each is said on its own, and knowing an address proves nothing about reaching it.

### Where DNS may be pointed

Five rules decide which address Dockwell may tell someone to point DNS at, and the rest follows from them.

1. **A way out never names a DNS target without the caveat,** unless a way-in source agrees with it. What STUN or the control plane sees is where packets leave from, wherever the server is: an Azure NAT gateway, an AWS private subnet and a proxy routing everything out of an Oracle server all leave somewhere the internet can't come back in. Being on a cloud doesn't change that, and a route out through a TUN device is the same case.
2. **Metadata counts as a way in only where the machine says it runs on that provider.** Anything on a local network can answer at `169.254.169.254`, and it can't write the machine's firmware tables, which the agent reads the way cloud-init does. Without them agreeing, what answered is unproven.
3. **On a cloud, metadata outranks a way out.** What the provider says the server is reached by beats how it happens to go out.
4. **A domain's proof belongs to its certificate, and must be fresh.** A certificate shows where a server answered, and a record moves. So a proof keeps the record as it read when the certificate came, and holds only while the current read matches it. A later read can drop a proof, and never bring one back or move it, so a domain that moves away while its route and certificate stay proves nothing about where it went. The next certificate makes a new proof, as getting it took the server answering at the record again, and that's read at once as it comes. The agent reads a domain's DNS every 6 hours, and a read older than two of those, and an hour, proves nothing until a newer one comes. On a home or office network, a way out whose address has changed since the certificate came, to one the record doesn't have, drops the proof too, as an address moving under a record is what that looks like. It goes by the address changing, not by when it was last seen, as a way out that always differed says nothing new. A record of both families proves neither, whatever else is known: a certificate authority falls back from one to the other, so the certificate may have come over the other family, and a stale AAAA beside a working A record would read as a way in.
5. **IPv4 and IPv6 are picked and told separately.** A public IPv6 address on an interface says nothing of how IPv4 reaches the server, and a record is only ever read against a way in of its own family.

### One definition of a public address

`resource.IsPublicAddress` is the only one, and it follows the "Globally Reachable" column of IANA's special-purpose registries for every range, with what's reachable inside a range that isn't, and multicast. So `198.18.0.0/15`, which mihomo gives its TUN by default, isn't public, nor is `100.64.0.0/10`. Three documentation ranges count as public against the registry, as this repo's tests and examples use them as a server's address: `198.51.100.0/24`, `203.0.113.0/24` and `2001:db8::/32`.

A test holds the definition to copies of the registries in `internal/resource/testdata/`, at both ends of every range. The panel's `isPrivateAddress` is a second reading of the same ranges, and `internal/resource/testdata/addresses.json` has a row for both ends of each, which Go and the panel both answer. A private address is never named as somewhere to point DNS, and it only appears as a server's address with a Private tag.

A TUN or TAP device is also told by kind, from the `tun_flags` file the kernel gives only those, as a program's device can take any address. DNS checks skip it, and an outbound route through one reads as the server's own address.

### The sources, in the order they win

| Source | What it means | A way in? |
| --- | --- | --- |
| Set by a person | They know, from their provider's console or their router. It's in the store and the audit log | Taken at their word |
| On an interface | The internet routes to that address | Yes, as far as an address goes |
| Proved by a domain | A certificate came over HTTP-01 or TLS-ALPN for a name whose one record, of one family, points there, and the record reads as it did then, within rule 4's limit, so the server answered there | Yes |
| Metadata the machine confirms | The provider says so, on AWS, GCP, Azure, DigitalOcean or Hetzner, and the machine's firmware names the same provider | Yes, as the provider's 1:1 NAT |
| Seen by the control plane | A joined agent connected from it | No: its way out |
| Metadata the machine doesn't confirm | Something answered at the metadata address, and the machine names no such provider | Unproven |
| Looked up | A public STUN server saw the server's packets come from it | No: its way out |

Each family walks this list by itself.

The control plane works this out from what it hears, in one list, `publicSources` in `internal/control/fleet/network.go`. Another source is another line there. The addresses reach the server's agent over its stream, each marked as a way in or not, so what the agent says of a domain, and how it reads DNS, go by them. A family with no way in known never makes a record of that family read as pointing elsewhere: the agent can't tell, and says so.

A setting isn't a revision: it changes nothing on a server, only what's said of it, so it goes through `SetServerPublicAddress` and the audit log, which keeps the address it replaced so it can be put back. The PRD lists this among the exceptions to principle 2. What's heard from agents is held in memory, goes with an agent's last stream, and comes back as it says it again. What each certificate proved is the exception, kept in the store, as an upgrade restarts the control plane, and it would otherwise take a moved record for new.

### What "reachable" may claim

Yes, no or not checked, and never a guess. Only something outside connecting proves a way in.

- **Yes** is a certificate that came for a domain whose record points at the public address, with its proof still standing (rule 4). The agent reads a domain's DNS while it waits for a certificate, as one comes, and every 6 hours after, so this is as old as that read, and the panel says as of when.
- **No** is the certificate authority failing to connect for such a domain. A certificate for another domain wins over it, as a failure can have other causes.
- **Not checked** is everything else, whatever else is known. Nothing looked up, seen or read from metadata changes it, and a person setting the address doesn't either.

Another joined server connecting to the control server would also prove a way in, to the agent listener's port. It's a true claim, and it isn't built.

### A way out isn't a way in

What the control plane sees and what STUN sees is where a server's packets leave from. Behind a NAT gateway it's another address from the way in, and behind a home or office router it's the router's, shared with everything else there. STUN alone can't tell that from a cloud's 1:1 NAT.

So these two never set reachable, and never make a record pointing elsewhere read as wrong. Wherever one is named, the words say what it may not be, in the kind of record its family takes:

> Point an A record at 203.0.113.7, which is how this server reaches the internet. If it's behind a router, that record won't reach it without a forwarded port, and a tunnel needs neither.

The panel doesn't show one as the server's address, as a link to it wouldn't reach the server. Its Network section shows it, as looked up.

### The shapes

`Server.network.shape` is read from a server's IPv4 addresses and what the machine says it runs on. An agent always says what it found, so saying nothing isn't taken for no, and a server with no IPv4 address has no shape.

| It has | It reads as | What follows |
| --- | --- | --- |
| A public IPv4 address on an interface | On the internet | A record |
| Private interfaces, on a machine that names a cloud, Oracle included | A cloud's NAT | A record, with the caveat until a way in is known, and the provider's firewall named |
| Private interfaces, on a machine that names none | Likely a home or office network | Cloudflare Tunnel first, and a record second, needing a forwarded port |
| Its route to the internet leaves from `100.64.0.0/10`, on a machine that names none | Carrier NAT | A tunnel only, as a forwarded port can't work |

Carrier NAT goes by the address a server's route to the internet leaves from, not by what's on an interface, as Tailscale puts that range on an interface of a server on any network. A machine naming a cloud wins, as a cloud can use that range inside a VPC. Behind carrier NAT, a way out isn't offered as anywhere to point a record at all. "Likely" is as far as this goes: a server in a datacentre behind a firewall, on no cloud we know, reads as a home or office.

### Asking a third party

The owner decided this, and nothing else asks one: another lookup needs the same.

- A server asks a public STUN server where its packets come from, with one 20-byte binding request over UDP, as its stream first opens and every 6 hours, however often that stream comes back. It's `stun.cloudflare.com:3478`, then `stun.l.google.com:19302` if that doesn't answer, so one operator hears from it. The control server does it too, as its own agent.
- It's on by default, as a server behind NAT otherwise has nothing to say. `--no-stun` or `DOCKWELL_NO_STUN` turns it off, which [the install guide](../install.md) says how to set. The first lookup runs as the service first starts, before anyone can.
- A server with a public IPv4 address of its own doesn't ask, as the answer can't change anything.
- It's a few seconds at most, beside the agent's stream, and nothing waits for it. With UDP blocked it says nothing and goes on.
- It needs no module: the wire format is 20 bytes out and one attribute back, and the agent build links nothing new for it.

[The threat model](../security/public-address.md) says what's sent and what an answer can do. Metadata services are link-local, and reading them is no third party.

### What the domain form says

It says what to do by the shape, and a way in leads with its record whatever the shape is, as someone has proved or said so, but for IPv4 at home. There's one record for each family, and a way out is offered as what it is: on a cloud a record to check against the provider's console, and behind carrier NAT not at all. At home a tunnel leads unless IPv4 itself is a known way in, a way in of IPv6 is a record beside it, and the router's address goes only behind a forwarded port, to the address the server reaches the internet from. Each record says whether DNS has it by its own address among the answers, as an A record says nothing of an AAAA one.

`DomainService.CheckDomain` reads DNS for a name as it's typed and until each record is there, with no route yet, on the control plane's resolver. It uses what's known of the server, so it says what a plan would, and only the server's public addresses count as where a record leads to it, as a private one, a bridge's or a TUN's never is. As it resolves names through the control server, it needs a token that can make changes, a caller can check 60 names a minute, and a private answer is shown only when it's the server's own, so it can't be used to read what names resolve to inside a network. A record that points at Cloudflare's ranges reads as `proxied`, as the proxy can block a certificate over HTTP-01 or TLS-ALPN, and works with a route that takes none. With no address known and no shape to go by, it says where the address is shown, and the form's switch for a tunnel or a proxy is the one question it asks.

## Consequences

- Every word about DNS has one source of truth, on the agent, so a change to what's known changes what a domain says within seconds.
- A certificate from before this has no record kept, so the first read of it stands in for one. A record that moved before that read, with the route left behind, can read as proved at the new address until its certificate next renews, or fails to. The claim is as old as that read, and a person's setting wins over it.
- A proof that's dropped stays dropped until the next certificate, about 60 days at most. So a home server whose address moved reads as unproven until then, even with its record put right, and a person can set the address sooner.
- A proxy in front that passes HTTP-01 through, other than Cloudflare, whose ranges are known, proves its own address. That's true as far as it goes: the server does answer there.
- A server that sends its UDP through a proxy, as mihomo does when it routes everything, looks up the proxy's address, not its own. It's the lowest source and says it's unproven, so anything else known wins over it.
- Only the providers in the table are recognised. Aliyun has a metadata service at its own address, which isn't asked, so a server there behind 1:1 NAT reads as a home or office network until someone sets its address.
- Docker's end-to-end can't show NAT, so the sources and shapes are tested with fakes. A test binary refuses to ask a real STUN server or metadata service, and `DOCKWELL_TEST`, which the Makefile and CI set for all they start, has a real binary ask neither. Real STUN servers answered the code by hand, once. No real cloud's metadata service has been read: the paths are from each provider's documentation, and Oracle's is the only one the owner's server can confirm.
