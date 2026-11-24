// SPDX-License-Identifier: AGPL-3.0-only

/** An HTTP candidate for a published TCP port. Loopback bindings stay local to the server. */
export function publishedPortUrl(bind: string, port: number, serverAddress?: string): string | undefined {
  if (!port || port > 65535) return undefined;
  const ip = bind.replace(/^\[|\]$/g, "");
  if (ip === "::1" || ip.startsWith("127.") || ip.startsWith("::ffff:127.")) return undefined;
  const host = !ip || ip === "0.0.0.0" || ip === "::" ? serverAddress : ip;
  if (!host || /[\s/\\@?#]/.test(host)) return undefined;
  const address = host.replace(/^\[|\]$/g, "");
  try {
    const url = new URL(`http://${address.includes(":") ? `[${address}]` : address}:${port}`);
    if (url.hostname === "localhost" || url.hostname.startsWith("127.") || url.hostname === "[::1]") return undefined;
    return url.href;
  } catch {
    return undefined;
  }
}
