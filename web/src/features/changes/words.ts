// SPDX-License-Identifier: AGPL-3.0-only

import { ActorKind, ChangeState, PlanAction, StepEffect, StepState, Surface } from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useMemo } from "react";

/** How a step is going, as a status and a word. */
export interface StepLook {
  status: Status;
  word: string;
}

/** The words the plan sheet and Activity share, in the active locale. */
export interface ChangeWords {
  /** A resource's kind, as a noun: "Project". */
  kind(kind: string): string;
  /** What a step does: "Create", "Replace". */
  action(action: PlanAction): string;
  /** What a step does to what the resource serves: "Restarts". Empty for nothing. */
  effect(effect: StepEffect): string;
  step(state: StepState): StepLook;
  /** Where a change stands, as a status and a word, or undefined for an applied one, which stays quiet. */
  state(state: ChangeState): StepLook | undefined;
  /** Where something came from, as a value: "The panel". */
  surface(surface: Surface): string;
  /** Where something came from, as a line of its own: "From the panel". */
  via(surface: Surface): string;
  /** What kind of actor someone is: "API token". Empty for a person. */
  actorKind(kind: ActorKind): string;
}

export function useChangeWords(): ChangeWords {
  const { t } = useLingui();
  return useMemo<ChangeWords>(
    () => ({
      kind(kind) {
        switch (kind) {
          case "project":
            return t`Project`;
          case "environment":
            return t`Environment`;
          case "service":
            return t`Service`;
          case "container":
            return t`Container`;
          case "firewall.rule":
            return t`Firewall rule`;
        }
        return kind;
      },
      action(action) {
        switch (action) {
          case PlanAction.CREATE:
            return t`Create`;
          case PlanAction.UPDATE:
            return t`Update in place`;
          case PlanAction.REPLACE:
            return t`Replace`;
          case PlanAction.DELETE:
            return t`Destroy`;
        }
        return t`Nothing to do`;
      },
      effect(effect) {
        switch (effect) {
          case StepEffect.RESTART:
            return t`Restarts`;
          case StepEffect.RECREATE:
            return t`Recreated`;
        }
        return "";
      },
      step(state) {
        switch (state) {
          case StepState.RUNNING:
            return { status: "deploying", word: t`Running` };
          case StepState.DONE:
            return { status: "healthy", word: t`Done` };
          case StepState.FAILED:
            return { status: "down", word: t`Failed` };
          case StepState.SKIPPED:
            return { status: "stopped", word: t`Skipped` };
          case StepState.WAITING:
            return { status: "degraded", word: t`Waiting` };
        }
        return { status: "stopped", word: t`Queued` };
      },
      state(state) {
        switch (state) {
          case ChangeState.PLANNED:
            return { status: "stopped", word: t`Planned` };
          case ChangeState.APPLYING:
            return { status: "deploying", word: t`Applying` };
          case ChangeState.FAILED:
            return { status: "down", word: t`Failed` };
          case ChangeState.DISCARDED:
            return { status: "stopped", word: t`Discarded` };
        }
        return undefined;
      },
      surface(surface) {
        switch (surface) {
          case Surface.PANEL:
            return t`The panel`;
          case Surface.API:
            return t`The API`;
          case Surface.CLI:
            return t`The CLI`;
          case Surface.AUTOMATIC:
            return t`Automatic`;
        }
        return t`Unknown`;
      },
      via(surface) {
        switch (surface) {
          case Surface.PANEL:
            return t`From the panel`;
          case Surface.API:
            return t`From the API`;
          case Surface.CLI:
            return t`From the CLI`;
          case Surface.AUTOMATIC:
            return t`Automatic`;
        }
        return "";
      },
      actorKind(kind) {
        switch (kind) {
          case ActorKind.TOKEN:
            return t`API token`;
          case ActorKind.DOCKWELL:
            return t`Automatic`;
          case ActorKind.SERVER:
            return t`Server`;
          case ActorKind.SHELL:
            return t`In a shell on the control server`;
        }
        return "";
      },
    }),
    [t],
  );
}
