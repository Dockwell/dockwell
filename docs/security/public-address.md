# A server's public address: threat model

Finding a server's public address talks to things beyond it. This says what's sent, to whom, and what an answer can do. The design is in [ADR 0034](../adr/0034-a-servers-public-address.md).

## What leaves the server

| Who hears | What's sent | When | Turn it off |
| --- | --- | --- | --- |
| `stun.cloudflare.com:3478`, Cloudflare's public STUN server, or `stun.l.google.com:19302`, Google's, if that doesn't answer | One 20-byte STUN binding request over UDP, with a random ID in it. That's all it holds, and a DNS lookup of the name goes before it, through the server's resolver | As its agent first connects, and every 6 hours, from any server with no public IPv4 address of its own, the control server included | `dockwell run --no-stun`, or `DOCKWELL_NO_STUN=1` |
| The cloud provider's metadata service, at `169.254.169.254` | HTTP requests for the instance's ID and public address, and no redirect is followed | At the same times, from every server | It's link-local, so it doesn't leave the server's own network, and it can't be turned off |
| The control plane | What those said, over the agent's stream | At the same times | |

The operator of a STUN server learns the address the request came from and when, as anything a server connects to does. It learns nothing of Dockwell, the server's name or its domains. It may keep logs of that under its own terms.

## What an answer can do

Nothing that's looked up, seen or read from metadata can say a server is reachable, and none of it changes anything on a server. It only changes what Dockwell says of a server's address. What's wrong can mislead a person into a record that doesn't work:

| Who | What they can do | What stops it |
| --- | --- | --- |
| Someone on the path to a STUN server | Answer with an address of their choosing, if they can read the request's ID | It's the lowest source there is, so anything else known wins, and the words always say it's unproven. An answer must carry the ID, come from the address asked, and be a public address |
| Something on the local network answering at `169.254.169.254` | Say it's a provider, and give an address | The machine's firmware has to name that provider too, which nothing on a network can change. Without that the address is only ever named as unproven, below what the control plane sees, and no provider is named. The address must be public, and it's below what a person sets, an interface and a certificate either way |
| A compromised agent | Say anything of its own server | It's that server's address and nothing more, and a person's setting wins |

## What it doesn't protect

- A proxy that passes HTTP-01 through, other than Cloudflare, proves its own address, not the server's.
- A certificate from before Dockwell kept what each one proved has no record to go by, so the first read stands in for it. A record that moved before that read can read as proved at the new address until the certificate next renews. Any later move drops the proof, and a read goes stale in 13 hours.
- Something that controls a machine's firmware tables, as a hypervisor does, can say what it runs on. That's its own machine to describe.
- Anyone who can call `SetServerPublicAddress` with a read-write token or a session can set the address. It's on Activity, and it changes only what's said.
