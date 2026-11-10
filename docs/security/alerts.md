# Alert channels: threat model

An alert channel makes the control plane send to an address a person typed: a webhook, an SMTP server, an ntfy server, Slack, Discord or Telegram. This says what that protects, from whom, what's refused, and what it doesn't protect. The design is in [ADR 0044](../adr/0044-alerts.md), beside the threat model for [secrets](secrets.md). The code is in `internal/resource/alert` (what a channel may say) and `internal/control/alerts` (how it's sent).

## What there is to protect

| Asset | Where it lives | Why it matters |
| --- | --- | --- |
| What signs in to each channel: a webhook's URL and secret, an SMTP password, an ntfy topic and token, a Slack or Discord webhook's URL, a Telegram bot's token | Sealed in the channel's spec ([ADR 0022](../adr/0022-secrets-encrypted-at-rest.md)) | Each lets someone post as the install, or read what it sends |
| The cloud's metadata service | A link-local address, such as `169.254.169.254` | It hands out the machine's own credentials to whatever asks from it |
| What alerts say | Each message | Server and app names, and what's wrong with them |

## Who we consider

| Who | What they have |
| --- | --- |
| An API token that can make changes | A bearer secret from a CI log or a dotfile ([ADR 0016](../adr/0016-scoped-api-tokens.md)). It can plan, apply and test channels |
| A stolen session | A person's cookie. It can do what the token can |
| Someone on the path to a channel's server | They read and change what crosses it |
| A channel's far end | It answers as it likes, and may be someone else's once a URL is changed |

A signed-in person can make a channel send anywhere their install can reach, as they can make an app that does. Nothing here defends against them.

## What's refused

Every request to an address a person gave goes through these rules, for every channel and for tests alike. They're `internal/netguard`'s, the guard every feature that reaches an address a person names shares (#341), with what a channel may reach in `alert.Guard`: this server and its private networks as well as the internet.

| Refused | How | Tested |
| --- | --- | --- |
| A link-local, multicast, unspecified, broadcast or reserved address, or where a cloud keeps its metadata service (`169.254.169.254`, `100.100.100.200`, `fd00:ec2::254`, `metadata.google.internal`), written out, mapped or through NAT64 | When the channel is planned, and when it's tested | `TestEachKindOfChannel`, `TestAWebhookChannel`, netguard's `TestWhatEachAddressIs` |
| A proxy from the environment | Never used: a channel's requests go straight to the address, so the dial check sees where they go | `TestAWebhookGoesThroughNoProxy` |
| A name that resolves to one | As each connection is made: the dialer checks the address it's about to connect to, so a name that resolves elsewhere when it's planned gets nothing either. | `TestNoChannelReachesLinkLocal`, netguard's `TestANameIsCheckedWhereItLeads` |
| Plain HTTP, or email without TLS, to an address that isn't on this server or its private network | When planned, by the name, and again as each connection is made, by the address it led to, so a name a search domain sends to the internet gets nothing in the clear. Email with STARTTLS stops when the server doesn't offer it, before anything is sent | `TestEachKindOfChannel`, `TestAnEmail`, `TestWithoutTLSNothingCrossesTheInternet` |
| A certificate that isn't valid for the name | As TLS starts: nothing is sent, a password least of all | `TestAnEmail` |
| A password over a connection without TLS | When planned, and again before signing in | `TestEachKindOfChannel` |
| A user and password in a URL | When planned | `TestAWebhookChannel` |
| A redirect | Never followed: the far end's answer says it moved | netguard's `TestNoRedirectIsFollowed` |
| A slow far end, or a large answer | 10 seconds a try, and 64 KiB read back, past which the read fails | netguard's `TestAnAnswerIsCapped` |
| Reading what an internal service answers | Why a far end refused a message is read back only from each kind's own service: Slack's `hooks.slack.com`, Discord's hosts, `ntfy.sh`, and Telegram's API. Anything else, a webhook and a self-hosted ntfy included, is said by its status alone | `TestTheChatChannels` |
| What signs in, echoed back | Each sealed value of the channel, and its URL's path and query, are taken out of what a far end said before anything keeps it | `TestWhatAFarEndSaysHoldsNothingThatSignsIn` |
| Many tests | Six a minute for each person and each token; every test goes on Activity, with who sent it, the channel's type and host, and whether it arrived | `TestTestingAChannel` |

## What each can and can't do

### A token that can make changes

| It tries to | What happens |
| --- | --- |
| Read what signs in to a channel | It can't. No plan, list, resource, event or log holds it; a list names the fields a channel keeps sealed, and a form gets them empty. Tested for every type in `TestEveryChannelKeepsWhatSignsInSealed` |
| Point a channel at a server of its own, keeping the password the channel has, and send a test | Refused. What's kept stays with where it signed in: the same webhook host, the same SMTP server, port, security and user, or the same ntfy server. Moving any of them asks for it again, in a plan and in a test. Tested |
| Reach the metadata service through a channel | Refused, as above |
| Reach something on the private network, such as an admin page on `10.0.0.5` | It can send it a `POST`. A webhook to a private address is what many installs want. It reads back only the status, and a line of the reason for Slack, Discord, ntfy and Telegram |
| Learn what a far end said that signs in to it | A webhook's answer is kept as its status alone, as its body is its own. Nothing Dockwell says of a failure holds a URL |
| Inject a header into an email with a title or an address | It can't. The subject is one line, Q-encoded; every address is parsed, and a line break in one is refused. Tested |
| Mention everyone in a Discord channel through an alert's title | It can't. Messages to Discord allow no mentions. Tested |

### Someone on the path

| It tries to | What happens |
| --- | --- |
| Read an alert or a password in flight | Everything to an address that isn't on this server or its private network uses HTTPS or TLS, with the certificate verified |
| Strip STARTTLS from an SMTP server's answer | Dockwell sends nothing to a server that doesn't offer it, so a stripped answer gets nothing |

## What this doesn't do

- **A private network is trusted.** Plain HTTP and SMTP without TLS are allowed to an address on this server or its private network, and so is a request to any service there. A channel is made and tested only by a person or a token that can make changes.
- **No proxy, for now.** `HTTPS_PROXY` and `HTTP_PROXY` are ignored for channels, as through a proxy the dial check would see the proxy alone. An install whose control plane reaches the internet only through one can't send to an outside channel yet; if one needs to, it becomes a setting of its own.
- **What a message says is readable by whoever runs the far end.** ntfy.sh, Slack, Discord and Telegram see each alert. Use a server of your own where that matters.
