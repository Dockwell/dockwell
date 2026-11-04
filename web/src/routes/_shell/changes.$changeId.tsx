// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useEffect } from "react";

import { ActivityPage, searchWithout } from "../../features/activity/ActivityPage";
import { useChangeSheet } from "../../features/changes/ChangeSheet";

/**
 * A change's own page, /changes/<id>, which the CLI sends a person to for
 * a plan only a signed-in person may apply, such as a destructive one. It
 * opens the change's plan sheet over Activity, with the change chosen, to
 * review and apply it, typing its fingerprint first when it asks. Closing
 * the sheet leaves Activity as it is.
 */
export const Route = createFileRoute("/_shell/changes/$changeId")({
  component: function Change() {
    const { changeId } = Route.useParams();
    const navigate = useNavigate();
    const { open } = useChangeSheet();
    useEffect(() => {
      open({
        kind: "change",
        id: changeId,
        onClose: () => void navigate({ to: "/activity", search: { event: changeId }, replace: true }),
      });
    }, [open, changeId, navigate]);
    return (
      <ActivityPage
        search={{ event: changeId }}
        onSearch={(next) => void navigate({ to: "/activity", search: searchWithout(next) })}
      />
    );
  },
});
