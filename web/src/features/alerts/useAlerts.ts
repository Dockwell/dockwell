// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type AlertChannelInfo,
  type AlertDelivery,
  type AlertRuleInfo,
  AlertService,
  AlertRule_Severity as Severity,
} from "@dockwell/api";
import type { IconName, Status } from "@dockwell/ui";
import type { MessageDescriptor } from "@lingui/core";
import { msg } from "@lingui/core/macro";
import { useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";

/** The channels, by name. */
export function useChannels() {
  return useQuery(AlertService.method.listAlertChannels, {});
}

/** The rules, in the order Dockwell lists them. */
export function useRules() {
  return useQuery(AlertService.method.listAlertRules, {});
}

/** What's firing now, and what fired and resolved after it. */
export function useAlerts() {
  return useQuery(AlertService.method.listAlerts, {});
}

/** The newest messages to every channel. */
export function useDeliveries() {
  return useQuery(AlertService.method.listAlertDeliveries, { pageSize: 50 });
}

/** The kinds of channel, as the API names them. */
export type ChannelType = "webhook" | "email" | "ntfy" | "slack" | "discord" | "telegram";

export const CHANNEL_TYPES: readonly ChannelType[] = ["email", "slack", "discord", "telegram", "ntfy", "webhook"];

/** Each kind of channel's name and mark. */
export const CHANNEL_KIND: Readonly<Record<ChannelType, { label: MessageDescriptor; icon: IconName }>> = {
  webhook: { label: msg`Webhook`, icon: "link" },
  email: { label: msg`Email`, icon: "mail" },
  ntfy: { label: msg`ntfy`, icon: "bell" },
  slack: { label: msg`Slack`, icon: "chat" },
  discord: { label: msg`Discord`, icon: "chat" },
  telegram: { label: msg`Telegram`, icon: "chat" },
};

export function isChannelType(type: string): type is ChannelType {
  return Object.hasOwn(CHANNEL_KIND, type);
}

/** How a channel's last message went, as a status. */
export function channelStatus(c: AlertChannelInfo): { status: Status; word: MessageDescriptor } {
  if (!c.last) return { status: "unknown", word: msg`Nothing sent yet` };
  return c.last.ok ? { status: "healthy", word: msg`Delivered` } : { status: "down", word: msg`Failing` };
}

/** The channels each severity's rules send to: a rule that names none sends to every one. */
export function routing(
  rules: readonly AlertRuleInfo[],
  channels: readonly AlertChannelInfo[],
): { severity: Severity; channels: string[]; rules: number }[] {
  return [Severity.CRITICAL, Severity.WARNING].map((severity) => {
    const mine = rules.filter((r) => r.spec?.severity === severity && !r.spec?.disabled);
    const names = new Set<string>();
    for (const r of mine) {
      const named = r.spec?.channels ?? [];
      for (const c of named.length > 0 ? named : channels.map((c) => c.name)) names.add(c);
    }
    return { severity, channels: channels.map((c) => c.name).filter((n) => names.has(n)), rules: mine.length };
  });
}

/** Sends a test through a channel by its name, and says how it went in a line. */
export function useSendTest() {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const client = useMemo(() => createClient(AlertService, transport), [transport]);
  const [sending, setSending] = useState<string>();
  const [last, setLast] = useState<{ name: string; delivery?: AlertDelivery; error?: unknown }>();
  const send = async (name: string) => {
    setSending(name);
    setLast(undefined);
    try {
      const res = await client.testAlertChannel({ name });
      setLast({ name, delivery: res.delivery });
    } catch (error) {
      setLast({ name, error });
    } finally {
      setSending(undefined);
      void queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: AlertService, cardinality: undefined }),
      });
    }
  };
  return { sending, last, send, clear: () => setLast(undefined) };
}
