// SPDX-License-Identifier: AGPL-3.0-only

import { Role } from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";

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
  const { t } = useLingui();
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
}
