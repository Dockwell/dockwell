// SPDX-License-Identifier: AGPL-3.0-only

import { ActorKind, type Change, ChangeState, DeploymentState, FieldOp, type ServiceRevision } from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { createElement, type ReactNode, useCallback, useMemo } from "react";

import { type AppState, fieldName, shortImage } from "./app";
import { useAppWords } from "./words";

/** A revision's place in an app's history, as a status and a word. */
export interface Look {
  status: Status;
  word: string;
}

/**
 * Where a revision stands: failed, deploying, live, rolled back or replaced.
 * The one that runs says what the app's header says when that is more than
 * "it deployed", as when its server is offline. What failed stays failed,
 * whatever is still marked as on its way.
 */
export function useRevisionLook() {
  const { t } = useLingui();
  const words = useAppWords();
  return useCallback(
    (rev: ServiceRevision, latest: bigint, state?: AppState): Look => {
      const change = rev.change;
      if (rev.deleted) return { status: "stopped", word: t`Deleted` };
      if (change?.state === ChangeState.FAILED || rev.deployments.some((d) => d.state === DeploymentState.FAILED)) {
        return { status: "down", word: t`Failed` };
      }
      if (change?.state === ChangeState.APPLYING || rev.deployments.some((d) => d.state === DeploymentState.PENDING)) {
        return { status: "deploying", word: t`Deploying` };
      }
      if (rev.number === latest) {
        if (state && (state.kind === "offline" || state.kind === "unknown" || state.kind === "unplaced")) {
          return { status: state.status, word: words.state(state) };
        }
        return { status: "healthy", word: t({ message: "Live", context: "An app whose latest revision is deployed" }) };
      }
      if (change?.undoneBy) return { status: "stopped", word: t`Rolled back` };
      return { status: "stopped", word: t`Replaced` };
    },
    [t, words],
  );
}

/**
 * What a revision did, for its history: in the past tense, or the present for
 * work in flight, as the content rules have it, and without the app's name, as
 * its page says that. The words someone gave the plan are its "Plan" fact.
 */
export function useRevisionTitle() {
  const { t } = useLingui();
  return useCallback(
    (rev: ServiceRevision): ReactNode => {
      const change = rev.change;
      if (rev.deleted) return t`Deleted`;
      if (change?.rollback) return t`Rolled back after a failed deploy`;
      if (rev.rolledBackTo > 0n) {
        const target = `r${rev.rolledBackTo}`;
        return t`Rolled back to ${target}`;
      }
      const failed =
        change?.state === ChangeState.FAILED || rev.deployments.some((d) => d.state === DeploymentState.FAILED);
      const moving =
        !failed &&
        (change?.state === ChangeState.APPLYING || rev.deployments.some((d) => d.state === DeploymentState.PENDING));
      const image = createElement("code", { className: "app-title-code", title: rev.image }, shortImage(rev.image));
      const imageField = rev.changes.find((c) => fieldName(c.path) === "image");
      const created = rev.number === 1n || imageField?.op === FieldOp.ADDED;
      const redeployed = !created && imageField !== undefined;
      if (created || redeployed) {
        const parts = created
          ? { past: t`Created from`, now: t`Creating from`, tried: t`Couldn't create from` }
          : { past: t`Deployed`, now: t`Deploying`, tried: t`Couldn't deploy` };
        const words = failed ? parts.tried : moving ? parts.now : parts.past;
        return createElement("span", null, words, " ", image);
      }
      const [first] = rev.changes;
      if (!first) return moving ? t`Moving` : t`Moved`;
      const field = fieldName(first.path).replace(/\[.*$/, "");
      const more = rev.changes.length - 1;
      if (failed) return more > 0 ? t`Couldn't change ${field} and ${more} more` : t`Couldn't change ${field}`;
      if (moving) return more > 0 ? t`Changing ${field} and ${more} more` : t`Changing ${field}`;
      return more > 0 ? t`Changed ${field} and ${more} more` : t`Changed ${field}`;
    },
    [t],
  );
}

/** Local access's token says where it is in its name: "Local access on hz-1, read-write". */
const LOCAL_ACCESS = /^Local access on (.+?)(?:, read-(?:write|only))?$/;

/**
 * Who did a change, in words that read after a time, as "by Mira Chen". A
 * token isn't a person, so what comes from local access reads as where it was
 * run, "from the CLI on hz-1".
 */
export function useWhoWords() {
  const { t } = useLingui();
  return useMemo(
    () => ({
      /** The name alone, for a row with room for a name: "Mira Chen", "the CLI on hz-1". */
      name(change: Change | undefined): string {
        const actor = change?.appliedBy?.name ? change.appliedBy : change?.proposedBy;
        const name = actor?.name ?? "";
        const local = actor?.kind === ActorKind.TOKEN ? LOCAL_ACCESS.exec(name)?.[1] : undefined;
        return local ? t`the CLI on ${local}` : name;
      },
      /** The whole phrase, after a time: "by Mira Chen", "from the CLI on hz-1". Empty when no one is named. */
      phrase(change: Change | undefined): string {
        const actor = change?.appliedBy?.name ? change.appliedBy : change?.proposedBy;
        const name = actor?.name ?? "";
        if (name === "") return "";
        const local = actor?.kind === ActorKind.TOKEN ? LOCAL_ACCESS.exec(name)?.[1] : undefined;
        return local ? t`from the CLI on ${local}` : t`by ${name}`;
      },
    }),
    [t],
  );
}
