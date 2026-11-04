// SPDX-License-Identifier: AGPL-3.0-only

import { Button, type ButtonProps, cx } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useEffect, useState } from "react";

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

export interface CopyButtonProps extends Omit<ButtonProps, "onClick" | "icon" | "children" | "aria-label"> {
  /** What the button copies. */
  text: string;
  /** What the button says, such as "Copy codes". */
  label: string;
  /** What it says once it has copied, such as "Codes copied", which screen readers hear too. */
  copiedLabel: string;
  /** Shows only the icon, with label as the button's name. */
  iconOnly?: boolean;
}

/**
 * A button that copies text to the clipboard. It says so for 2 s, then is
 * ready to copy again. If the browser refuses, the text stays on screen to
 * select by hand.
 */
export function CopyButton({ text, label, copiedLabel, iconOnly = false, size = "sm", ...button }: CopyButtonProps) {
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(timer);
  }, [copied]);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
    } catch {
      // Clipboard access was refused; the text stays selectable.
    }
  };

  const name = copied ? copiedLabel : label;
  return (
    <>
      <Button
        {...button}
        size={size}
        icon={copied ? "check" : "copy"}
        aria-label={iconOnly ? name : undefined}
        onClick={() => void copy()}
      >
        {iconOnly ? null : name}
      </Button>
      <span className="dw-sr" aria-live="polite">
        {copied ? copiedLabel : ""}
      </span>
    </>
  );
}
