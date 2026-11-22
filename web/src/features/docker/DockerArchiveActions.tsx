// SPDX-License-Identifier: AGPL-3.0-only

import { DockerArtifactKind, type DockerImageSupport, DockerService, type Server } from "@dockwell/api";
import { Button, SelectField, Sheet } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { useMay } from "../auth/may";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { DockerExportSheet, DockerImportSheet } from "./DockerArchiveSheets";
import { DockerArtifactLeases } from "./DockerArtifactLeases";
import { DockerBuildSheet } from "./DockerBuildSheet";

type ServerChoice = Pick<Server, "id" | "name">;
export interface ArchiveImageChoice {
  serverId: string;
  imageId: string;
  tags: readonly string[];
  platform: string;
}

export function useArchiveActions(
  servers: readonly ServerChoice[],
  support: ReadonlyMap<string, DockerImageSupport | undefined>,
  refresh: () => void,
) {
  const { t } = useLingui();
  const may = useMay()(DockerService.method.beginDockerArtifactUpload).allowed;
  const importable = may ? servers.filter((s) => support.get(s.id)?.canImport) : [];
  const buildable = may ? servers.filter((s) => support.get(s.id)?.canBuild) : [];
  const [editor, setEditor] = useState<
    | { import: true; restore?: { serverId: string; artifactId: string } }
    | { build: true; restore?: { serverId: string; artifactId: string; kind: DockerArtifactKind } }
    | ArchiveImageChoice
    | { choices: readonly ArchiveImageChoice[] }
  >();
  const [selected, select] = useState("");
  const close = () => setEditor(undefined);
  const canExport = (image: ArchiveImageChoice) => may && support.get(image.serverId)?.canExport;
  const importAction: MenuAction[] = importable.length
    ? [{ id: "import", label: t`Import image`, icon: "upload", onSelect: () => setEditor({ import: true }) }]
    : [];
  const exportAction = (choices: readonly ArchiveImageChoice[]): MenuAction[] =>
    choices.length
      ? [
          {
            id: "export",
            label: t`Export image`,
            icon: "download",
            onSelect: () => {
              select("");
              setEditor(choices.length === 1 ? choices[0] : { choices });
            },
          },
        ]
      : [];
  return {
    header: (images: readonly ArchiveImageChoice[]) => {
      const actions = [...importAction, ...exportAction(images.filter(canExport))];
      if (buildable.length)
        actions.unshift({
          id: "build",
          label: t`Build image`,
          icon: "layers",
          onSelect: () => setEditor({ build: true }),
        });
      return actions.length ? <ActionMenu label={t`Image actions`} actions={actions} /> : null;
    },
    forImage: (image: ArchiveImageChoice, detail: boolean) => [
      ...(detail ? importAction : []),
      ...(canExport(image) ? exportAction([image]) : []),
    ],
    leases: importable.length ? (
      <DockerArtifactLeases
        servers={importable}
        resume={(serverId, artifactId, kind) =>
          setEditor(
            kind === DockerArtifactKind.IMAGE_IMPORT
              ? { import: true, restore: { serverId, artifactId } }
              : { build: true, restore: { serverId, artifactId, kind } },
          )
        }
      />
    ) : null,
    sheets:
      editor && "import" in editor ? (
        <DockerImportSheet servers={importable} close={close} refresh={refresh} restore={editor.restore} />
      ) : editor && "build" in editor ? (
        <DockerBuildSheet servers={buildable} close={close} refresh={refresh} restore={editor.restore} />
      ) : editor && "choices" in editor ? (
        <Sheet
          open
          title={t`Export image`}
          onClose={close}
          footer={
            <>
              <Button variant="ghost" onClick={close}>
                <Trans>Cancel</Trans>
              </Button>
              <Button
                variant="primary"
                disabled={!selected}
                onClick={() => setEditor(editor.choices[Number(selected) - 1])}
              >
                <Trans>Choose image</Trans>
              </Button>
            </>
          }
        >
          <SelectField
            label={t`Image`}
            value={selected}
            options={[
              { value: "", label: t`Choose an image`, disabled: true },
              ...editor.choices.map(({ imageId, tags, serverId }, index) => {
                const image = tags[0] ?? imageId;
                const server = servers.find((s) => s.id === serverId)?.name;
                return {
                  value: String(index + 1),
                  label: servers.length > 1 && server ? t`${image} on ${server}` : image,
                };
              }),
            ]}
            onChange={(event) => select(event.target.value)}
          />
        </Sheet>
      ) : editor ? (
        <DockerExportSheet
          server={editor.serverId}
          imageId={editor.imageId}
          tags={editor.tags}
          platform={editor.platform}
          close={close}
        />
      ) : null,
  };
}
