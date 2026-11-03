// SPDX-License-Identifier: AGPL-3.0-only

import { createRouterTransport, type ServiceImpl } from "@connectrpc/connect";
import { SystemService } from "@dockwell/api";
import { QueryClient } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import type { ReactElement } from "react";

import { Providers } from "../app/Providers";

/** Renders ui with the real providers, an in-memory API, and a query cache that doesn't retry. */
export function renderWithApi(ui: ReactElement, system: Partial<ServiceImpl<typeof SystemService>>) {
  const transport = createRouterTransport(({ service }) => {
    service(SystemService, system);
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <Providers transport={transport} queryClient={queryClient}>
      {ui}
    </Providers>,
  );
}
