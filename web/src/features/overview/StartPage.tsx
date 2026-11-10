// SPDX-License-Identifier: AGPL-3.0-only

import { useLingui } from "@lingui/react/macro";

import { RouterButtonLink } from "../../lib/links";
import { useFleet } from "../servers/useFleet";
import { Page } from "../shell/Page";
import { StartChoices, StartLead } from "./StartChoices";
import "./Start.css";

/**
 * Where a claim lands, as Onboarding-FirstDeploy has it: what the first app comes
 * from, each choice opening its create flow, and the overview a step away.
 */
export function StartPage() {
  const { t } = useLingui();
  const fleet = useFleet();
  const server = fleet.status === "connecting" ? "" : (fleet.servers.find((s) => s.controlServer)?.name ?? "");
  return (
    <Page
      title={t`What do you want to run?`}
      lead={<StartLead server={server} />}
      back="/"
      actions={
        <RouterButtonLink to="/" variant="ghost" icon="home">
          {t`Skip to the overview`}
        </RouterButtonLink>
      }
    >
      <StartChoices />
    </Page>
  );
}
