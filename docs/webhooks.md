# Receiving alerts by webhook

An alert channel of the webhook kind sends each alert, and each that resolves, to a URL of yours as a JSON `POST`. This page says what it sends, and how to check that a request came from your install. The design is in [ADR 0044](adr/0044-alerts.md).

## What arrives

```json
{
  "version": 1,
  "status": "firing",
  "rule": { "name": "disk-full", "title": "Disk above 90%", "severity": "warning", "condition": "disk.percent > 90" },
  "title": "web-1's disk / is 93% full",
  "text": "web-1's disk / is 93% full\nIt's over 90%. …",
  "alerts": [
    {
      "id": "alt_01kat4zr6e3v0y5xbkzb5d9q6m",
      "status": "firing",
      "title": "web-1's disk / is 93% full",
      "detail": "It's over 90%. …",
      "facts": { "server": "web-1", "where": "/", "value": 93 },
      "server_id": "srv_01kat4zr6e3v0y5xbkzb5d9q6n",
      "opened_at": "2026-11-08T12:00:00Z",
      "url": "https://panel.example.com/servers/srv_01kat4zr6e3v0y5xbkzb5d9q6n"
    }
  ],
  "sent_at": "2026-11-08T12:00:30Z",
  "dockwell": "v0.2.1"
}
```

- `status` is `firing`, `resolved` or `test`. A message carries one rule's alerts that fired, or resolved, together.
- `title` and `text` are English, ready to show. `facts` holds what a title says apart, for a receiver that words it its own way.
- An alert closed because its rule was removed or turned off has `"ended": "removed"` or `"disabled"`.
- A message can arrive twice, as Dockwell sends again whatever it isn't sure arrived: after a restart, or when your endpoint didn't answer in time. Each alert's `id` and `status` tell a message you've seen from a new one. A firing alert is sent again as its rule's repeat says, with the same `id`.
- Answer with any 2xx. Anything else, or no answer in 10 seconds, and Dockwell tries again: after 10 seconds, 30, a minute, 2, 5, 10, then every 15 minutes, for 24 hours.

## Checking the signature

With a secret on the channel, each request has a `Dockwell-Signature` header:

```
Dockwell-Signature: t=1794139230,v1=5f0c…9a1e
```

`t` is when it was sent, in Unix seconds, and `v1` is the hex HMAC-SHA256, under your secret, of `t`, a full stop, and the request's body, byte for byte. To check a request:

1. Read the body as bytes, before any JSON parsing changes it.
2. Split the header at the comma, and take `t` and `v1`.
3. Refuse a `t` more than 5 minutes from your clock, so a request someone recorded can't be sent to you again later.
4. Compute the HMAC-SHA256 of `t + "." + body` under your secret, and compare it with `v1` in constant time, with `hmac.Equal` or `crypto.timingSafeEqual`, never `==`, which says through its timing how much of a guess was right.

In Go:

```go
func fromDockwell(r *http.Request, secret []byte) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, false
	}
	var t, v1 string
	for _, part := range strings.Split(r.Header.Get("Dockwell-Signature"), ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			t = v
		case "v1":
			v1 = v
		}
	}
	sent, err := strconv.ParseInt(t, 10, 64)
	if err != nil || math.Abs(time.Since(time.Unix(sent, 0)).Minutes()) > 5 {
		return nil, false
	}
	got, err := hex.DecodeString(v1)
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(t + "."))
	mac.Write(body)
	return body, hmac.Equal(got, mac.Sum(nil))
}
```

In Node:

```js
import crypto from "node:crypto";

function fromDockwell(header, body, secret) {
  const parts = Object.fromEntries(header.split(",").map((p) => p.split("=")));
  if (Math.abs(Date.now() / 1000 - Number(parts.t)) > 300) return false;
  const want = crypto.createHmac("sha256", secret).update(`${parts.t}.`).update(body).digest();
  const got = Buffer.from(parts.v1 ?? "", "hex");
  return got.length === want.length && crypto.timingSafeEqual(got, want);
}
```

Without a secret, nothing is signed: use one whenever the URL can be reached by anyone else. A token in the URL's path or query works too, as Dockwell keeps the whole URL sealed and lists show only its host.
