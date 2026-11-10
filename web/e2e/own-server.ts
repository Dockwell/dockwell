// SPDX-License-Identifier: AGPL-3.0-only
// The page of the runner's own server, the control server, for a flow that works on
// it. The fleet's Overview links a server only when something is wrong with it, as
// Overview-Fleet draws the rest as tiles, so a flow asks the API which server it is
// rather than take the first link it finds, which may be a server another flow joined.
import type { Page } from "playwright-core";

export async function ownServerPath(page: Page, origin: string): Promise<string> {
  const res = await page.request.get(`${origin}/v1/servers`);
  if (!res.ok()) throw new Error(`GET /v1/servers answered ${res.status()}`);
  const { servers } = (await res.json()) as { servers?: { id: string; name: string; controlServer?: boolean }[] };
  const own = servers?.find((server) => server.controlServer);
  if (!own) throw new Error(`the API names no control server among ${JSON.stringify(servers)}`);
  return `/servers/${own.id}`;
}
