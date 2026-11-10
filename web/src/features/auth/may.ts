// SPDX-License-Identifier: AGPL-3.0-only

import type { DescMethod } from "@bufbuild/protobuf";
import { getOption } from "@bufbuild/protobuf";
import {
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
  /** An environment by ID, or undefined for one not read yet. */
  environment(id: string): Placed | undefined;
}

/** Why a call can't be made. */
export type Refusal =
  /** The role held is below what the method needs. */
  | "role"
  /** The role held does for an open environment, and this one is protected. */
  | "protected"
  /** Nothing is held where the object is. */
  | "reach";

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
  if (rule.reach !== Reach.PROJECT || !where) return { allowed: true };

  const onInstall = highest(holding.grants.filter((g) => g.projectId === "").map((g) => g.role));
  // Admins and the owner hold every place.
  if (RANK[onInstall] >= RANK[Role.ADMIN]) return { allowed: true };
  if ("install" in where) return { allowed: false, why: "role", needs: Role.ADMIN };

  // An environment not read yet is taken as protected, so nothing is offered that may then be refused.
  const placed = "environment" in where ? holding.environment(where.environment) : undefined;
  const project = "project" in where ? where.project : placed?.project;
  const isProtected = "environment" in where && (placed?.protected ?? true);
  const needs = isProtected ? rule.needsProtected : rule.needs;
  if (project === undefined) {
    return RANK[onInstall] >= RANK[needs] ? { allowed: true } : { allowed: false, why: "protected", needs };
  }
  const held = highest(holding.grants.filter((g) => g.projectId === "" || g.projectId === project).map((g) => g.role));
  if (held === Role.UNSPECIFIED) return { allowed: false, why: "reach", needs };
  if (RANK[held] >= RANK[needs]) return { allowed: true };
  return { allowed: false, why: isProtected && RANK[held] >= RANK[rule.needs] ? "protected" : "role", needs };
}

/** What a control gets from an answer: nothing when it may, and when it may not, off with the reason as its description. */
export interface Offer extends Decision {
  /** Why not, in a sentence, or "" when it may. */
  reason: string;
  /** Spread onto a button or a switch: `disabled` and `title` when it may not. */
  off: { disabled?: true; title?: string };
}

/** What the signed-in person holds, with the environments as far as they're read. */
export function useHolding(): Holding {
  const grants = useSignedIn()?.grants;
  const places = usePlaces();
  return useMemo(
    () => ({
      grants: grants ?? [],
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
export function useMay(): (method: DescMethod, where?: Where) => Offer {
  const { t } = useLingui();
  const holding = useHolding();
  return useCallback(
    (method, where) => {
      const decision = decide(method, holding, where);
      if (decision.allowed) return { ...decision, reason: "", off: {} };
      let reason: string;
      if (decision.why === "protected") {
        reason = t`This environment is protected, so an admin or the owner does this.`;
      } else if (decision.why === "reach") {
        reason = t`You hold no role in this project. Ask an admin to add you.`;
      } else if (decision.needs === Role.OWNER) {
        reason = t`Only the owner can do this.`;
      } else if (decision.needs === Role.ADMIN) {
        reason = t`Only an admin or the owner can do this.`;
      } else {
        reason = t`Only a developer or more can do this. Ask an admin to change your role.`;
      }
      return { ...decision, reason, off: { disabled: true, title: reason } };
    },
    [holding, t],
  );
}
