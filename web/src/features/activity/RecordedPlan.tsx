// SPDX-License-Identifier: AGPL-3.0-only

import type { Plan } from "@dockwell/api";
import { PlanDiff } from "@dockwell/ui";

import type { useFieldRows } from "../changes/fields";
import { diffResources, foldPlumbing } from "../changes/plan";
import { useAlsoWords, useChangeWords } from "../changes/words";

/**
 * What a change on record did, as the plan sheet reads it: what it was for, and
 * what matters first. Activity and the audit log both open a change to it.
 */
export function RecordedPlan({
  plan,
  applied,
  rows,
}: {
  plan: Plan | undefined;
  applied: boolean;
  rows: ReturnType<typeof useFieldRows>;
}) {
  const words = useChangeWords();
  const alsoWords = useAlsoWords();
  const fold = foldPlumbing(plan, diffResources(plan, rows, { tag: (intent) => words.intentTag(intent, applied) }));
  return (
    <PlanDiff
      resources={fold.shown}
      also={
        fold.folded.length > 0
          ? { label: alsoWords(fold, applied), action: "create", resources: fold.folded }
          : undefined
      }
    />
  );
}
