// SPDX-License-Identifier: AGPL-3.0-only

import { Button } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useEffect, useState } from "react";

/** A small button that copies text, and says so. */
export function CopyButton({ text, label }: { text: string; label: string }) {
  const { t } = useLingui();
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

  return (
    <>
      <Button
        variant="ghost"
        size="sm"
        className="copy-button"
        icon={copied ? "check" : "copy"}
        aria-label={copied ? t`Copied ${text}` : label}
        onClick={() => void copy()}
      />
      <span className="dw-sr" aria-live="polite">
        {copied ? t`Copied ${text}` : ""}
      </span>
    </>
  );
}
