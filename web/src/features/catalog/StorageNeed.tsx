// SPDX-License-Identifier: AGPL-3.0-only

import { type CatalogNeedOption, ChangeService, Feature, Kind, type Need } from "@dockwell/api";
import { Button, cx, Icon } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useId, useState } from "react";

import { useMay } from "../auth/may";

/** The choice that leaves an optional need unmet, as the control plane names it. */
export const UNSET = "unset";

/** Whether a need is for storage, which a connection meets (ADR 0099). */
export function isStorage(need: Need): boolean {
  return need.kind === Kind.S3;
}

/** What a need asks of a bucket, each once, such as signed URLs. */
export function needFeatures(need: Need): Feature[] {
  return [...new Set(need.accepts.flatMap((a) => a.features))].sort((a, b) => a - b);
}

/** Whether an install admin is the one asking, who adds a connection and links one. */
export function useMayLink() {
  return useMay()(ChangeService.method.proposeChange, { install: true });
}

/** What a need asks of a bucket, in a line, such as "Needs signed URLs". */
export function useFeatureLine(need: Need): string {
  const { t } = useLingui();
  const words: Partial<Record<Feature, string>> = {
    [Feature.PRESIGNED_URLS]: t`signed URLs`,
    [Feature.VIRTUAL_HOST]: t`the bucket in the host name`,
    [Feature.PATH_STYLE]: t`the bucket in the path`,
    [Feature.MULTIPART]: t`uploads in parts`,
    [Feature.BATCH_DELETE]: t`deleting many objects at once`,
    [Feature.CORS]: t`uploads straight from browsers`,
    [Feature.PUBLIC_READ]: t`reading objects at its public address`,
    [Feature.OBJECT_ACL]: t`an ACL on each object`,
    [Feature.PRESIGNED_POST]: t`form uploads from browsers`,
    [Feature.SDK_CHECKSUMS]: t`the checksums AWS's SDKs send`,
  };
  const named = needFeatures(need)
    .map((f) => words[f])
    .filter((w): w is string => !!w);
  if (named.length === 0) return t`Any S3-compatible bucket`;
  const list = named.join(", ");
  return t`Needs ${list}`;
}

/**
 * A storage need on the install sheet (ADR 0099): what meets it in a line, with
 * Change, or, while changing or with nothing picked, its ways, each a card to pick:
 * the app's own disk where the need is optional, and each connection the new project
 * may use, one that can't meet it off with why. An install admin adds a connection
 * from here, which then meets it.
 */
export function StorageNeed({
  need,
  app,
  options,
  value,
  picking,
  disabled,
  onChange,
  onPick,
  onAdd,
}: {
  need: Need;
  app: string;
  options: readonly CatalogNeedOption[];
  /** The choice picked, "" where none can be. */
  value: string;
  /** Its ways are open, as Change opened them. */
  picking: boolean;
  disabled?: boolean;
  onChange: () => void;
  onPick: (choice: string) => void;
  /** Opens the sheet that adds a connection, whose name then meets the need. */
  onAdd: () => void;
}) {
  const { t } = useLingui();
  const id = useId();
  const may = useMayLink();
  const takes = useFeatureLine(need);
  const own = options.find((o) => o.choice === UNSET);
  const connections = options.filter((o) => o.connection !== "");
  const usable = connections.filter((o) => o.why === "");
  const picked = options.find((o) => o.choice === value && o.why === "");
  const add = may.allowed ? (
    <Button size="sm" variant="ghost" icon="plus" onClick={onAdd} disabled={disabled}>
      <Trans>Add a connection</Trans>
    </Button>
  ) : null;

  if (!picking && picked) {
    const label = picked.connectionLabel || picked.connection;
    const what = need.label.toLowerCase();
    const canChange = (own ? 1 : 0) + usable.length > 1 || may.allowed;
    return (
      <fieldset className="catalog-chosen" aria-labelledby={id}>
        <span id={id} className="catalog-chosen-label">
          {need.label}
        </span>
        <span className="catalog-chosen-value">
          <span>{picked.choice === UNSET ? t`On its own disk` : label}</span>
          {canChange ? (
            picked.choice === UNSET ? (
              <button type="button" className="catalog-change" onClick={onChange}>
                <Trans>Use a connection</Trans>
              </button>
            ) : (
              <button type="button" className="catalog-change" aria-label={t`Change ${what}`} onClick={onChange}>
                <Trans>Change</Trans>
              </button>
            )
          ) : null}
        </span>
      </fieldset>
    );
  }
  return (
    <fieldset className="catalog-engines catalog-need" aria-labelledby={id} disabled={disabled}>
      <span id={id} className="catalog-chosen-label">
        {need.label}
      </span>
      <span className="catalog-need-ways">
        <span className="catalog-need-takes">{takes}</span>
        {own ? (
          <label className={cx("catalog-need-server", value === UNSET && "is-picked")}>
            <input type="radio" name={id} value={UNSET} checked={value === UNSET} onChange={() => onPick(UNSET)} />
            <span className="catalog-need-server-name">
              <Trans>On its own disk</Trans>
            </span>
            <span className="catalog-need-server-state">
              <Trans>{app} keeps its files beside it</Trans>
            </span>
          </label>
        ) : null}
        {connections.map((o) => (
          <label
            key={o.choice}
            className={cx("catalog-need-server", o.choice === value && "is-picked", o.why && "is-off")}
          >
            <input
              type="radio"
              name={id}
              value={o.choice}
              checked={o.choice === value}
              disabled={o.why !== ""}
              onChange={() => onPick(o.choice)}
            />
            <span className="catalog-need-server-name">{o.connectionLabel || o.connection}</span>{" "}
            {o.connectionLabel && o.connectionLabel !== o.connection ? (
              <code className="catalog-engine-version">{o.connection}</code>
            ) : null}
            <span className="catalog-need-server-state">
              {o.why ? <Reason why={o.why} /> : t`Checked from ${app}'s server before it's planned`}
            </span>
          </label>
        ))}
        {usable.length === 0 && !own ? (
          <span className="catalog-need-server is-off">
            <Icon name="shield" size={14} className="catalog-chosen-icon" />
            <span className="catalog-need-server-state">
              <Trans>No connection meets it yet</Trans>
            </span>
          </span>
        ) : null}
        {add ? <span>{add}</span> : null}
      </span>
    </fieldset>
  );
}

/** A reason the control plane words to follow a colon, as a sentence of its own. */
function capital(why: string): string {
  return why.charAt(0).toUpperCase() + why.slice(1);
}

/** A reason's first clause, which fits a line, as "It serves another app already". */
function firstClause(why: string): string {
  // A ", which" clause says why, so it stays (slice 2 re-check, N5).
  return why.match(/^(.*?)(?:, and |, so |\. )/)?.[1] ?? why.replace(/\.$/, "");
}

/**
 * Why a way can't be taken: its first clause in a line, on a phone too, with the
 * whole a step away behind Why (slice 2 review, L3).
 */
export function Reason({ why }: { why: string }) {
  const [open, setOpen] = useState(false);
  const full = capital(why);
  const short = firstClause(full);
  if (short === full) return <>{full}</>;
  return (
    <>
      {open ? full : short}{" "}
      <button
        type="button"
        className="catalog-change"
        aria-expanded={open}
        onClick={(event) => {
          event.preventDefault();
          setOpen(!open);
        }}
      >
        {open ? <Trans context="show less of the rest">Less</Trans> : <Trans>Why</Trans>}
      </button>
    </>
  );
}
