// SPDX-License-Identifier: AGPL-3.0-only

import { Button, CopyButton, cx } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useId, useState } from "react";

import "./CopyCommand.css";

export { CopyButton, type CopyButtonProps } from "@dockwell/ui";

/** A command in mono on a sunken well, with a button that copies it. */
export function CopyCommand({ command, className }: { command: string; className?: string }) {
  const { t } = useLingui();
  return (
    <div className={cx("copy-command", className)}>
      <code>{command}</code>
      <CopyButton text={command} label={t`Copy command`} copiedLabel={t`Command copied`} iconOnly variant="ghost" />
    </div>
  );
}

/**
 * The command the dockwell CLI runs to do what a form would, for whoever would
 * rather script it: a button that copies it, and the command itself on asking.
 * Values a form keeps secret aren't in it; the CLI reads those from the
 * environment, as the command says by naming them alone.
 */
export function CopyAsCommand({ command, className }: { command: string; className?: string }) {
  const { t } = useLingui();
  const [shown, setShown] = useState(false);
  const id = useId();
  return (
    <div className={cx("copy-as-command", className)}>
      <div className="copy-as-command-actions">
        <CopyButton text={command} label={t`Copy as CLI command`} copiedLabel={t`Copied the command`} variant="ghost" />
        <Button
          variant="ghost"
          size="sm"
          icon={shown ? "chevron-down" : "chevron-right"}
          aria-expanded={shown}
          aria-controls={shown ? id : undefined}
          onClick={() => setShown((was) => !was)}
        >
          {shown ? <Trans>Hide it</Trans> : <Trans>Show it</Trans>}
        </Button>
      </div>
      {shown ? (
        <div id={id}>
          <CopyCommand command={command} className="copy-as-command-well" />
        </div>
      ) : null}
    </div>
  );
}
