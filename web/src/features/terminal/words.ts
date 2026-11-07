// SPDX-License-Identifier: AGPL-3.0-only

import { type TerminalEnd, TerminalEndReason } from "@dockwell/api";
import type { Tone } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

/** Why a terminal ended, for people. */
export function useEndWords() {
  const { t } = useLingui();
  return (end: TerminalEnd | undefined): { title: string; tone: Tone; text: string } => {
    switch (end?.reason) {
      case TerminalEndReason.EXITED: {
        const code = end.exitCode;
        return code === 0
          ? { title: t`The shell exited`, tone: "info", text: t`It exited with code 0.` }
          : { title: t`The shell exited`, tone: "warning", text: t`It exited with code ${code}.` };
      }
      case TerminalEndReason.CLOSED:
        return {
          title: t`This terminal was closed`,
          tone: "info",
          text: t`Its shell hung up, as an SSH session does.`,
        };
      case TerminalEndReason.DETACHED:
        return {
          title: t`This terminal closed`,
          tone: "warning",
          text: t`Nobody was connected to it for 10 minutes, so its shell hung up.`,
        };
      case TerminalEndReason.IDLE:
        return {
          title: t`This terminal closed`,
          tone: "warning",
          text: t`Nothing was typed into it for an hour, so its shell hung up, rather than leave a root shell open.`,
        };
      case TerminalEndReason.SIGNED_OUT:
        return { title: t`This terminal closed`, tone: "info", text: t`The session that opened it ended.` };
      case TerminalEndReason.AGENT_RESTARTED:
        return {
          title: t`This terminal closed`,
          tone: "warning",
          text: t`The server's agent restarted, and its shell with it.`,
        };
      case TerminalEndReason.CONTROL_PLANE_RESTARTED:
        return { title: t`This terminal closed`, tone: "warning", text: t`The control plane restarted.` };
      case TerminalEndReason.SERVER_REMOVED:
        return { title: t`This terminal closed`, tone: "warning", text: t`Its server was removed from Dockwell.` };
      case TerminalEndReason.FAILED:
        return { title: t`This terminal stopped`, tone: "critical", text: end.message };
      default:
        return {
          title: t`This terminal has ended`,
          tone: "info",
          text: t`It ended while this page wasn't attached to it.`,
        };
    }
  };
}
