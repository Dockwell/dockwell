// SPDX-License-Identifier: AGPL-3.0-only

import { type Actor, ActorKind, TokenScope } from "@dockwell/api";
import type { IconName } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useMemo } from "react";

/** How an actor shows wherever one does: Activity, the audit log, an app's deployments. */
export interface ActorWords {
  /** The name for a row, such as "Mira Chen", "GitHub Actions deploy" or "CLI on hz-1". */
  name: string;
  /** Its mark; a person has their initials instead. */
  icon: IconName | undefined;
  /** All of it, for a tooltip or an opened row, such as "Local access token, read-write, tok_…, made by mira@hz-1". */
  full: string;
  /** A machine's name, such as a server's or a shell account's, which reads best in mono. */
  machine: boolean;
}

/**
 * The control server a token the CLI made for local access was made on, as its
 * account says it, "mira@hz-1"; undefined for any other actor. Only what made the
 * token says so: its name says whatever its maker typed.
 */
export function localHost(actor: Actor | undefined): string | undefined {
  const account = actor?.kind === ActorKind.TOKEN ? actor.localAccess?.account : undefined;
  if (!account) return undefined;
  const at = account.lastIndexOf("@");
  return at >= 0 ? account.slice(at + 1) : account;
}

/** Says who an actor is, the same way on every screen. */
export function useActorWords(): (actor: Actor | undefined) => ActorWords {
  const { t } = useLingui();
  return useMemo(
    () => (actor: Actor | undefined) => {
      if (!actor) return { name: t`Someone`, icon: undefined, full: t`Someone`, machine: false };
      const host = localHost(actor);
      const local = actor.localAccess;
      if (host !== undefined && local) {
        const { id } = actor;
        const { account } = local;
        const full =
          local.scope === TokenScope.READ_WRITE
            ? t`Local access token, read-write, ${id}, made by ${account}`
            : t`Local access token, read-only, ${id}, made by ${account}`;
        return { name: t`CLI on ${host}`, icon: "terminal", full, machine: false };
      }
      const name = actor.name || actor.id || t`Someone`;
      switch (actor.kind) {
        case ActorKind.TOKEN:
          return { name, icon: "key", full: name, machine: false };
        case ActorKind.SERVER:
          return { name, icon: "server", full: name, machine: true };
        case ActorKind.SHELL:
          return { name, icon: "terminal", full: name, machine: true };
        case ActorKind.DOCKWELL:
          return { name, icon: "activity", full: name, machine: false };
        default:
          return { name, icon: undefined, full: name, machine: false };
      }
    },
    [t],
  );
}
