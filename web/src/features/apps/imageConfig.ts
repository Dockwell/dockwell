// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { ImageAnswer, type ImageConfig, ImageService, type ServiceInfo } from "@dockwell/api";

import { useMay } from "../auth/may";
import { environmentOf } from "./app";

/**
 * What the image an app runs declares, read from its registry through the
 * server it runs on (ADR 0062), so a form shows the image's own command, user
 * and health check where the app sets none. Undefined while it's read, or when
 * it can't be.
 */
export function useImageConfig(service: ServiceInfo, image: string): ImageConfig | undefined {
  const may = useMay();
  const environment = environmentOf(service);
  const server = service.deployments[0]?.serverId ?? "";
  const allowed = may(ImageService.method.getImage, environment ? { environment } : undefined).allowed;
  const read = useQuery(
    ImageService.method.getImage,
    { reference: image, environment, server },
    { enabled: allowed && image !== "" && server !== "", staleTime: 10 * 60_000, retry: false },
  );
  return read.data?.answer === ImageAnswer.FOUND ? read.data.config : undefined;
}
