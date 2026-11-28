// SPDX-License-Identifier: AGPL-3.0-only

import { BrandMark, cx } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { MAIL_PROVIDERS, type MailProviderId } from "./mail";
import { MailMark, StorageMark } from "./ProviderMark";
import { PROVIDERS, type ProviderId } from "./providers";

/** What a connection reaches: storage or a mail server, or either. */
export type ConnectionKindPick = "storage" | "mail";

/**
 * The services a connection is made for, as tiles in two groups, storage and mail:
 * picking one starts the form at it (ADR 0099). Only one group shows where the
 * connection's kind is settled, as for an install's need or a saved connection.
 */
export function ServiceTiles({
  picked,
  onPick,
  onCustom,
  only,
  disabled,
  pressed = true,
}: {
  picked?: ProviderId | MailProviderId | undefined;
  onPick: (id: ProviderId | MailProviderId) => void;
  onCustom?: (() => void) | undefined;
  only?: ConnectionKindPick | undefined;
  disabled?: boolean;
  /** Tiles say which is picked, as in a form; on a page they only start one. */
  pressed?: boolean;
}) {
  const { t } = useLingui();
  const groups: {
    kind: ConnectionKindPick;
    label: string;
    tiles: { id: ProviderId | MailProviderId; mark: ReactNode; name: string; full: string }[];
  }[] = [
    {
      kind: "storage",
      label: t`Storage`,
      // Each tile's name fits one line, so the tiles are one height (#799): the full
      // name is what a screen reader says, and a hover shows.
      tiles: PROVIDERS.map((p) => ({
        id: p.id,
        mark: <StorageMark id={p.id} />,
        name: p.short || p.name || t({ message: "Other", context: "another S3-compatible store, as a tile" }),
        full: p.name || t`Another S3-compatible store`,
      })),
    },
    {
      kind: "mail",
      label: t`Mail`,
      tiles: MAIL_PROVIDERS.map((p) => ({
        id: p.id,
        mark: <MailMark id={p.id} />,
        name: p.name || t({ message: "Other", context: "another mail server, as a tile" }),
        full: p.name || t`Another mail server`,
      })),
    },
  ];
  const shown = groups.filter((g) => only === undefined || g.kind === only);
  return (
    <div className="connection-service-groups">
      {shown.map((g) => (
        <fieldset key={g.kind} className="connection-stores" disabled={disabled}>
          <legend>{g.label}</legend>
          <div className="connection-store-grid">
            {g.tiles.map((x) => (
              <button
                key={x.id}
                type="button"
                className={cx("connection-store", pressed && picked === x.id && "is-picked")}
                aria-pressed={pressed ? picked === x.id : undefined}
                aria-label={x.full}
                title={x.full === x.name ? undefined : x.full}
                onClick={() => onPick(x.id)}
              >
                {x.mark}
                <span className="connection-store-name">{x.name}</span>
              </button>
            ))}
          </div>
        </fieldset>
      ))}
      {onCustom ? (
        <fieldset className="connection-stores" disabled={disabled}>
          <legend>{t`Other services`}</legend>
          <div className="connection-store-grid">
            <button type="button" className="connection-store" onClick={onCustom}>
              <BrandMark icon="link" />
              <span className="connection-store-name">{t`Custom`}</span>
            </button>
          </div>
        </fieldset>
      ) : null}
    </div>
  );
}
