// SPDX-License-Identifier: AGPL-3.0-only

import { CopyButton, cx } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

import "./CopyCommand.css";

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

/** The copy button lives in @dockwell/ui, beside the connection card that uses it. */
export { CopyButton, type CopyButtonProps } from "@dockwell/ui";
