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
type TakeOnContainer = Pick<ServerContainer, "name" | "composeService" | "composeProject"> & {
  engineId?: string;
  id?: string;
};

export function useTakeOn(serverId: string, onApplied?: () => void) {
  const transport = useTransport();
  const sheet = useChangeSheet();
  const navigate = useNavigate();
  const places = usePlaces();

  /** The environment a container goes to unless a person picks another. */
  const environmentFor = (container: TakeOnContainer): string => {
    const project = container.composeProject;
    const joins = project ? places.environments.find((e) => e.project === labelFor(project)) : undefined;
    return joins?.id || places.defaultEnvironment?.id || "";
  };

  const take = (container: TakeOnContainer, service = serviceNameFor(container), environment = "") => {
    const expectedContainerId = container.engineId || container.id;
    sheet.open({
      kind: "plan",
      make: () =>
        createClient(ContainerService, transport).adoptContainer({
          id: container.name,
          ...(expectedContainerId ? { expectedContainerId } : {}),
          server: serverId,
          service,
          environment: environment || environmentFor(container),
        }),
      onClose: (change) => {
        const made = change?.plan?.resources[0]?.id;
        if (change?.state === ChangeState.APPLIED && made) {
          if (onApplied) onApplied();
          else void navigate({ to: "/apps/$serviceId", params: { serviceId: made } });
        }
      },
    });
  };

  return { take, environmentFor };
}
