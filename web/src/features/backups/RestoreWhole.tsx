// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { BackupService } from "@dockwell/api";
import { Button, Sheet, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { useChangeSheet } from "../changes/ChangeSheet";

/** What a whole restore of a shared server asks to be typed: the server's name, from what it says. */
function nameToType(said: string): string {
  return /Type (\S+) to confirm/.exec(said)?.[1] ?? "";
}

/**
 * Restores a backup of a whole volume as a plan. A database server other projects'
 * apps share asks first for its name typed, as a whole restore takes back what each
 * wrote since, which it says (ADR 0084).
 */
export function useRestoreWhole(): { restore: (id: string) => void; confirming: ReactNode } {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const [asking, setAsking] = useState<{ id: string; said: string; name: string }>();
  const [typed, setTyped] = useState("");
  const client = createClient(BackupService, transport);
  const restore = (id: string) => {
    void (async () => {
      try {
        const planned = await client.restoreBackup({ id });
        sheet.open({ kind: "plan", make: () => Promise.resolve(planned) });
      } catch (error) {
        if (reasonOf(error) === "confirm_name") {
          const said = sentence(error);
          setTyped("");
          setAsking({ id, said, name: nameToType(said) });
          return;
        }
        sheet.open({ kind: "plan", make: () => Promise.reject(error) });
      }
    })();
  };
  const close = () => setAsking(undefined);
  const server = asking?.name ?? "";
  const confirming = asking ? (
    <Sheet
      open
      onClose={close}
      title={t`Restore ${server} whole`}
      footer={
        <>
          <Button variant="secondary" onClick={close}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="danger"
            disabled={typed !== asking.name}
            onClick={() => {
              const { id, name } = asking;
              close();
              sheet.open({ kind: "plan", make: () => client.restoreBackup({ id, confirm: name }) });
            }}
          >
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <p>{asking.said}</p>
      <TextField
        label={t`Type ${server} to confirm`}
        value={typed}
        onChange={(e) => setTyped(e.currentTarget.value)}
        autoComplete="off"
        spellCheck={false}
      />
    </Sheet>
  ) : null;
  return { restore, confirming };
}
