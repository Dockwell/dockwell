// SPDX-License-Identifier: AGPL-3.0-only

import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { useCallback } from "react";

import { countKinds } from "./contents";
import type { ProjectView } from "./useProjects";

/**
 * What a project holds, as parts of one line, such as ["2 apps", "1 domain", "3 variables"]:
 * what runs first, then what it uses (ADR 0074). A kind with none says nothing.
 */
export function useContentsWords(): (view: ProjectView) => string[] {
  const { t } = useLingui();
  return useCallback(
    (view: ProjectView) => {
      const kinds = countKinds(view.inside);
      const n = (kind: string) => kinds.get(kind) ?? 0;
      const parts: string[] = [];
      if (view.website) parts.push(t`a website`);
      if (view.apps.length > 0) parts.push(plural(view.apps.length, { one: "# app", other: "# apps" }));
      if (view.databases.length > 0)
        parts.push(plural(view.databases.length, { one: "# database", other: "# databases" }));
      if (view.domains.length > 0) parts.push(plural(view.domains.length, { one: "# domain", other: "# domains" }));
      if (n("variable") > 0) parts.push(plural(n("variable"), { one: "# variable", other: "# variables" }));
      if (n("secret") > 0) parts.push(plural(n("secret"), { one: "# secret", other: "# secrets" }));
      if (n("volume") > 0) parts.push(plural(n("volume"), { one: "# volume", other: "# volumes" }));
      if (n("task") > 0) parts.push(plural(n("task"), { one: "# scheduled task", other: "# scheduled tasks" }));
      return parts;
    },
    [t],
  );
}
