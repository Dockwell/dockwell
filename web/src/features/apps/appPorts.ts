// SPDX-License-Identifier: AGPL-3.0-only

import { Protocol, type ServiceInfo } from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";

import { appOf } from "./app";

/**
 * Container ports known for something other than web pages, by what serves on them,
 * as internal/control/services/webport.go's elsewhere has them: a domain sent to one
 * reaches no web page.
 */
const KNOWN: Record<number, string> = {
  21: "FTP",
  22: "SSH",
  25: "SMTP",
  53: "DNS",
  110: "POP3",
  143: "IMAP",
  389: "LDAP",
  465: "SMTP",
  587: "SMTP",
  636: "LDAP",
  993: "IMAP",
  995: "POP3",
  1433: "SQL Server",
  1521: "Oracle",
  1883: "MQTT",
  3306: "MySQL",
  4369: "Erlang",
  5432: "PostgreSQL",
  5672: "RabbitMQ",
  6379: "Redis",
  8883: "MQTT",
  9042: "Cassandra",
  11211: "Memcached",
  27017: "MongoDB",
};

/** What serves on a port that isn't a web page's, such as "SSH" for 22, or "". */
export function knownFor(port: number): string {
  return KNOWN[port] ?? "";
}

/** A TCP port in an app's container, and what's known of it. */
export interface AppPort {
  port: number;
  /** It serves the app's web page, as the control plane says (ServiceInfo.web_port). */
  web: boolean;
  /** The server's port it's published on, or 0. */
  published: number;
}

/** "3000/tcp" or "3000" as a TCP port, or 0. */
function exposedPort(expose: string): number {
  const [port = "", protocol = "tcp"] = expose.split("/");
  const n = Number(port);
  return protocol === "tcp" && Number.isInteger(n) && n > 0 && n < 65536 ? n : 0;
}

/**
 * The TCP ports an app is known to listen on, from its spec: the one that serves its
 * web page first, whether or not it's published, then what it publishes, then what it
 * exposes beyond that. What its image exposes isn't in the spec.
 */
export function appPortsOf(service: ServiceInfo | undefined): AppPort[] {
  const app = service ? appOf(service) : undefined;
  if (!service || !app || app.hostNetwork) return [];
  const out: AppPort[] = [];
  const add = (port: number, published = 0) => {
    if (port <= 0) return;
    const had = out.find((p) => p.port === port);
    if (had) {
      if (!had.published) had.published = published;
      return;
    }
    out.push({ port, web: port === service.webPort, published });
  };
  add(service.webPort);
  for (const p of app.ports) {
    if (p.protocol !== Protocol.UDP) add(p.containerPort, p.hostPort);
  }
  for (const e of app.expose) add(exposedPort(e));
  return out;
}

/**
 * A port as a picker lists it: "3000 · web page", "22 · SSH", or "8080". What a port is
 * known for wins, as the web port falls back to the first published port, which may be
 * Redis's 6379 for Redis alone.
 */
export function usePortLabel(): (port: AppPort) => string {
  const { t } = useLingui();
  return (p) => {
    const known = knownFor(p.port);
    if (known) return `${p.port} · ${known}`;
    return p.web ? t`${p.port} · web page` : String(p.port);
  };
}
