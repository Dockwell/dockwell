// SPDX-License-Identifier: AGPL-3.0-only

import { ChangeService, type Server } from "@dockwell/api";
import { Button, cx, Tag } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { useMay } from "../auth/may";
import { EditLabelsSheet } from "./EditLabelsSheet";
import "./LabelChips.css";

/**
 * A server's labels as tags, with the way to change them beside them for whoever may:
 * Add labels while it has none, an edit button once it has some. Someone who may not
 * sees the tags alone.
 */
export function ServerLabels({
  server,
  className,
  labelled,
}: {
  server: Server;
  className?: string;
  /** Says what they are ahead of them, as a phone's status card has no other heading for them. */
  labelled?: string;
}) {
  const { t } = useLingui();
  const may = useMay()(ChangeService.method.planServerLabels, { install: true });
  const [editing, setEditing] = useState(false);
  const keys = Object.keys(server.labels).sort();
  const name = server.name;
  if (keys.length === 0 && !may.allowed) return null;
  const tags = (
    <span className={cx("server-labels", !labelled && className)}>
      {keys.map((k) => (
        <Tag key={k} label={k} value={server.labels[k] || undefined} />
      ))}
      {may.allowed ? (
        keys.length === 0 ? (
          <Button variant="ghost" size="sm" icon="plus" onClick={() => setEditing(true)}>
            <Trans>Add labels</Trans>
          </Button>
        ) : (
          <Button
            variant="ghost"
            size="sm"
            icon="edit"
            aria-label={t`Edit ${name}'s labels`}
            title={t`Edit labels`}
            onClick={() => setEditing(true)}
          />
        )
      ) : null}
      {editing ? <EditLabelsSheet server={server} onClose={() => setEditing(false)} /> : null}
    </span>
  );
  return labelled ? (
    <div className={cx("server-labels-row", className)}>
      <span className="server-labels-term">{labelled}</span>
      {tags}
    </div>
  ) : (
    tags
  );
}
