// SPDX-License-Identifier: AGPL-3.0-only

import { type AlertChannelInfo, type ConnectionInfo, LinkKind, SmtpSecurity } from "@dockwell/api";

import { nearbyHost } from "../connections/mail";
import { appsOf, type ReadingApp } from "../connections/storage";

/** The mail connection an email alert channel sends through, or "" when inline. */
export function channelConnection(channel: AlertChannelInfo): string {
  return channel.channel?.to.case === "email" ? channel.channel.to.value.connection : "";
}

/** Alert channels that send through the named mail connection. */
export function channelsOn(connectionName: string, channels: readonly AlertChannelInfo[]): AlertChannelInfo[] {
  return channels.filter((c) => channelConnection(c) === connectionName);
}

/** Why a mail connection can or can't send alert email right now (ADR 0099, #833). */
export type MailPickerStatus =
  | { state: "available" }
  | { state: "serves-app"; apps: ReadingApp[] }
  | { state: "shared" }
  | { state: "held" }
  | { state: "no-tls" };

/** Whether a mail connection can take alert email right now, or why it can't. */
export function mailPickerStatus(connection: ConnectionInfo, currentConnection?: string): MailPickerStatus {
  if (currentConnection && connection.name === currentConnection) return { state: "available" };
  if (connection.uses.length > 0) return { state: "serves-app", apps: appsOf(connection) };
  if (connection.shared) return { state: "shared" };
  if (connection.held) return { state: "held" };
  if (connection.security === SmtpSecurity.NONE && connection.host !== "" && !nearbyHost(connection.host)) {
    return { state: "no-tls" };
  }
  return { state: "available" };
}

/** Default From value derived from a mail connection's sender fields. */
export function mailConnectionSender(connection: ConnectionInfo): string {
  if (!connection.sender) return "";
  return connection.senderName ? `${connection.senderName} <${connection.sender}>` : connection.sender;
}

/** The first usable mail connection's name, or "" when none can take alerts yet. */
export function firstAvailableMailConnection(connections: readonly ConnectionInfo[]): string {
  const usable = connections.find((c) => c.kind === LinkKind.SMTP && mailPickerStatus(c).state === "available");
  return usable?.name ?? "";
}
