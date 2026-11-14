// SPDX-License-Identifier: AGPL-3.0-only

import { useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeService, ChangeState } from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";

import { type Offer, useMay } from "../auth/may";
import { type Opening, useChangeSheet } from "../changes/ChangeSheet";
import { planDeletingProject } from "./contents";
import type { ProjectView } from "./useProjects";

/**
 * Deleting a project, with everything in it, as one plan (ADR 0074): whether the signed-in
 * person may, and what opens the plan. A project is an admin's to delete (ADR 0049); the
 * default project, where apps with no project go, stays, and so does the only one.
 */
export function useProjectDelete(count: number): {
  offer: (view: ProjectView) => Offer;
  open: (view: ProjectView, after?: () => void) => void;
} {
  const { t } = useLingui();
  const may = useMay();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const navigate = useNavigate();
  // A project itself is in no project, so deleting one is an admin's (ADR 0049).
  const admin = may(ChangeService.method.proposeChange, { install: true });
  const offer = (view: ProjectView): Offer => {
    if (!admin.allowed) return admin;
    if (view.isDefault || count < 2) {
      const reason = t`Apps with no project go here, so it stays.`;
      return { allowed: false, reason, off: { off: reason } };
    }
    return admin;
  };
  const onClose = (after?: () => void) => (change: Change | undefined) => {
    if (change && change.state !== ChangeState.PLANNED && change.state !== ChangeState.FAILED) {
      if (after) after();
      else void navigate({ to: "/projects" });
    }
  };
  // What apps of other projects use moves out to keep them working, unless the switch
  // says to delete it too, which names the project to type (ADR 0084).
  const opening = (view: ProjectView, after: (() => void) | undefined, anyway: boolean): Opening => ({
    kind: "plan",
    make: () => planDeletingProject(transport, view.project, t`Delete the project ${view.label}`, anyway),
    onClose: onClose(after),
    ...(anyway ? { confirmName: view.project.resource?.name ?? view.label } : {}),
    choice: {
      label: t`Also delete what other apps use`,
      on: anyway,
      description: anyway
        ? t`Those apps stop working; their databases are dumped first`
        : t`It moves to a project of its own`,
      // Only where the project shares something with other projects' apps.
      moot: (plan) => !plan?.notes.some((n) => n.reason === "keeps_shared" || n.reason === "breaks_shared"),
      flip: () => opening(view, after, !anyway),
    },
  });
  const open = (view: ProjectView, after?: () => void) => sheet.open(opening(view, after, false));
  return { offer, open };
}
