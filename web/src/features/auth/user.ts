// SPDX-License-Identifier: AGPL-3.0-only

import { Role } from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";
import { useCallback } from "react";

/**
 * The letters for someone's avatar: the first of their first and last
 * names, "MC" for Mira Chen, or the first character of a name with no
 * spaces, such as 陈 for 陈米拉.
 */
export function initials(name: string): string {
  const words = name.trim().split(/\s+/).filter(Boolean);
  const first = words[0];
  if (!first) return "";
  const last = words.length > 1 ? words[words.length - 1] : undefined;
  const lead = (word: string) => Array.from(word)[0] ?? "";
  return (lead(first) + (last ? lead(last) : "")).toLocaleUpperCase();
}

/**
 * What a role on the whole install is called under someone's name, such as
 * "Owner"; nothing for someone who holds projects alone.
 */
export function useRoleName(role: Role): string {
  return useRoleNames()(role);
}

/** What each role is called, as useRoleName says one. */
export function useRoleNames(): (role: Role) => string {
  const { t } = useLingui();
  return useCallback(
    (role: Role) => {
      switch (role) {
        case Role.OWNER:
          return t`Owner`;
        case Role.ADMIN:
          return t`Admin`;
        case Role.DEVELOPER:
          return t`Developer`;
        case Role.VIEWER:
          return t`Viewer`;
        default:
          return "";
      }
    },
    [t],
  );
}

/**
 * Who a role makes someone, where, in a sentence: "an admin", "a developer of
 * shop", "a viewer of every project".
 */
export function useRolePhrase(): (role: Role, project: string) => string {
  const { t } = useLingui();
  return useCallback(
    (role: Role, project: string) => {
      switch (role) {
        case Role.OWNER:
          return t`the owner`;
        case Role.ADMIN:
          return t`an admin`;
        case Role.DEVELOPER:
          return project ? t`a developer of ${project}` : t`a developer of every project`;
        case Role.VIEWER:
          return project ? t`a viewer of ${project}` : t`a viewer of every project`;
        default:
          return t`someone with no role`;
      }
    },
    [t],
  );
}
