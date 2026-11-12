// SPDX-License-Identifier: AGPL-3.0-only

import { useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeService, ChangeState } from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";

import { type Offer, useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
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
  const open = (view: ProjectView, after?: () => void) =>
    sheet.open({
      kind: "plan",
      make: () =>
        planDeletingProject(
          transport,
          view.project,
          view.environments.map((e) => e.id),
          t`Delete the project ${view.label}`,
        ),
      onClose: (change: Change | undefined) => {
        if (change && change.state !== ChangeState.PLANNED && change.state !== ChangeState.FAILED) {
          if (after) after();
          else void navigate({ to: "/projects" });
        }
      },
    });
  return { offer, open };
}
