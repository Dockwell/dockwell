# 0088. Certificates by DNS-01, and wildcard domains

- Status: Proposed
- Date: 2026-11-13

## Context

A route's certificate comes from Caddy's own ACME, over HTTP-01 or TLS-ALPN-01 ([ADR 0024](0024-the-caddy-ingress-driver.md)), so the certificate authority must reach the server on port 80 or 443 at the name. A server behind NAT, on a private network, or behind a tunnel that passes TLS through can't be reached that way, and a wildcard such as `*.example.com` can only be proven by DNS. The PRD's networking row asks for HTTP-01, DNS-01 and wildcards (P0), and #602 asks for DNS-01 through a DNS provider's API, Cloudflare first, wildcards with apps' routes under one certificate, each change a plan, and renewal failures as alerts in the provider's own words. 1Panel does this for servers behind NAT and for wildcards, and people moving from it expect it.

Two records left this for now:

- [ADR 0024](0024-the-caddy-ingress-driver.md#certificate-state) has a seam for the fleet: the control plane issues, the agent loads what it's given into Caddy's `tls.certificates`, with a policy that stops Caddy issuing for those names, and certificate state keeps reporting.
- [ADR 0030](0030-routes-behind-a-tunnel-or-a-proxy.md#a-real-certificate-with-no-open-port) sketched DNS-01 as Caddy built with `caddy-dns/cloudflare`, an image of Dockwell's own, with the API token in Caddy's config. This record decides otherwise, below.

## Decision

### The control plane proves names by DNS, and Caddy keeps HTTP-01

The control plane runs the ACME DNS-01 challenge itself, with `golang.org/x/crypto/acme`, which the panel's own certificate already uses, and writes the challenge's TXT record through the DNS provider's API. The agent loads the certificate it's given into Caddy, as ADR 0024's seam says. Caddy keeps issuing by HTTP-01 and TLS-ALPN-01 for every name no certificate by DNS covers, exactly as now.

Not a Caddy DNS module:

- **Caddy's official image has none.** One means building Caddy with `xcaddy`, for each architecture, for every Caddy release, and publishing, signing and mirroring an image of our own in place of the pinned official one ([ADR 0013](0013-mirrors-for-releases-and-images.md)). Each DNS module is a new dependency in that image, held to the 14-day rule, on every server with a route.
- **The token would be on every server with a route.** Caddy's config holds what its modules use, so the token would be in its last config and the agent's copy on each such server, as the panel's key is now, and a compromised Caddy, or anyone who reads its config, could rewrite the zone. A token that edits a zone's DNS can take every name in it, so it stays in one place.
- **Here the token never leaves the control plane,** as an ECR key doesn't ([ADR 0086](0086-registry-logins-for-private-images.md)). What reaches a server is a certificate and its key, for names its routes serve, which is what Caddy has on disk for any name it issues itself.
- **The agent build links nothing new.** It loads PEMs into Caddy's config, as it does for the panel's certificate today. The ACME client, the DNS providers and their tokens are in control-plane packages, which `cmd/dockwell/build_test.go` keeps out of the agent build. The memory is the control plane's, a few KB per certificate.
- **It lifts the one-server rule's reason for these names.** A name proven by DNS has nothing for two servers to race on. Routes on several servers for one name stay refused for now; this record makes it possible later, as ADR 0024 foresaw.

### A DNS provider is something an admin connects

A DNS provider is a resource of kind `dns.provider`, install-wide, as a registry login is, and an admin's to add, change and remove, in Settings, Integrations, under **DNS**:

```proto
message DnsProvider {
  // Which provider's API: "cloudflare" for now.
  string provider = 1;
  // The zones it may write in, such as "example.com", each checked live.
  repeated string zones = 2;
  // The provider's API token (sensitive): a Cloudflare API token with
  // Zone:Read and DNS:Edit, scoped to these zones.
  string api_token = 3;
}
```

- **The token is a secret, and never shown back.** The kind is sealed at rest like every kind with a `(sensitive)` field ([ADR 0022](0022-secrets-encrypted-at-rest.md)); plans, lists, exports and Activity name the provider and never the token. An update that leaves the token out keeps it, unless it changes the provider, which asks for it again. The CLI takes it from a file or standard input, never the command line, as for a registry login's password.
- **A live check, from the control plane, as you type.** `DomainService.CheckDnsProvider` verifies the token, lists the zones it reaches, and in each zone the provider names writes a TXT record, `_dockwell-check-<random>.<zone>`, reads it back and deletes it. It answers **Can write DNS for example.com**, **Wrong token** or **Can't write DNS**, with the provider's own words ("Cloudflare: Authentication error (10000)"), or **Can't reach Cloudflare**. Review plan goes ahead once it can write in every zone named. A test reaches only the stand-in `DOCKWELL_TEST_CLOUDFLARE` names, and no provider without one.
- **Scoped as narrowly as the provider allows.** The sheet says in one line what token to make: a Cloudflare API token with Zone:Read and DNS:Edit, for the zones you name. A Global API Key is refused by name, as it can do anything the account can. A Global API Key is refused as the provider is planned, not only by the live check. Whenever the token reaches more zones than are picked, the check says how many, with **Scope it to these zones**, which opens Cloudflare's token page. The zones a token reaches are listed for you to pick, as the check writes in each one picked; the only one is picked already.
- **Its projects, which its names may be proven for,** `projects` (slice 2), by ID. Empty is none: an admin says which projects' routes it proves.
- **Checking a saved one again is on Activity,** who checked which provider, as its token wrote in the zones.
- **Removing one is refused while a certificate uses it,** naming them, once certificates come (slice 2).
- **More providers** come as small API clients in the same package, one per provider, behind one interface: append and delete a TXT record, list zones, verify. Where a library is worth it, `libdns`'s, in the control plane alone and held to the 14-day rule.

### One key per server, by default

The default gives no server a key that serves another's names, so one stolen key takes one server's names and no more.

- **A server's routes proven by DNS share one certificate of the server's own,** for exactly their names, with a key of its own: one order for them all, as a batch, made again as the set changes.
- **A route says how its name is proven,** `proof` (120): empty for automatic, `http`, or `dns`. Automatic is DNS where a provider whose zone holds the name serves the route's project, and the plan's DNS check says HTTP can't reach the server: no record, a record elsewhere, as behind a tunnel that passes TLS on, or a server with private addresses alone. HTTP otherwise, by Caddy, as now. The plan says which, and the step carries it, so the fingerprint covers it. `dns` with no provider for the name refuses the plan, saying which to connect.
- **Proof by DNS can be had for a name that points anywhere,** which HTTP-01 can't. So only the projects a provider names may use it, and a route of another refuses the plan.
- **The first time a key goes to a server, an admin confirms it, with step-up.** The plan says so, "app.example.com is proven by DNS, and a certificate's key goes to hz-2 for the first time", as `host_access` of a new kind, `certificate_key`, which only an admin applies, confirming with their passkey ([ADR 0016](0016-scoped-api-tokens.md)). Later keys to that server, as it renews or its names change, need no one. Where no key goes, as no provider proves the name, the plan says so and asks for no passkey.
- **Up to 100 names on a server's own certificate.** A name past that gets none, and the plan says so, with the fix: a shared wildcard for its zone.

### A shared wildcard is an admin's choice

A certificate by DNS that several servers share is a resource of kind `certificate`, install-wide, an admin's to make:

```proto
// Kind "certificate". DnsCertificate, as the API's certificate state is
// Certificate.
message DnsCertificate {
  // The names it covers, such as "*.example.com" and "example.com". A
  // wildcard covers one label, as the CA issues it.
  repeated string domains = 1;
  // The DNS provider that proves them, by name.
  string dns_provider = 2;
  // The projects whose routes it serves, by ID. Empty is none.
  repeated string projects = 3;
}
```

- **Its names are checked as a route's domain is,** with the wildcard only as a whole first label, and each must be in one of the zones its provider was saved with, which the plan checks, as the provider's token is used in those zones alone.
- **Its projects' routes under it are served with it,** on whichever servers they run, in place of a server's own certificate. Its key goes to those servers, each the first time with an admin's step-up, as above: confirmed for that certificate and that server, whatever else the server has, with the plan naming the certificate's names. Empty serves none, so making one gives no server anything until an admin says which projects.
- **A wildcard is one step.** On Domains, **Add a wildcard** for a zone a provider covers makes one certificate for `*.example.com` and `example.com`, with the project it's added from, as one plan.
- **Up to 100 names each,** as Let's Encrypt allows, and 200 certificates.

### The panel's domain stays on the control server

A certificate whose names reach the panel's domain, as it's served or as it's saved, never goes to a server but the control server: one that covers it, as a wildcard over it would, one that is it, or one under it, whose key could set the panel's cookies. A route on another server under it gets that server's own certificate instead, and the plan says why. A key that serves the panel's domain is a key to sign in with passkeys there ([ADR 0024](0024-the-caddy-ingress-driver.md#who-may-claim-a-domain)), so it stays where the panel is.

### Issuing and renewing

- **One ACME account, the install's own,** apart from the panel's autocert account (`control/acme`) and from Caddy's on each server. Its key is sealed in the store ([ADR 0022](0022-secrets-encrypted-at-rest.md)), made on the first order, against Let's Encrypt or `DOCKWELL_ACME_DIRECTORY_URL`, with `DOCKWELL_ACME_ROOTS`. No email is sent to the CA. Its key rotates by ACME's key change (RFC 8555, section 7.3.5), keeping the account and what it ordered: `dockwell certificates rotate-account`, an admin's, with step-up.
- **A CAA record holds the zone to it.** The Domains page suggests one for a zone with certificates by DNS: `0 issue "letsencrypt.org; accounturi=<the account's URL>; validationmethods=dns-01"` (RFC 8657), so no other account, and no other way of proving, gets a certificate for its names.
- **A new key at every issuance,** renewals included.
- **An order proves each name by DNS-01.** Before it writes `_acme-challenge.<name>`'s TXT record, the control plane writes down the name and value it will write, and the record's ID once it has one, in the store. It waits until the zone's own nameservers answer it, up to 2 minutes, asks the CA to validate, and deletes the record whatever happens. A crash mid-order leaves the note, and the control plane deletes what it names as it next starts.
- **Renewal when the CA says,** by ACME Renewal Information (RFC 9773): the control plane asks the CA's `renewalInfo` for a window and renews inside it, and 30 days before the end, or at two thirds of a shorter life, where the CA says nothing. A failure backs off from 2 minutes to 6 hours, no more than 5 tries an hour for a set of names, under Let's Encrypt's limit on failed validations.
- **Orders are spaced and capped.** A certificate whose names changed is ordered again an hour after its last order at the soonest, so routes coming and going don't use up the CA's limits, and Dockwell orders no more than 20 certificates a week for one registered domain, under Let's Encrypt's 50, which Caddy's and the panel's share. A name whose proof fails is dropped, the rest ordered without it, and it's tried again once a day, saying why.
- **A failed delivery sends the stored certificate again,** and never orders another: what the CA issued is kept until it's replaced.
- **A failure says why, in the provider's own words or the CA's.** A provider's API error is its message and code ("Cloudflare: Authentication error (10000)"); the CA's is its problem type and detail. Each with the fix. It's the certificate's state, and the existing alert rules read it: **certificate failed** fires on a renewal that failed, in those words, and **certificate expiring** as it does for Caddy's.

### What reaches a server

- **Sealed to its agent's stream with HPKE,** as a step's values are ([ADR 0022](0022-secrets-encrypted-at-rest.md)), as it's issued, renewed or first needed, and when the agent connects.
- **The agent keeps its copy in its sealed desired-state cache** ([ADR 0054](0054-the-agent-keeps-its-desired-state.md)), and loads it into Caddy's `tls.certificates.load_pem`, tagged with the certificate's ID, listing the routes' names it covers in `automatic_https.skip_certificates`, so Caddy asks the CA for none of its own for them. Caddy keeps both in its last config, `ingress/config/autosave.json`, mode 0600, owned by Caddy's uid, in a directory of mode 0700; the agent's copy of that config, `ingress/last-good.json`, is root's, mode 0600. A reboot, an agent down or a control plane down go on serving it, as ADR 0024's principle 5 asks.
- **Each key delivery is on the server's timeline,** with the certificate and its names, and never the key: once for each key, told by its certificate's serial, whatever restarts or reconnects send it again.
- **It goes when it's no longer needed,** as the last route it serves on that server goes, or the certificate is removed or narrowed: the agent takes it out of Caddy's config, its last config, its own copy and its cache.

### Rotation and revocation

- **Rotation is the real protection.** Browsers barely check revocation: Chrome reads a small list of its own, and Let's Encrypt has ended OCSP. So every issuance has a new key, and a key lives no longer than its certificate's 90 days at most.
- **A server removed, or marked compromised,** has everything it held re-keyed and issued again, at once: each shared certificate it had goes to the servers left with a new key, and the old ones, and its own, are revoked with ACME's `revokeCert`, reason `keyCompromise`. The plan to remove a server, or to mark it compromised, says which certificates it re-issues. What an admin let it have is forgotten, so a server that joins again asks again.
- **As built,** the control plane keeps each key a server was given, by its certificate's serial, until the certificate ends, with the servers given it, even once it's replaced. A server that's revoked, or gone from the store, is taken off them: each key it had that's still in use is ordered again at once, past the hour between orders and the week's cap, and given to the servers left; then every key it had is revoked, signed with that certificate's own key, so the CA refuses the key from then on too, and each revocation is on the gone server's timeline (`certificate.revoke`). A replaced or no-longer-wanted certificate is kept apart, sealed as it was, until it's revoked or ends. A revocation the CA doesn't take is tried again with the same backoff as an order, in the CA's words. Removing a server, and marking one compromised, have no API yet: what revokes a server in the store sets this off, and the removal plan's list of what it re-issues comes with that flow.
- **This comes before any agent loads a key.** Removal's re-keying and revocation are built in slice 2b, beside the agent's loading, and an agent says it loads certificates (`Hello.loads_certificates`) only from that release, so no key reaches a server before its removal can take it back.

### Renewal with the control plane down

A certificate by DNS renews only through the control plane, which holds the token, so a control plane down for long enough lets it expire.

- **From 14 days before its end,** the agent says so in its log, once a day, and in the state it reports: "Expires in 9 days, and the control plane hasn't renewed it".
- **Within 7 days of its end, with the control plane unreachable,** the agent takes the names that HTTP-01 can reach, those its own DNS check finds pointing at it, out of `skip_certificates`, so Caddy gets their certificates itself, and puts them back once the control plane renews.
- **A wildcard's names, and those behind NAT,** can't be proven that way, so they go dark at the end, which the warnings come before.

### Backups and scope

- **A control-plane backup holds the token, the account key and every certificate's key,** sealed as the store keeps them ([ADR 0073](0073-backing-up-and-restoring-the-control-plane.md)). A backup is as sensitive as the control plane.
- **Scope below a zone is CNAME delegation.** A provider's token writes the whole zone. To give the control plane one name and no more, point `_acme-challenge.app.example.com` by CNAME at a zone of its own, as acme-dns does, and connect that. It may come as one more provider.

### Numbers

Fields this adds to messages that exist take 120 to 129: the resource oneof's `dns_provider` 120 and `certificate` 121, `Route.proof` 120, and `Certificate.by_dns` 120 in a domain's state. The agent protocol takes 320 to 329: `put_certificates` 320 in `Call` and `CallResult`, and `Hello.loads_certificates` 321, by which an agent says it loads them. New messages number from 1.

### Slices

1. DNS providers: the kind, Cloudflare, the live check, Settings, the CLI. This record.
2. Certificates by DNS, in two parts:
   1. On the control plane: a provider's projects, a route's `proof`, each server's own certificate, the shared kind, issuing and renewing, ARI, the challenge records' note, an admin's step-up for a server's first key, giving agents what they need, each delivery on the server's timeline, and `ListCertificates`.
   2. Re-keying and revoking what a server removed or marked compromised held, first; then, on the agent: `Hello.loads_certificates`, loading them into Caddy with `skip_certificates`, its sealed cache, the warnings and the fallback with the control plane down; and automatic proof, with `dns` refused where no provider proves the name.
3. The panel's defaults, and the rest: **Add a wildcard** on Domains, **By DNS** in a route's form where HTTP can't reach, the CAA suggestion, the account's key change, and alerts in the provider's words.

## Alternatives considered

- **Caddy with `caddy-dns` modules,** as ADR 0030 sketched. Above: an image of our own to build and sign for every release, and the token on every server with a route.
- **CertMagic or lego in the control plane.** Both bring a DNS library per provider, and lego's tree is large. `x/crypto/acme` is already linked, the DNS-01 flow is a few hundred lines, and a provider's API is a few calls.
- **Delegating `_acme-challenge` by CNAME to an acme-dns server.** It works for any DNS host, with no API token, but someone runs the acme-dns server. It may come later, as one more provider.
- **Wildcards shared by default.** One key would serve every app on every server under it, and the panel's domain with them; one server stolen would take them all. A server's own certificate is the default, and sharing is what an admin chooses.
- **A route naming any provider.** It would let anyone who can add a route have a name proven that points elsewhere. A route's proof by DNS goes through a provider that names its project, and the first key to a server through an admin.

## Consequences

- A server behind NAT, or one that only passes TLS on, gets real certificates, and a wildcard covers every app under it.
- A DNS token can rewrite a zone, so it lives in the control plane alone, and only an admin connects one, says which projects it proves names for, or shares a certificate.
- A key serves one server's names unless an admin shares it, never goes to a server for the first time without an admin, and never serves the panel's domain outside the control server.
- A stolen key is replaced and revoked with its server's removal; rotation at every renewal is what limits it.
- With the control plane down for weeks, certificates by DNS expire; the agent warns two weeks ahead, and hands names HTTP can reach to Caddy in the last week.
- Caddy's image stays the official one, pinned by digest.
