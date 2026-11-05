// SPDX-License-Identifier: AGPL-3.0-only

import { ActorKind, type Change, ChangeState, DeploymentState, FieldOp, type ServiceRevision } from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { createElement, type ReactNode, useCallback, useMemo } from "react";

import { type AppState, bareImage, fieldName, fieldOfChange, shortImage } from "./app";
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

/** What a revision did: to show, and as plain words for what clips it. */
export interface RevisionTitle {
  node: ReactNode;
  text: string;
}

/**
 * What a revision did, for its history: in the past tense, or the present for
 * work in flight, as the content rules have it, and without the app's name, as
 * its page says that. The words someone gave the plan are its "Plan" fact.
 */
export function useRevisionTitle() {
  const { t } = useLingui();
  const words = useAppWords();
  return useCallback(
    (rev: ServiceRevision): RevisionTitle => {
      const plain = (text: string): RevisionTitle => ({ node: text, text });
      const change = rev.change;
      if (rev.deleted) return plain(t`Deleted`);
      if (change?.rollback) return plain(t`Rolled back after a failed deploy`);
      if (rev.rolledBackTo > 0n) {
        const target = `r${rev.rolledBackTo}`;
        return plain(t`Rolled back to ${target}`);
      }
      const failed =
        change?.state === ChangeState.FAILED || rev.deployments.some((d) => d.state === DeploymentState.FAILED);
      const moving =
        !failed &&
        (change?.state === ChangeState.APPLYING || rev.deployments.some((d) => d.state === DeploymentState.PENDING));
      const pulled = shortImage(bareImage(rev.image));
      const imageField = rev.changes.find((c) => fieldName(c.path) === "image");
      const created = rev.number === 1n || imageField?.op === FieldOp.ADDED;
      if (created || imageField) {
        const image = shortImage(bareImage(rev.image));
        const parts = created
          ? { past: t`Created from`, now: t`Creating from`, tried: t`Couldn't create from` }
          : {
              past: t({ message: "Deployed", context: "What a revision did, before the image it deployed" }),
              now: t`Deploying`,
              tried: t`Couldn't deploy`,
            };
        const words = failed ? parts.tried : moving ? parts.now : parts.past;
        return {
          node: createElement("span", null, words, " ", createElement("code", { className: "app-title-code" }, image)),
          text: `${words} ${image}`,
        };
      }
      // A port added says which: three fields of one port aren't three changes.
      const fields = [...new Set(rev.changes.map((c) => fieldOfChange(c.path)))];
      const published = rev.changes.filter((c) => /\.host_port$/.test(c.path) && c.op === FieldOp.ADDED);
      const [port] = published;
      if (port && fields.length === 1 && published.length === 1) {
        const number = port.to;
        if (failed) return plain(t`Couldn't publish port ${number}`);
        return plain(moving ? t`Publishing port ${number}` : t`Published port ${number}`);
      }
      const [first] = fields;
      if (first === undefined) {
        // Nothing but the image its tag names moved, which a pull found.
        if (failed) return plain(t`Couldn't pull ${pulled}`);
        return plain(moving ? t`Pulling ${pulled} again` : t`Pulled ${pulled} again`);
      }
      const field = words.field(first);
      const more = fields.length - 1;
      if (failed) return plain(more > 0 ? t`Couldn't change ${field} and ${more} more` : t`Couldn't change ${field}`);
      if (moving) return plain(more > 0 ? t`Changing ${field} and ${more} more` : t`Changing ${field}`);
      return plain(more > 0 ? t`Changed ${field} and ${more} more` : t`Changed ${field}`);
    },
    [t, words],
  );
}

/** Local access's token says where it is in its name: "Local access on hz-1, read-write". */
const LOCAL_ACCESS = /^Local access on (.+?)(?:, read-(?:write|only))?$/;

/**
 * Who did a change, in words that read: a person by name, and what comes from
 * local access, as a token isn't a person, by where it was run, "from the CLI
 * on hz-1".
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
      /** That it deployed, how long ago, and who did it: "deployed 3 min ago by Mira Chen", said whole for other languages. */
      deployed(change: Change | undefined, ago: string): string {
        const actor = change?.appliedBy?.name ? change.appliedBy : change?.proposedBy;
        const name = actor?.name ?? "";
        const local = actor?.kind === ActorKind.TOKEN ? LOCAL_ACCESS.exec(name)?.[1] : undefined;
        if (local) return t`deployed ${ago} from the CLI on ${local}`;
        return name ? t`deployed ${ago} by ${name}` : t`deployed ${ago}`;
      },
      /** The same on a phone's panel, which says which revision. */
      revisionDeployed(change: Change | undefined, revision: string, when: string): string {
        const actor = change?.appliedBy?.name ? change.appliedBy : change?.proposedBy;
        const name = actor?.name ?? "";
        const local = actor?.kind === ActorKind.TOKEN ? LOCAL_ACCESS.exec(name)?.[1] : undefined;
        if (local) return t`${revision} deployed ${when} from the CLI on ${local}`;
        return name ? t`${revision} deployed ${when} by ${name}` : t`${revision} deployed ${when}`;
      },
    }),
    [t],
  );
}
