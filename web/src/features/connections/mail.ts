// SPDX-License-Identifier: AGPL-3.0-only

import { SmtpSecurity } from "@dockwell/api";

/**
 * The mail services a connection to a mail server is made for, each with where its
 * server is and who signs in there, as its own documentation has them: a person picks
 * the service and pastes its key and the address mail comes from (ADR 0099).
 */
export type MailProviderId = "resend" | "postmark" | "ses" | "mailgun" | "brevo" | "mail";

export interface MailProvider {
  id: MailProviderId;
  /** The service's name, which no translation changes. */
  name: string;
  /** Two letters for its tile. */
  mark: string;
  /** Its regions, where a person picks one, first the default. Empty where it has none. */
  regions: readonly string[];
  /** Its server, from the region picked; empty for another server, which a person types. */
  host: (region: string) => string;
  /** Who signs in, where the service says: Resend's is always resend. Empty where it's the person's. */
  user: string;
  /** The user is the key, as Postmark's server token is both. */
  userIsKey: boolean;
  /**
   * The ports it takes besides the usual ones, by security, as its documentation lists
   * them: the way round a server whose provider blocks 25, 587 and 465.
   */
  alts: { starttls: readonly number[]; tls: readonly number[] };
  /** It takes TLS from the start, on 465 at least: Postmark lists no such port. */
  tlsFromStart: boolean;
}

const SES_REGIONS = [
  "us-east-1",
  "us-east-2",
  "us-west-2",
  "ca-central-1",
  "eu-west-1",
  "eu-west-2",
  "eu-central-1",
  "eu-north-1",
  "ap-south-1",
  "ap-southeast-1",
  "ap-southeast-2",
  "ap-northeast-1",
  "sa-east-1",
] as const;

export const MAIL_PROVIDERS: readonly MailProvider[] = [
  {
    id: "resend",
    name: "Resend",
    mark: "Re",
    regions: [],
    host: () => "smtp.resend.com",
    user: "resend",
    userIsKey: false,
    alts: { starttls: [2587], tls: [2465] },
    tlsFromStart: true,
  },
  {
    id: "postmark",
    name: "Postmark",
    mark: "Pm",
    regions: [],
    host: () => "smtp.postmarkapp.com",
    user: "",
    userIsKey: true,
    alts: { starttls: [2525], tls: [] },
    tlsFromStart: false,
  },
  {
    id: "ses",
    name: "Amazon SES",
    mark: "SE",
    regions: SES_REGIONS,
    host: (region) => (region ? `email-smtp.${region}.amazonaws.com` : ""),
    user: "",
    userIsKey: false,
    alts: { starttls: [2587], tls: [2465] },
    tlsFromStart: true,
  },
  {
    id: "mailgun",
    name: "Mailgun",
    mark: "Mg",
    regions: ["us", "eu"],
    host: (region) => (region === "eu" ? "smtp.eu.mailgun.org" : "smtp.mailgun.org"),
    user: "",
    userIsKey: false,
    alts: { starttls: [2525], tls: [] },
    tlsFromStart: true,
  },
  {
    id: "brevo",
    name: "Brevo",
    mark: "Br",
    regions: [],
    host: () => "smtp-relay.brevo.com",
    user: "",
    userIsKey: false,
    alts: { starttls: [2525], tls: [] },
    tlsFromStart: true,
  },
  {
    id: "mail",
    name: "",
    mark: "@",
    regions: [],
    host: () => "",
    user: "",
    userIsKey: false,
    alts: { starttls: [2525, 2587], tls: [] },
    tlsFromStart: true,
  },
];

export function mailProvider(id: MailProviderId): MailProvider {
  return MAIL_PROVIDERS.find((p) => p.id === id) ?? (MAIL_PROVIDERS[MAIL_PROVIDERS.length - 1] as MailProvider);
}

export function isMailProvider(id: string): id is MailProviderId {
  return MAIL_PROVIDERS.some((p) => p.id === id);
}

/** The service a saved mail server's host is at, and its region, where a service has one. */
export function mailPlaceOf(host: string): { provider: MailProviderId; region: string } {
  const h = host.toLowerCase();
  const ses = /^email-smtp\.([a-z0-9-]+)\.amazonaws\.com$/.exec(h);
  if (ses) return { provider: "ses", region: ses[1] ?? "" };
  if (h === "smtp.eu.mailgun.org") return { provider: "mailgun", region: "eu" };
  for (const p of MAIL_PROVIDERS) {
    if (p.id !== "ses" && p.id !== "mail" && p.host("") === h) return { provider: p.id, region: p.regions[0] ?? "" };
  }
  return { provider: "mail", region: "" };
}

/** The port a security takes unless set, as the server's defaults have them. */
export function defaultPort(security: SmtpSecurity): number {
  return security === SmtpSecurity.TLS ? 465 : security === SmtpSecurity.NONE ? 25 : 587;
}

/** A mail server's host: a name or an address alone, as the server checks it. */
export const MAIL_HOST = /^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$|^[0-9a-fA-F:.]+$/;

/** An email address alone, such as notes@example.com: what the server takes as a sender. */
export const SENDER = /^[^\s@<>"]+@[^\s@<>"]+\.[^\s@<>"]+$/;

/**
 * The ports a service takes where a server can't reach the one set, for its security:
 * Resend's and Amazon SES's 2587 and 2465, Postmark's, Mailgun's and Brevo's 2525, and
 * 2525 or 2587 for another server (the SMTP review's M1).
 */
export function altPorts(p: MailProvider, security: SmtpSecurity, port: number): number[] {
  const list = security === SmtpSecurity.TLS ? p.alts.tls : p.alts.starttls;
  return list.filter((x) => x !== port);
}

/**
 * The port "Use TLS" moves a check to, or undefined where the service takes no TLS from
 * the start: the service's own TLS port for the one set, as SES's 2465 for 2587; a port
 * of its own for another server, which may take TLS on any; and 465 otherwise (the
 * re-check's N4).
 */
export function tlsPortFor(p: MailProvider, security: SmtpSecurity, port: number): number | undefined {
  if (!p.tlsFromStart) return undefined;
  const ports = (s: SmtpSecurity) => [defaultPort(s), ...(s === SmtpSecurity.TLS ? p.alts.tls : p.alts.starttls)];
  const known = ports(security).indexOf(port);
  const like = known >= 0 ? ports(SmtpSecurity.TLS)[known] : undefined;
  if (like !== undefined) return like;
  if (p.id === "mail" && known < 0) return port;
  return defaultPort(SmtpSecurity.TLS);
}

/**
 * An address's 16 bytes, IPv4 as IPv4-mapped, parsed as Go's netip.ParseAddr does:
 * dotted IPv4 with no leading zero and each part at most 255, and IPv6 with one "::"
 * at most, groups of 1 to 4 hex digits, an IPv4 tail, and a zone. Anything else, a
 * name among it, is undefined.
 */
function addressBytes(text: string): number[] | undefined {
  const v4 = (t: string): number[] | undefined => {
    const parts = t.split(".");
    if (parts.length !== 4) return undefined;
    const out: number[] = [];
    for (const part of parts) {
      if (!/^(0|[1-9][0-9]{0,2})$/.test(part) || Number(part) > 255) return undefined;
      out.push(Number(part));
    }
    return out;
  };
  const four = v4(text);
  if (four) return [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, ...four];
  if (!text.includes(":")) return undefined;
  const zone = text.indexOf("%");
  if (zone === 0 || zone === text.length - 1) return undefined;
  let t = (zone > 0 ? text.slice(0, zone) : text).toLowerCase();
  let tail: number[] = [];
  const lastColon = t.lastIndexOf(":");
  if (t.slice(lastColon + 1).includes(".")) {
    const embedded = v4(t.slice(lastColon + 1));
    if (!embedded) return undefined;
    tail = [
      ((embedded[0] as number) << 8) | (embedded[1] as number),
      ((embedded[2] as number) << 8) | (embedded[3] as number),
    ];
    // What's before the IPv4 tail, without its last colon unless that's half of "::".
    const before = t.slice(0, lastColon + 1);
    t = before.endsWith("::") ? before : before.slice(0, -1);
  }
  const halves = t.split("::");
  if (halves.length > 2) return undefined;
  const group = (g: string) => (/^[0-9a-f]{1,4}$/.test(g) ? Number.parseInt(g, 16) : Number.NaN);
  const read = (h: string) => (h === "" ? [] : h.split(":").map(group));
  const head = read(halves[0] ?? "");
  const rest = halves.length === 2 ? read(halves[1] ?? "") : [];
  const groups = [...head, ...rest];
  if (groups.some(Number.isNaN)) return undefined;
  const room = 8 - tail.length;
  let words: number[];
  if (halves.length === 2) {
    if (groups.length > room - 1) return undefined;
    words = [...head, ...new Array(room - groups.length).fill(0), ...rest];
  } else {
    if (groups.length !== room) return undefined;
    words = groups;
  }
  words = [...words, ...tail];
  return words.flatMap((w) => [w >> 8, w & 0xff]);
}

/**
 * Whether a host is a private address, loopback or shared address space, as the server
 * judges a host a password may go to without TLS: parsed as the server parses it, and
 * judged by its range alone, never a name (the SMTP review's H1, its re-check's N2).
 */
export function privateAddress(host: string): boolean {
  const b = addressBytes(host.replace(/^\[|\]$/g, ""));
  if (!b) return false;
  const mapped = b.slice(0, 10).every((x) => x === 0) && b[10] === 0xff && b[11] === 0xff;
  if (mapped) {
    const [a, c] = [b[12] as number, b[13] as number];
    return (
      a === 10 ||
      a === 127 ||
      (a === 172 && c >= 16 && c <= 31) ||
      (a === 192 && c === 168) ||
      (a === 100 && c >= 64 && c <= 127)
    );
  }
  const loopback = b.slice(0, 15).every((x) => x === 0) && b[15] === 1;
  return loopback || ((b[0] as number) & 0xfe) === 0xfc;
}

/**
 * Whether a host is on this server or its private network, matching alert.Nearby
 * on the control plane: IP literals are judged by their range alone, names by
 * single-label or private suffixes.
 */
export function nearbyHost(host: string): boolean {
  const bare = host
    .trim()
    .toLowerCase()
    .replace(/^\[|\]$/g, "");
  if (bare === "") return false;
  if (addressBytes(bare)) return privateAddress(bare);
  if (bare === "localhost" || !bare.includes(".")) return true;
  return bare.endsWith(".internal") || bare.endsWith(".lan") || bare.endsWith(".local") || bare.endsWith(".home.arpa");
}

/** Steps a mail server's check stops at, as the agent names them. */
export const MAIL_STEPS: readonly string[] = ["connect", "hello", "tls", "sign-in"];
