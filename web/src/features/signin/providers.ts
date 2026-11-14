// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { DomainService, type SigninProviderInfo } from "@dockwell/api";

/** The sign-in providers the caller may see: every one for an admin, by name below (ADR 0087). */
export function useSigninProviders(project = "", enabled = true) {
  return useQuery(DomainService.method.listSigninProviders, { project }, { enabled, staleTime: 30_000 });
}

/** An issuer's host, as typed or as saved: "auth.example.com" from "https://auth.example.com/application/o/x/". */
export function issuerHost(issuer: string): string {
  const typed = issuer.trim().toLowerCase();
  if (typed === "") return "";
  try {
    return new URL(typed.includes("://") ? typed : `https://${typed}`).host;
  } catch {
    return "";
  }
}

/**
 * Whether anyone can have an account at an issuer, as at Google or Entra's common
 * one, as internal/resource/signinprovider's Public says.
 */
export function publicIssuer(issuer: string): boolean {
  let url: URL;
  try {
    url = new URL(issuer);
  } catch {
    return false;
  }
  const host = url.hostname.toLowerCase();
  if (["accounts.google.com", "appleid.apple.com", "gitlab.com", "login.live.com"].includes(host)) return true;
  if (
    [
      "login.microsoftonline.com",
      "login.microsoftonline.us",
      "login.partner.microsoftonline.cn",
      "sts.windows.net",
    ].includes(host)
  ) {
    const tenant = url.pathname.toLowerCase().replace(/^\/+/, "").split("/")[0] ?? "";
    return ["", "common", "organizations", "consumers", "9188040d-6c67-4c5b-b112-36a304b66dad"].includes(tenant);
  }
  return false;
}

/** The domains people most often have a free address at, as internal/resource/signinprovider's Mailbox lists them. */
const MAILBOXES = new Set([
  "gmail.com",
  "googlemail.com",
  "outlook.com",
  "hotmail.com",
  "live.com",
  "msn.com",
  "hotmail.co.uk",
  "outlook.de",
  "hotmail.fr",
  "yahoo.com",
  "ymail.com",
  "yahoo.co.uk",
  "yahoo.co.jp",
  "yahoo.fr",
  "yahoo.de",
  "icloud.com",
  "me.com",
  "mac.com",
  "aol.com",
  "proton.me",
  "protonmail.com",
  "pm.me",
  "tutanota.com",
  "tuta.io",
  "gmx.com",
  "gmx.net",
  "gmx.de",
  "web.de",
  "mail.com",
  "zoho.com",
  "fastmail.com",
  "hey.com",
  "yandex.com",
  "yandex.ru",
  "mail.ru",
  "qq.com",
  "163.com",
  "126.com",
  "foxmail.com",
  "sina.com",
  "naver.com",
]);

/** Whether anyone can sign up for an address at a domain, so anyone at it is anyone at all. */
export function mailboxDomain(domain: string): boolean {
  return MAILBOXES.has(domain.trim().toLowerCase().replace(/^@/, "").replace(/\.$/, ""));
}

/** A free name for a provider at host, as a route names it: "auth-example-com", then "-2" on. */
export function freeProviderName(host: string, taken: readonly SigninProviderInfo[]): string {
  const clean =
    host
      .toLowerCase()
      .replace(/:\d+$/, "")
      .replace(/[^a-z0-9-]+/g, "-")
      .replace(/^-+|-+$/g, "")
      .slice(0, 40)
      .replace(/-+$/, "") || "sign-in";
  const start = /^[a-z]/.test(clean) ? clean : `p-${clean}`;
  const names = new Set(taken.map((p) => p.name));
  if (!names.has(start)) return start;
  for (let i = 2; ; i++) if (!names.has(`${start}-${i}`)) return `${start}-${i}`;
}

/** Scopes as typed, in one field: split on spaces and commas, without those sign-in always asks for. */
export function scopesOf(typed: string): string[] {
  const asked = new Set(["openid", "email", "profile"]);
  const out: string[] = [];
  for (const scope of typed.split(/[\s,]+/)) {
    if (scope !== "" && !asked.has(scope) && !out.includes(scope)) out.push(scope);
  }
  return out;
}
