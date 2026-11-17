// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import {
  type CheckConnectionResponse,
  ConnectionService,
  LinkPart,
  Connection as ServerConnection,
} from "@dockwell/api";
import { StatusBadge } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useMemo } from "react";

import { useAllResources } from "../projects/useProjects";
import { MAIL_STEPS } from "./mail";

/** Every connection the caller may see, or those an environment's project may use. */
export function useConnections(environment = "") {
  return useQuery(ConnectionService.method.listConnections, { environment });
}

/**
 * The server a check runs from unless a person picks one: the control server, which
 * holds the keys already, where it's connected; then any connected server.
 */
export function checkServer(
  servers: readonly { id: string; connection: ServerConnection; control?: boolean | undefined }[],
): string {
  const up = servers.filter((s) => s.connection === ServerConnection.CONNECTED);
  return (up.find((s) => s.control) ?? up[0] ?? servers[0])?.id ?? "";
}

/** The probe's own codes for what kept it from an answer, as the agent says them. */
const NO_ANSWER = ["refused", "timeout", "unreachable", "tls", "mismatch"] as const;

/**
 * The codes of a check that got no answer from the bucket, rather than a refusal: a
 * check from another network, as an app's, may still reach it. "refused" isn't one:
 * it's an address Dockwell keeps every check and app from.
 */
export const SILENT: readonly string[] = ["timeout", "unreachable"];

/**
 * What a check found, as a state and a line: Reached from a server, or the fact and
 * its fix in the words a person acts on, never the bucket's own body. A mail server's
 * says what stopped it by its reply code alone (ADR 0099).
 */
export function useCheckWords() {
  const { t } = useLingui();
  const mailWords = useMailCheckWords();
  return (c: CheckConnectionResponse, mail?: MailCheck): { passed: boolean; state: string; line: string } => {
    if (mail || MAIL_STEPS.includes(c.step)) return mailWords(c, mail ?? {});
    if (c.passed) {
      return {
        passed: true,
        state: c.server ? t`Reached from ${c.server}` : t`Reached`,
        line: c.region ? t`It answered from region ${c.region}.` : t`Wrote, read and deleted a test object.`,
      };
    }
    const code = c.code;
    const said = c.words || c.message;
    switch (code as (typeof NO_ANSWER)[number] | string) {
      case "refused":
        return {
          passed: false,
          state: t`Address refused`,
          line: `${said ? `${said}. ` : ""}${t`No app can use it either. Give the address apps reach the store at.`}`,
        };
      case "timeout":
        return { passed: false, state: t`No answer`, line: t`Check the endpoint, and that the server can reach it.` };
      case "unreachable":
        return { passed: false, state: t`Can't reach it`, line: said || t`Check the endpoint.` };
      case "tls":
        return { passed: false, state: t`Untrusted certificate`, line: t`Check the endpoint's address.` };
      case "mismatch":
        return {
          passed: false,
          state: t`Answered wrongly`,
          line: t`The bucket gave back something other than what was written.`,
        };
      case "SignatureDoesNotMatch":
      case "InvalidAccessKeyId":
        return { passed: false, state: t`Keys refused`, line: t`Check the access key ID and its secret.` };
      case "AccessDenied":
        return { passed: false, state: t`Access denied`, line: t`The keys may not write in this bucket.` };
      case "NoSuchBucket":
        return { passed: false, state: t`No such bucket`, line: t`Check the bucket's name.` };
    }
    return {
      passed: false,
      state: t`Refused`,
      line: said ? `${code || c.status}: ${said}` : t`The bucket refused the check with ${code || String(c.status)}.`,
    };
  };
}

/** What the panel knows of a mail server's check beyond its answer. */
export interface MailCheck {
  /** The port it checked, where known. */
  port?: number | undefined;
  /** It was sent a user to sign in as: only then does a pass say it took one. */
  signedIn?: boolean | undefined;
}

/**
 * What a check of a mail server found: Reached from a server, which sent nothing, or
 * what stopped it, what that means, and the fix.
 */
function useMailCheckWords() {
  const { t } = useLingui();
  const stepWords: Record<string, string> = {
    connect: t({ message: "connecting", context: "a step of a mail server's check" }),
    hello: t({ message: "greeting", context: "a step of a mail server's check" }),
    tls: t({ message: "TLS", context: "a step of a mail server's check" }),
    "sign-in": t({ message: "sign-in", context: "a step of a mail server's check" }),
  };
  return (c: CheckConnectionResponse, m: MailCheck): { passed: boolean; state: string; line: string } => {
    if (c.passed) {
      return {
        passed: true,
        state: c.server ? t`Reached from ${c.server}` : t`Reached`,
        // Only a check sent a user signed in (the SMTP review's L2).
        line: m.signedIn
          ? t`It took the sign-in, and Dockwell sent no mail.`
          : t`It answered, and Dockwell sent no mail.`,
      };
    }
    const said = c.words;
    const port = m.port ? String(m.port) : "";
    // It took the connection and said nothing, as a server that wants TLS from the
    // start does to a plain one (the SMTP review's L3).
    if (c.code === "timeout" && c.step === "hello") {
      return {
        passed: false,
        state: t`No greeting`,
        line: port
          ? t`It took the connection on port ${port} and answered nothing, so apps can't send through it. That port may want TLS from the start.`
          : t`It took the connection and answered nothing, so apps can't send through it. The port may want TLS from the start.`,
      };
    }
    if (c.code === "timeout" && c.step === "tls") {
      return {
        passed: false,
        state: t`TLS never finished`,
        line: t`It took the connection and never finished TLS, so apps can't send through it. The port may want STARTTLS.`,
      };
    }
    switch (c.code) {
      case "refused":
        return {
          passed: false,
          state: t`Address refused`,
          line: `${said ? `${said}. ` : ""}${t`No app can use it either. Give the address apps reach the server at.`}`,
        };
      case "timeout":
        return {
          passed: false,
          state: port ? t`Can't reach port ${port}` : t`No answer`,
          line: port
            ? t`This server can't reach port ${port}, so apps on it can't send mail through it.`
            : t`This server can't reach the port, so apps on it can't send mail through it.`,
        };
      case "unreachable":
        return { passed: false, state: t`Can't reach it`, line: said || t`Check the host and port.` };
      case "tls":
        return { passed: false, state: t`Untrusted certificate`, line: t`Check the host.` };
      case "no-starttls":
        return {
          passed: false,
          state: t`No STARTTLS`,
          line: t`Use TLS on port 465, or a server that offers STARTTLS.`,
        };
      case "clear":
        return {
          passed: false,
          state: t`Password kept back`,
          line: t`Without TLS, Dockwell sends a password only to a private address. Use STARTTLS or TLS.`,
        };
      case "no-sign-in":
        return {
          passed: false,
          state: t`Signs no one in`,
          line: t`Clear the user, or use a server that signs one in.`,
        };
      case "mechanisms":
        return {
          passed: false,
          state: t`Can't sign in`,
          line: t`It signs in only in ways Dockwell doesn't, which are PLAIN and LOGIN.`,
        };
    }
    const code = c.code || t`no code`;
    if (c.step === "sign-in") {
      return { passed: false, state: t`Sign-in refused`, line: t`It answered ${code}. Check the user and password.` };
    }
    const step = stepWords[c.step] ?? c.step;
    return { passed: false, state: t`Refused`, line: t`It refused at its ${step} step with ${code}.` };
  };
}

/** A check's state, as a badge: healthy where it passed, down where the bucket refused, unknown where nothing answered. */
export function CheckBadge({ check }: { check: CheckConnectionResponse }) {
  const words = useCheckWords()(check);
  const silent = SILENT.includes(check.code);
  return (
    <StatusBadge status={words.passed ? "healthy" : silent ? "unknown" : "down"} label={words.state} labelIsState />
  );
}

/** What the panel calls a project, by its ID: its display name, or its name. */
export function useProjectLabel(): (id: string) => string {
  const resources = useAllResources();
  return (id: string) => {
    const p = (resources.data ?? []).find((r) => r.id === id && r.resource?.kind === "project");
    return p?.displayName || p?.resource?.name || id;
  };
}

/** What each part a link sets is called, short, as the page lists them. */
export function usePartWords(): Record<number, string> {
  const { t } = useLingui();
  return {
    [LinkPart.ENDPOINT]: t`endpoint`,
    [LinkPart.REGION]: t`region`,
    [LinkPart.BUCKET]: t`bucket`,
    [LinkPart.ACCESS_KEY_ID]: t`access key ID`,
    [LinkPart.SECRET_KEY]: t`secret access key`,
    [LinkPart.PUBLIC_URL]: t`public address`,
    [LinkPart.PATH_STYLE]: t`bucket in the path`,
    [LinkPart.KEY_PREFIX]: t({ message: "prefix", context: "where in a bucket an app keeps its files" }),
    [LinkPart.HOST]: t({ message: "host", context: "a mail server's" }),
    [LinkPart.PORT]: t({ message: "port", context: "a mail server's" }),
    [LinkPart.USER]: t({ message: "user", context: "who signs in to a mail server" }),
    [LinkPart.PASSWORD]: t({ message: "password", context: "a mail server's" }),
    [LinkPart.URL]: t({ message: "URL", context: "a mail server's, with its sign-in" }),
    [LinkPart.SECURITY]: t({ message: "security", context: "how a mail server's connection is secured" }),
    [LinkPart.SENDER]: t`sender`,
    [LinkPart.SENDER_NAME]: t`sender's name`,
  };
}

/** A name of an environment a link sets from a connection. */
export interface LinkedName {
  connection: string;
  link: string;
  part: LinkPart;
}

/**
 * The names an environment's links to connections set, by name: each reads its
 * connection through the link, so it changes there, not as a value of its own.
 */
export function useLinkedNames(environment: string): ReadonlyMap<string, LinkedName> {
  const query = useConnections(environment);
  return useMemo(() => {
    const out = new Map<string, LinkedName>();
    for (const c of query.data?.connections ?? []) {
      for (const u of c.uses) {
        if (u.environment !== environment) continue;
        for (const [name, part] of Object.entries(u.sets)) out.set(name, { connection: c.name, link: u.link, part });
      }
    }
    return out;
  }, [query.data, environment]);
}
