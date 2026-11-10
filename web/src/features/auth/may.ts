// SPDX-License-Identifier: AGPL-3.0-only

import type { DescMethod } from "@bufbuild/protobuf";
import { getOption } from "@bufbuild/protobuf";
import {
  type Change,
  effect as effectOption,
  min_role,
  min_role_protected,
  Needs,
  Reach,
  Role,
  type RoleGrant,
  reach as reachOption,
} from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";
import { useCallback, useMemo } from "react";

import { usePlaces } from "../apps/useServices";
import { useSignedIn } from "./useAuthState";

/**
 * What the panel knows of what you may do (ADR 0059): who may call a method
 * is read from the method's own options, the ones the server's interceptor
 * reads, against the grants the sign-in state carries and where the call's
 * object is. Nothing here is a rule of the panel's own.
 */

/** Where a call's object is. Leave it out for a call that names nothing yet, such as a list. */
export type Where =
  /** In an environment, by ID: its project and whether it's protected come from the environments read. */
  | { environment: string }
  /** In a project, by ID, and no environment. */
  | { project: string }
  /** In no project: a server's own, such as a container Dockwell didn't make. */
  | { install: true };

/** An environment as `may` needs it. */
export interface Placed {
  project: string;
  protected: boolean;
}

/** What the caller holds, and what's known of where things are. */
export interface Holding {
  grants: readonly Pick<RoleGrant, "projectId" | "role">[];
  /** An environment by ID, or undefined for one not read. */
  environment(id: string): Placed | undefined;
  /** The environments are still being read. */
  pending: boolean;
}

/** Why a call can't be made. */
export type Refusal =
  /** The role held is below what the method needs. */
  | "role"
  /** The role held does for an open environment, and this one is protected. */
  | "protected"
  /** Nothing is held where the object is. */
  | "reach"
  /** Where the object is isn't read yet, so nothing is said either way. */
  | "pending";

/** Whether a call may be made, and when it can't, why and what it would take. */
export interface Decision {
  allowed: boolean;
  why?: Refusal;
  /** The lowest role that could make it there. */
  needs?: Role;
}

/** How roles rank, as the store ranks them. */
const RANK: Record<Role, number> = {
  [Role.UNSPECIFIED]: 0,
  [Role.VIEWER]: 1,
  [Role.DEVELOPER]: 2,
  [Role.ADMIN]: 3,
  [Role.OWNER]: 4,
};

/** The role a method's option names. Unset is the owner's, as on the server: a method that doesn't say is closed. */
function roleOf(needs: Needs): Role {
  switch (needs) {
    case Needs.NOTHING:
      return Role.UNSPECIFIED;
    case Needs.VIEWER:
      return Role.VIEWER;
    case Needs.DEVELOPER:
      return Role.DEVELOPER;
    case Needs.ADMIN:
      return Role.ADMIN;
    default:
      return Role.OWNER;
  }
}

/** A method's rule, as its options say it. */
export function ruleOf(method: DescMethod) {
  return {
    needs: roleOf(getOption(method, min_role)),
    needsProtected: roleOf(getOption(method, min_role_protected)),
    reach: getOption(method, reachOption),
    effect: getOption(method, effectOption),
  };
}

const highest = (roles: Role[]): Role => roles.reduce((best, r) => (RANK[r] > RANK[best] ? r : best), Role.UNSPECIFIED);

/**
 * Whether whoever holds `holding` may call `method` where `where` says, as the
 * interceptor decides it: the lowest role the method needs anywhere against the
 * highest held anywhere, then, for a project's object, the role held at its
 * place against what the method needs there. What a plan's contents ask of
 * whoever applies it is the plan's own to say (Change.waits).
 */
export function decide(method: DescMethod, holding: Holding, where?: Where): Decision {
  const rule = ruleOf(method);
  if (rule.needs === Role.UNSPECIFIED) return { allowed: true };
  const anywhere = highest(holding.grants.map((g) => g.role));
  if (RANK[anywhere] < RANK[rule.needs]) return { allowed: false, why: "role", needs: rule.needs };
  // Only a project's object has a place to check. What a method reaches by name on the install, a server
  // or a destination, is checked by the server against the request, so a place given for one changes nothing.
  if (rule.reach !== Reach.PROJECT || !where) return { allowed: true };

  const onInstall = highest(holding.grants.filter((g) => g.projectId === "").map((g) => g.role));
  // Admins and the owner hold every place.
  if (RANK[onInstall] >= RANK[Role.ADMIN]) return { allowed: true };
  if ("install" in where) return { allowed: false, why: "role", needs: Role.ADMIN };

  let project: string;
  let isProtected = false;
  if ("project" in where) {
    project = where.project;
  } else {
    const placed = holding.environment(where.environment);
    if (!placed) {
      // Not read yet: nothing is said of it while the environments load. If they can't be read, the
      // first check stands alone and the server says the rest, as nothing here knows it to be protected.
      return holding.pending ? { allowed: false, why: "pending" } : { allowed: true };
    }
    project = placed.project;
    isProtected = placed.protected;
  }
  const needs = isProtected ? rule.needsProtected : rule.needs;
  const held = highest(holding.grants.filter((g) => g.projectId === "" || g.projectId === project).map((g) => g.role));
  if (held === Role.UNSPECIFIED) return { allowed: false, why: "reach", needs };
  if (RANK[held] >= RANK[needs]) return { allowed: true };
  return { allowed: false, why: isProtected && RANK[held] >= RANK[rule.needs] ? "protected" : "role", needs };
}

/** What a control gets from an answer. */
export interface Offer extends Decision {
  /** Why not, in a sentence, or "" when it may, and while what decides it is still being read. */
  reason: string;
  /**
   * Spread onto a Button: nothing when it may; when it may not, `off` with the reason, which keeps it in
   * place and in the tab order with the reason as its description; while it isn't known yet, `disabled`.
   * For anything that isn't a Button, pass `reason` to `useOff` from @dockwell/ui.
   */
  off: { off?: string; disabled?: true };
}

/**
 * Where a change's items are, each place once: an environment's, a project's, or no project's, as a
 * server's own. Undoing it is asked for in each.
 */
export function placesOfChange(change: Pick<Change, "plan"> | undefined): Where[] {
  const seen = new Set<string>();
  const out: Where[] = [];
  for (const planned of change?.plan?.resources ?? []) {
    const ref = planned.resource;
    if (!ref) continue;
    // A project itself is in no project; anything else is where its scope is.
    const scope = ref.kind === "project" ? "" : ref.scope;
    const where: Where = scope.startsWith("env_")
      ? { environment: scope }
      : scope.startsWith("prj_")
        ? { project: scope }
        : { install: true };
    const key = JSON.stringify(where);
    if (seen.has(key)) continue;
    seen.add(key);
    out.push(where);
  }
  return out;
}

/** What the signed-in person holds, with the environments as far as they're read. */
export function useHolding(): Holding {
  const grants = useSignedIn()?.grants;
  const places = usePlaces();
  return useMemo(
    () => ({
      grants: grants ?? [],
      pending: places.isPending,
      environment: (id) => {
        const place = places.place(id);
        return place ? { project: place.scope, protected: place.protected } : undefined;
      },
    }),
    [grants, places],
  );
}

/**
 * Asks whether the signed-in person may call a method, here. An action on
 * something they can see stays where it is, off, with the reason; a way to a
 * place they can't read is left out (ADR 0059).
 */
export function useMay(): (method: DescMethod, where?: Where | readonly Where[]) => Offer {
  const { t } = useLingui();
  const holding = useHolding();
  return useCallback(
    (method, where) => {
      // Several places, as a change's items have: it may where it may in each, and the first no is the answer.
      const wheres: readonly (Where | undefined)[] = Array.isArray(where)
        ? where.length > 0
          ? where
          : [undefined]
        : [where as Where | undefined];
      const decisions = wheres.map((w) => decide(method, holding, w));
      const decision = decisions.find((d) => !d.allowed) ?? { allowed: true };
      if (decision.allowed) return { ...decision, reason: "", off: {} };
      // Not known yet: off for the moment, with nothing said.
      if (decision.why === "pending") return { ...decision, reason: "", off: { disabled: true } };
      let reason: string;
      if (decision.why === "protected") {
        reason = t`This environment is protected, so an admin or the owner does this. Ask one of them.`;
      } else if (decision.why === "reach") {
        reason = t`You hold no role in this project. Ask an admin to add you.`;
      } else if (decision.needs === Role.OWNER) {
        reason = t`Only the owner can do this.`;
      } else if (decision.needs === Role.ADMIN) {
        reason = t`Only an admin or the owner can do this. Ask one of them.`;
      } else {
        reason = t`Only a developer or more can do this. Ask an admin to change your role.`;
      }
      return { ...decision, reason, off: { off: reason } };
    },
    [holding, t],
  );
}
