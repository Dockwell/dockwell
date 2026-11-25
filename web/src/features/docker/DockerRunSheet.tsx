// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  ChangeState,
  DockerObjectKind,
  DockerService,
  type EditSchema,
  ResourceService,
  type Server,
} from "@dockwell/api";
import { Button, Callout, CodeField, SelectField, Sheet, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { nameFromImage, toLabel } from "../apps/app";
import { usePlaces } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { parseDockerRun, type RunApp } from "./dockerRun";

type Props = {
  servers: readonly Pick<Server, "id" | "name">[];
  imageId?: string | undefined;
  imageLabel?: string | undefined;
  onClose: () => void;
};

export function DockerRunSheet({ servers, imageId, imageLabel, onClose }: Props) {
  const { t } = useLingui();
  const places = usePlaces();
  const may = useMay();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const navigate = useNavigate();
  const [command, setCommand] = useState(imageId ? `docker run ${imageId}` : "");
  const [name, setName] = useState<string>();
  const [server, setServer] = useState(servers.length === 1 ? (servers[0]?.id ?? "") : "");
  const [environment, setEnvironment] = useState<string>();
  const scope = environment ?? places.defaultEnvironment?.id ?? places.environments[0]?.id ?? "";
  const parsed = parseDockerRun(command);
  const app = parsed.app;
  const initialName =
    parsed.name ||
    (imageId && imageId === app?.image
      ? imageLabel
        ? nameFromImage(imageLabel)
        : `image-${imageId.slice(7, 19)}`
      : nameFromImage(app?.image ?? ""));
  const appName = name ?? toLabel(initialName);
  const permission = may(ChangeService.method.proposeChange, scope ? { environment: scope } : undefined);
  const validName = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(appName);
  const valid = app && !parsed.issues.length && validName && !!server && !!scope && permission.allowed;
  const syntax =
    parsed.syntax && command.trim()
      ? {
          command: t`Paste a docker run command.`,
          quote: t`Close the quote or complete the line continuation.`,
          image: t`Add the image name after the options.`,
          length: t`Use a command under 32 KB.`,
        }[parsed.syntax]
      : undefined;

  const review = () => {
    if (!app || !valid) return;
    sheet.open({
      kind: "plan",
      make: async () => {
        const docker = createClient(DockerService, transport);
        const mounts = [...(app.mounts ?? [])];
        const named = [
          ...new Set(mounts.flatMap((m) => (m.source?.case === "volume" && m.source.value ? [m.source.value] : []))),
        ];
        const existing = new Set<string>();
        const managed = new Set<string>();
        const readError = t`Docker couldn't verify existing volumes. Refresh the server and review again.`;
        if (named.length) {
          const resources = await createClient(ResourceService, transport).listResources({ kind: "volume", scope });
          for (const resource of resources.resources) {
            if (resource.resource?.name) managed.add(resource.resource.name);
          }
        }
        for (const volume of named) {
          let pageToken = "";
          for (let page = 0; page < 8; page++) {
            const result = await docker.listDockerObjects({
              kind: DockerObjectKind.VOLUME,
              serverIds: [server],
              filter: { search: volume },
              pageToken,
            });
            if (result.unavailable.length) throw new Error(readError);
            if (result.objects.some((row) => row.detail.case === "volume" && row.name === volume)) {
              existing.add(volume);
              break;
            }
            pageToken = result.nextPageToken;
            if (!pageToken) break;
            if (page === 7) throw new Error(readError);
          }
        }
        const volumes = new Set<string>();
        const prepared = mounts.map((m, i) => {
          if (m.source?.case !== "volume") return m;
          const volume = m.source.value || `${appName.slice(0, 43)}-volume-${i + 1}`;
          if (existing.has(volume)) return { ...m, external: true };
          if (!managed.has(volume)) volumes.add(volume);
          return { ...m, source: { case: "volume" as const, value: volume } };
        });
        const edits: MessageInitShape<typeof EditSchema>[] = [...volumes].map((volume) => ({
          resource: { kind: "volume", scope, name: volume },
          spec: { kind: { case: "volume", value: {} } },
          servers: [server],
          mustCreate: true,
        }));
        const preparedApp: RunApp = { ...app, mounts: prepared };
        edits.push({
          resource: { kind: "service", scope, name: appName },
          spec: { kind: { case: "service", value: { runs: { case: "app", value: preparedApp } } } },
          servers: [server],
          mustCreate: true,
        });
        return createClient(ChangeService, transport).proposeChange({
          summary: t`Run ${appName} as a Dockwell app`,
          edits,
        });
      },
      onClose: (result) => {
        if (result?.state !== ChangeState.APPLIED) return;
        onClose();
        const service = result.plan?.resources.find((r) => r.resource?.kind === "service");
        if (service?.id) void navigate({ to: "/apps/$serviceId/logs", params: { serviceId: service.id } });
      },
    });
  };

  return (
    <Sheet
      open
      title={imageId ? t`Run image` : t`Paste docker run`}
      subtitle={imageLabel}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" disabled={!valid} {...permission.off} onClick={review}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="docker-image-fields">
        <CodeField
          label={t`Docker run command`}
          lines={5}
          value={command}
          error={syntax}
          placeholder="docker run --name web -d -p 8080:80 nginx:alpine"
          onChange={(event) => setCommand(event.target.value)}
        />
        {parsed.issues.length ? (
          <Callout tone="warning" title={t`Options that need editing`}>
            <ul>
              {parsed.issues.map((issue) => (
                <li key={issue}>
                  {issue === "shell" ? (
                    <Trans>Shell syntax or expansion. Use literal Docker arguments.</Trans>
                  ) : (
                    <code>{issue}</code>
                  )}
                </li>
              ))}
            </ul>
          </Callout>
        ) : null}
        {app ? (
          <>
            <TextField
              label={t`App name`}
              value={appName}
              maxLength={63}
              onChange={(event) => setName(event.target.value)}
            />
            {servers.length !== 1 ? (
              <SelectField
                label={t`Server`}
                value={server}
                options={[
                  { value: "", label: t`Choose a server` },
                  ...servers.map(({ id, name }) => ({ value: id, label: name })),
                ]}
                onChange={(event) => setServer(event.target.value)}
              />
            ) : null}
            {places.environments.length > 1 ? (
              <SelectField
                label={t`Environment`}
                value={scope}
                options={places.environments.map(({ id, label }) => ({ value: id, label }))}
                onChange={(event) => setEnvironment(event.target.value)}
              />
            ) : null}
            {app.mounts?.some((m) => m.source?.case === "volume" && !m.source.value) ? (
              <Callout tone="info" title={t`Anonymous mounts get new managed volumes`} />
            ) : null}
          </>
        ) : null}
      </div>
    </Sheet>
  );
}
