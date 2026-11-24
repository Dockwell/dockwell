// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { ChangeService, DomainService, type Resource, type ServiceInfo, ServiceService } from "@dockwell/api";
import { Button, Loading, Sheet, SkeletonFacts } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useRef, useState } from "react";
import { useMay } from "../auth/may";
import type { MenuAction } from "../shell/ActionMenu";
import { ReadFailed } from "../shell/ReadFailed";
import { environmentOf, nameOf } from "./app";
import { useDeleteAppPlan } from "./deleteApp";

export function useServiceRowActions<Service>(environment: (service: Service) => string) {
  const { t } = useLingui();
  const may = useMay();
  const [selected, setSelected] = useState<{ service: Service; action: "move" | "delete" }>();
  const actionsFor = (service: Service): MenuAction[] => {
    const where = { environment: environment(service) };
    return [
      {
        id: "move",
        icon: "arrow-right",
        label: t`Move to a project`,
        ...may(ServiceService.method.moveService, where).off,
        onSelect: () => setSelected({ service, action: "move" }),
      },
      {
        id: "delete",
        icon: "trash",
        label: t`Delete`,
        danger: true,
        ...may(ChangeService.method.proposeChange, where).off,
        onSelect: () => setSelected({ service, action: "delete" }),
      },
    ];
  };
  return { actionsFor, selected, close: () => setSelected(undefined) };
}

export function PreparingServiceAction({
  title,
  error,
  retry,
  onClose,
}: {
  title: string;
  error?: Error | null;
  retry?: () => void;
  onClose: () => void;
}) {
  return (
    <Sheet open title={title} onClose={onClose}>
      {error ? (
        <ReadFailed
          error={error}
          title={title}
          actions={
            retry && (
              <Button onClick={retry}>
                <Trans>Try again</Trans>
              </Button>
            )
          }
        />
      ) : (
        <Loading label={title}>
          <SkeletonFacts count={3} />
        </Loading>
      )}
    </Sheet>
  );
}

/** Read the service's routes and project once there's a Delete to plan. */
export function PrepareServiceDelete({
  service,
  after,
  alongside,
  onClose,
}: {
  service: ServiceInfo;
  after?: "/apps" | "/databases";
  alongside?: readonly Resource[];
  onClose: () => void;
}) {
  const { t } = useLingui();
  const name = nameOf(service);
  const domains = useQuery(
    DomainService.method.listDomains,
    { environment: environmentOf(service), service: name },
    { staleTime: 0 },
  );
  const plan = useDeleteAppPlan(service, domains.data?.domains ?? [], { after, alongside });
  const opened = useRef(false);
  const ready = !!domains.data && !domains.isFetching && !domains.error && !plan.pending && !plan.error;
  useEffect(() => {
    if (!ready || opened.current) return;
    opened.current = true;
    onClose();
    plan.open();
  }, [ready, plan, onClose]);
  if (ready) return null;
  return (
    <PreparingServiceAction
      title={t`Prepare to delete ${name}`}
      error={domains.error ?? plan.error}
      retry={domains.error ? () => void domains.refetch() : plan.retry}
      onClose={onClose}
    />
  );
}
