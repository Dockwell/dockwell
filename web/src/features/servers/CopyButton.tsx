// SPDX-License-Identifier: AGPL-3.0-only

import { Button } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useEffect, useState } from "react";

/**
 * Copies text, and says it did for 2 s. A browser that refuses, or has no
 * clipboard, as one on plain HTTP hasn't, copies nothing and throws nothing: the
 * text stays on screen to select.
 */
export function useCopy(text: string): { copied: boolean; copy: () => void } {
  const [copied, setCopied] = useState(false);

  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(timer);
  }, [copied]);

  const copy = () => {
    void (async () => {
      try {
        await navigator.clipboard.writeText(text);
        setCopied(true);
      } catch {
        // Refused, or no clipboard; the text stays selectable.
      }
    })();
  };
  return { copied, copy };
}

/** A small button that copies text, and says so. */
export function CopyButton({ text, label }: { text: string; label: string }) {
  const { t } = useLingui();
  const { copied, copy } = useCopy(text);

  return (
    <>
      <Button
        variant="ghost"
        size="sm"
        className="copy-button"
        icon={copied ? "check" : "copy"}
        aria-label={copied ? t`Copied ${text}` : label}
        onClick={copy}
      />
      <span className="dw-sr" aria-live="polite">
        {copied ? t`Copied ${text}` : ""}
      </span>
    </>
  );
}
