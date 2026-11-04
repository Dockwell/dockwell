// SPDX-License-Identifier: AGPL-3.0-only

import { useLingui } from "@lingui/react/macro";
import { useCallback } from "react";

import { describeUserAgent } from "../../lib/userAgent";

/** Names a browser by its User-Agent, such as "Chrome on macOS", or "" when it can't tell. */
export function useDeviceLabel(): (userAgent: string) => string {
  const { t } = useLingui();
  return useCallback(
    (userAgent: string) => {
      const { browser, os } = describeUserAgent(userAgent);
      if (browser && os) return t`${browser} on ${os}`;
      return browser || os;
    },
    [t],
  );
}

/** What to call a passkey made in this browser, such as "Chrome on macOS". */
export function useDeviceName(): string {
  return useDeviceLabel()(navigator.userAgent);
}
