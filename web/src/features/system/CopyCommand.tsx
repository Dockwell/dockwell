// SPDX-License-Identifier: AGPL-3.0-only

import { Button, cx } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useEffect, useState } from "react";

import "./CopyCommand.css";

/** A command in mono on a sunken well, with a button that copies it. */
export function CopyCommand({ command, className }: { command: string; className?: string }) {
  const { t } = useLingui();
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(timer);
  }, [copied]);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(command);
      setCopied(true);
    } catch {
      // Clipboard access was refused; the command stays selectable.
    }
  };

  return (
    <div className={cx("copy-command", className)}>
      <code>{command}</code>
      <Button
        variant="ghost"
        size="sm"
        icon={copied ? "check" : "copy"}
        aria-label={copied ? t`Command copied` : t`Copy command`}
        onClick={() => void copy()}
      />
      <span className="dw-sr" aria-live="polite">
        {copied ? t`Command copied` : ""}
      </span>
    </div>
  );
}
