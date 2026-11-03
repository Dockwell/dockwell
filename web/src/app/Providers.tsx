// SPDX-License-Identifier: AGPL-3.0-only

import type { Transport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { type UIStrings, UIStringsProvider } from "@dockwell/ui";
import { I18nProvider } from "@lingui/react";
import { useLingui } from "@lingui/react/macro";
import { type QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { type ReactNode, useMemo } from "react";

import { i18n } from "./i18n";

/** Gives the design system's own words (status names, button labels) in the active locale. */
function TranslatedUIStrings({ children }: { children: ReactNode }) {
  const { t } = useLingui();
  const strings = useMemo<UIStrings>(
    () => ({
      status: {
        healthy: t`Healthy`,
        degraded: t`Degraded`,
        down: t`Down`,
        deploying: t`Deploying`,
        stopped: t`Stopped`,
      },
      remove: (what) => t`Remove ${what}`,
    }),
    [t],
  );
  return <UIStringsProvider strings={strings}>{children}</UIStringsProvider>;
}

export interface ProvidersProps {
  transport: Transport;
  queryClient: QueryClient;
  children: ReactNode;
}

/** Everything a screen needs: translations, the API and the query cache. */
export function Providers({ transport, queryClient, children }: ProvidersProps) {
  return (
    <I18nProvider i18n={i18n}>
      <TranslatedUIStrings>
        <TransportProvider transport={transport}>
          <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
        </TransportProvider>
      </TranslatedUIStrings>
    </I18nProvider>
  );
}
