// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeService, ChangeState, type Server } from "@dockwell/api";
import { Button, Callout, cx, Icon, PHONE_QUERY, Sheet, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { LabelsField } from "./LabelsField";
import { labelChanges } from "./labels";
import { useFleet } from "./useFleet";
import "./LabelChips.css";

/**
 * A server's labels, to change: chips that offer what the fleet uses, then the plan,
 * which takes no step on the server, as it says (ADR 0091).
 */
export function EditLabelsSheet({ server, onClose }: { server: Server; onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const fleet = useFleet();
  const servers = fleet.status === "connecting" ? [] : fleet.servers;
  const [labels, setLabels] = useState<Record<string, string>>({ ...server.labels });
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string>();
  const [plan, setPlan] = useState<Opening>();
  const { set, remove } = labelChanges(server.labels, labels);
  const same = set.length === 0 && remove.length === 0;
  const name = server.name;

  const review = async () => {
    if (busy || same) return;
    setBusy(true);
    setFailure(undefined);
    let change: Change | undefined;
    try {
      change = (await client.planServerLabels({ serverIds: [server.id], set, remove })).change;
    } catch (err) {
      setFailure(sentence(err));
      setBusy(false);
      return;
    }
    setBusy(false);
    setPlan({
      kind: "plan",
      make: () => Promise.resolve({ change }),
      retryLabel: t`Edit again`,
      onDone: onClose,
      onClose: (after) => {
        if (!after || after.state === ChangeState.PLANNED) setPlan(undefined);
        else onClose();
      },
    });
  };

  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      title={t`Labels of ${name}`}
      subtitle={t`What the fleet groups and selects servers by`}
      onClose={onClose}
      className={cx("labels-sheet", phone && "is-phone")}
      footer={
        <>
          <span className="dw-sheet-note">
            <Icon name="commit" size={14} />
            <Trans>Nothing runs on {name}</Trans>
          </span>
          <Button variant="ghost" size={phone ? "lg" : "md"} onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            size={phone ? "lg" : "md"}
            loading={busy}
            disabled={same}
            onClick={() => void review()}
          >
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <form
        className="labels-sheet-form"
        onSubmit={(e) => {
          e.preventDefault();
          void review();
        }}
      >
        <LabelsField labels={labels} onChange={setLabels} fleet={servers} label={t`Labels`} />
        {failure ? (
          <Callout tone="critical" title={t`No plan yet`}>
            {failure}
          </Callout>
        ) : null}
      </form>
    </Sheet>
  );
}
