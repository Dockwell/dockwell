// SPDX-License-Identifier: AGPL-3.0-only

import { clone } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ChangeState, type WebsiteInfo, WebsitePauseMode, WebsiteSchema, WebsiteService } from "@dockwell/api";
import { Button, SegmentedControl, Sheet } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { useChangeSheet } from "../changes/ChangeSheet";

/**
 * Pause one or more websites as a plan, choosing between a friendly maintenance
 * page and a plain 503 response. Remembers the site's previous choice when pausing
 * a single site.
 */
export function PauseSheet({
  sites,
  onClose,
  onApplied,
}: {
  sites: readonly WebsiteInfo[];
  onClose: () => void;
  onApplied?: () => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const single = sites.length === 1 ? sites[0] : undefined;
  const initialMode = single?.spec?.pauseMode || WebsitePauseMode.MAINTENANCE;
  const [mode, setMode] = useState<WebsitePauseMode>(initialMode);

  const submit = () => {
    const client = createClient(WebsiteService, transport);
    onClose();
    if (single?.spec) {
      const next = clone(WebsiteSchema, single.spec);
      next.paused = true;
      next.pauseMode = mode;
      sheet.open({
        kind: "plan",
        make: () =>
          client.updateWebsite({
            id: single.id,
            website: next,
            summary: t`Pause ${single.domain || single.name}`,
          }),
        onClose: (after) => {
          if (after?.state === ChangeState.APPLIED) onApplied?.();
        },
      });
      return;
    }
    sheet.open({
      kind: "plan",
      make: () =>
        client.batchUpdateWebsites({
          websiteIds: sites.map((s) => s.id),
          paused: true,
          pauseMode: mode,
          summary: plural(sites.length, { one: "Pause # website", other: "Pause # websites" }),
        }),
      onClose: (after) => {
        if (after?.state === ChangeState.APPLIED) onApplied?.();
      },
    });
  };

  const title = single
    ? t`Pause ${single.domain || single.name}`
    : plural(sites.length, { one: "Pause # website", other: "Pause # websites" });

  return (
    <Sheet
      open
      onClose={onClose}
      title={title}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" icon="pause" onClick={submit}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="website-fields">
        <p className="websites-muted">
          <Trans>Domains, certificates and files stay intact. Choose what visitors see while paused.</Trans>
        </p>
        <SegmentedControl<string>
          label={t`Visitor response`}
          value={mode === WebsitePauseMode.PLAIN_503 ? "503" : "maintenance"}
          onChange={(v) => setMode(v === "503" ? WebsitePauseMode.PLAIN_503 : WebsitePauseMode.MAINTENANCE)}
          options={[
            { value: "maintenance", label: t`Maintenance page` },
            { value: "503", label: t`Plain 503` },
          ]}
        />
        <p className="websites-muted">
          {mode === WebsitePauseMode.PLAIN_503 ? (
            <Trans>Answers every request with a plain 503 Service Unavailable status.</Trans>
          ) : (
            <Trans>Shows visitors a short maintenance page with a 503 status. Remembered for next time.</Trans>
          )}
        </p>
      </div>
    </Sheet>
  );
}
