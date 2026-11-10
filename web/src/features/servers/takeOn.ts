// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ChangeState, ContainerService, type ServerContainer } from "@dockwell/api";
import { useNavigate } from "@tanstack/react-router";

import { usePlaces } from "../apps/useServices";
import { useChangeSheet } from "../changes/ChangeSheet";
import { labelFor, serviceNameFor } from "./containers";

/**
 * Taking a container on as a service, as a plan: by the name and in the environment a person
 * chose, or else the ones Dockwell would. What came late to a Compose project that was taken on
 * joins it; otherwise it goes to the install's default environment. Once it's applied, the app's
 * page opens.
 */
export function useTakeOn(serverId: string) {
  const transport = useTransport();
  const sheet = useChangeSheet();
  const navigate = useNavigate();
  const places = usePlaces();

  /** The environment a container goes to unless a person picks another. */
  const environmentFor = (container: ServerContainer): string => {
    const project = container.composeProject;
    const joins = project ? places.environments.find((e) => e.project === labelFor(project)) : undefined;
    return joins?.id || places.defaultEnvironment?.id || "";
  };

  const take = (container: ServerContainer, service = serviceNameFor(container), environment = "") =>
    sheet.open({
      kind: "plan",
      make: () =>
        createClient(ContainerService, transport).adoptContainer({
          id: container.name,
          server: serverId,
          service,
          environment: environment || environmentFor(container),
        }),
      onClose: (change) => {
        const made = change?.plan?.resources[0]?.id;
        if (change?.state === ChangeState.APPLIED && made) {
          void navigate({ to: "/apps/$serviceId", params: { serviceId: made } });
        }
      },
    });

  return { take, environmentFor };
}
