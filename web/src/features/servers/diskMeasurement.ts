// SPDX-License-Identifier: AGPL-3.0-only

import { createClient, type Transport } from "@connectrpc/connect";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { type GetServerDiskUseResponse, ServerService } from "@dockwell/api";
import { queryOptions } from "@tanstack/react-query";

export function diskMeasurementOptions(id: string, transport: Transport) {
  const input = { id };
  const key = createConnectQueryKey({
    schema: ServerService.method.getServerDiskUse,
    input,
    cardinality: "finite",
  });
  return queryOptions({
    queryKey: [...key, "progressive"],
    queryFn: ({ signal }): Promise<GetServerDiskUseResponse> =>
      createClient(ServerService, transport).getServerDiskUse(input, { signal, headers: { Prefer: "respond-async" } }),
    staleTime: 30_000,
    refetchInterval: ({ state }) => (state.data?.diskUse && !state.data.diskUse.time ? 2_000 : 5 * 60_000),
    refetchOnWindowFocus: false,
    retry: false,
  });
}
