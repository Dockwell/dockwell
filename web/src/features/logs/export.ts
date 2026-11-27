// SPDX-License-Identifier: AGPL-3.0-only

import { useCallback, useEffect, useState } from "react";

/** Triggers a plain-text log download in the browser. */
export function downloadLogText(text: string, filename: string) {
  if (!text) return;
  const blob = new Blob([text], { type: "text/plain;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10_000);
}

/**
 * Copies log text built lazily on click so live streams don't allocate export
 * strings on every incoming batch.
 */
export function useCopyLogText(build: () => string) {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return undefined;
    const timer = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(timer);
  }, [copied]);
  const copy = useCallback(async () => {
    const text = build();
    if (!text) return;
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
    } catch {
      // Clipboard access was refused; Download still works.
    }
  }, [build]);
  return { copied, copy };
}
