// SPDX-License-Identifier: AGPL-3.0-only

import type { Intent } from "@dockwell/api";
import { Glyph, Icon } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";

import { formatBytesOf } from "../../lib/format";
import { MachineWords } from "./FailureNotice";
import { shortDigest, type UpgradePhase, type UpgradePhaseKey, upgradeFrom, upgradeTo } from "./upgrade";
import "./Upgrade.css";

/**
 * What an update goes from and to, in one line: the image and its digest it
 * leaves, and the image it goes to, or a catalog install's releases (ADR 0080).
 */
export function UpgradeLine({ intent }: { intent: Intent }) {
  const { t } = useLingui();
  const from = upgradeFrom(intent);
  const to = upgradeTo(intent);
  const catalog = intent.release !== "";
  return (
    <p className="upgrade-line">
      <Icon name="deploy" size={16} />
      {catalog ? (
        <span>
          <Trans>
            From <strong>{from}</strong> to <strong>{to}</strong>
          </Trans>
        </span>
      ) : (
        <span className="upgrade-images">
          <code title={intent.fromImage}>{intent.fromImage}</code>
          {intent.fromDigest ? (
            <code className="upgrade-digest" title={intent.fromDigest}>
              {shortDigest(intent.fromDigest)}
            </code>
          ) : null}
          <span aria-hidden="true">→</span>
          <span className="dw-sr">{t({ message: "to", context: "update" })}</span>
          <code title={intent.image}>{intent.image}</code>
          {from === to ? (
            <span className="upgrade-newest">
              <Trans>its newest image</Trans>
            </span>
          ) : null}
        </span>
      )}
    </p>
  );
}

/**
 * An update's phases in order, each with how far it is: what's done, what's under
 * way with what it says and a pull's bytes, and what's to come (ADR 0080).
 */
export function UpgradePhases({ phases }: { phases: readonly UpgradePhase[] }) {
  const { t, i18n } = useLingui();
  const label = (key: UpgradePhaseKey) =>
    ({ backup: t`Back up`, move: t`Move the data`, pull: t`Pull the image`, start: t`Start`, check: t`Health check` })[
      key
    ];
  const stateWord = (phase: UpgradePhase) =>
    ({ queued: t`To come`, current: t`Under way`, done: t`Done`, failed: t`Failed` })[phase.state];
  return (
    <ol className="upgrade-phases" aria-label={t`How the update is going`}>
      {phases.map((phase) => {
        const bytes =
          phase.done !== undefined && phase.total !== undefined
            ? formatBytesOf(phase.done, phase.total, i18n.locale)
            : undefined;
        return (
          <li key={phase.key} className={`upgrade-phase is-${phase.state}`}>
            <span className="upgrade-phase-mark" aria-hidden="true">
              {phase.state === "done" ? (
                <Icon name="check" size={14} />
              ) : phase.state === "current" ? (
                <Glyph shape="spinner" size={14} />
              ) : phase.state === "failed" ? (
                <Glyph shape="square" size={12} />
              ) : (
                <Glyph shape="ring" size={12} />
              )}
            </span>
            <span className="upgrade-phase-label">{label(phase.key)}</span>
            <span className="dw-sr">{stateWord(phase)}</span>
            {phase.state === "current" && bytes ? (
              <span className="upgrade-phase-bytes">
                <Trans context="bytes pulled">
                  {bytes[0]} of {bytes[1]}
                </Trans>
              </span>
            ) : null}
            {phase.state === "current" && phase.done !== undefined && phase.total !== undefined ? (
              <progress
                className="upgrade-progress"
                max={Number(phase.total) || 1}
                value={Number(phase.done)}
                aria-label={t`Pulled of the image`}
              />
            ) : null}
            {phase.state === "current" && phase.activity && !bytes ? (
              <span className="upgrade-phase-activity">
                <MachineWords>{phase.activity}</MachineWords>
              </span>
            ) : null}
          </li>
        );
      })}
    </ol>
  );
}
