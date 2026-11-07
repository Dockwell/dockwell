// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { ListServicesResponseSchema, ServiceInfoSchema, ServiceService } from "@dockwell/api";
import { QueryClient, QueryObserver } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";

import { readServicesAgain } from "./useServices";

const web = create(ServiceInfoSchema, { id: "svc_web" });
const gone = create(ServiceInfoSchema, { id: "svc_gone" });

/** A query a screen holds, which counts how often it's read. */
function held(client: QueryClient, queryKey: readonly unknown[], data: () => unknown) {
  const read = vi.fn(async () => data());
  const observer = new QueryObserver(client, { queryKey, queryFn: read, staleTime: Number.POSITIVE_INFINITY });
  const stop = observer.subscribe(() => {});
  return { read, stop };
}

const revisionsOf = (id: string) =>
  createConnectQueryKey({ schema: ServiceService.method.listServiceRevisions, input: { id }, cardinality: "infinite" });
const list = createConnectQueryKey({ schema: ServiceService.method.listServices, input: {}, cardinality: "finite" });

describe("readServicesAgain", () => {
  const stops: (() => void)[] = [];
  afterEach(() => {
    for (const stop of stops.splice(0)) stop();
  });

  // The page an app was deleted from shows under the plan sheet until it
  // closes, and must not ask for the app's revisions once more (#112).
  it("doesn't ask after a service the list no longer has", async () => {
    const client = new QueryClient();
    let services = [web, gone];
    const listed = held(client, list, () => create(ListServicesResponseSchema, { services }));
    const ofWeb = held(client, revisionsOf(web.id), () => ({ pages: [] }));
    const ofGone = held(client, revisionsOf(gone.id), () => ({ pages: [] }));
    stops.push(listed.stop, ofWeb.stop, ofGone.stop);
    await vi.waitFor(() => expect(ofGone.read).toHaveBeenCalledTimes(1));
    await vi.waitFor(() => expect(listed.read).toHaveBeenCalledTimes(1));

    services = [web];
    await readServicesAgain(client);
    expect(listed.read).toHaveBeenCalledTimes(2);
    expect(ofWeb.read).toHaveBeenCalledTimes(2);
    expect(ofGone.read).toHaveBeenCalledTimes(1);
  });

  it("reads every service's own with no list to go by", async () => {
    const client = new QueryClient();
    const ofWeb = held(client, revisionsOf(web.id), () => ({ pages: [] }));
    stops.push(ofWeb.stop);
    await vi.waitFor(() => expect(ofWeb.read).toHaveBeenCalledTimes(1));
    await readServicesAgain(client);
    expect(ofWeb.read).toHaveBeenCalledTimes(2);
  });
});
