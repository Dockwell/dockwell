// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { AlertService, AlertState, AlertRule_Severity as Severity } from "@dockwell/api";

import { useMay } from "../auth/may";

/**
 * How many alerts are open, and whether one is critical, for whoever may list them:
 * what a phone's More tab and its Alerts row say, as it has no rail (#794).
 */
export function useOpenAlerts(): { count: number; critical: boolean } {
  const may = useMay()(AlertService.method.listAlerts);
  const alerts = useQuery(AlertService.method.listAlerts, { firingOnly: true }, { enabled: may.allowed }).data;
  const open = (alerts?.alerts ?? []).filter((a) => a.state === AlertState.FIRING);
  return { count: alerts?.firing ?? open.length, critical: open.some((a) => a.severity === Severity.CRITICAL) };
}
