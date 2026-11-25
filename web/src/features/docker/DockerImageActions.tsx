// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type Change,
  ChangeService,
  DockerService,
  ImageService,
  type PlanDockerMaintenanceRequestSchema,
  type Server,
} from "@dockwell/api";
import { Button, Callout, Loading, Sheet, Skeleton, SkeletonFacts, StatusBadge, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { splitTag } from "../apps/ImageField";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { type ArchiveImageChoice, useArchiveActions } from "./DockerArchiveActions";
import { type ImageServer, useDockerPull } from "./DockerPullFields";
import { DockerPushSheet } from "./DockerPushSheet";
import { DockerRunSheet } from "./DockerRunSheet";

type Selection = ArchiveImageChoice & { alias: string };
type Editor = { kind: "pull" | "search"; image?: never } | { kind: "add" | "move" | "push" | "run"; image: Selection };
type PlanInput = MessageInitShape<typeof PlanDockerMaintenanceRequestSchema>;

function pullRequest(serverId: string, reference: string, loginId = "", imageId?: string): PlanInput {
  return {
    serverIds: [serverId],
    operation: { case: "pull", value: { reference, loginId, expectedLocalImageId: imageId } },
  };
}

/** Shared by the list and image detail so each action stays on the selected Engine. */
export function useImageActions(servers: readonly Pick<Server, "id" | "name">[], refresh: () => void) {
  const { t } = useLingui();
  const may = useMay();
  const mayPlan = may(DockerService.method.planDockerMaintenance).allowed;
  const canRun = may(ChangeService.method.proposeChange).allowed;
  const canSearch = may(ImageService.method.searchImages).allowed;
  const transport = useTransport();
  const sheets = useChangeSheet();
  const client = createClient(DockerService, transport);
  const overview = useQuery(
    DockerService.method.getDockerOverview,
    { serverIds: servers.map(({ id }) => id) },
    { enabled: mayPlan && servers.length > 0, retry: false, refetchOnWindowFocus: false },
  );
  const [editor, setEditor] = useState<Editor>();
  const [quick, setQuick] = useState<{ image: Selection; state: "pending" | "same" }>();
  const [failure, setFailure] = useState("");
  const request = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => request.current?.abort(), []);

  const support = new Map(overview.data?.serverSummaries.map((row) => [row.serverId, row.imageOperations]));
  const archives = useArchiveActions(servers, support, refresh);
  const pullServers = servers.flatMap((server) => {
    const capabilities = support.get(server.id);
    return capabilities?.canPull ? [{ ...server, registry: capabilities.defaultRegistry }] : [];
  });
  const capabilityPending = mayPlan && servers.length > 0 && !overview.data && overview.isFetching;
  const openEditor = (next: Editor) => {
    request.current?.abort();
    setQuick(undefined);
    setFailure("");
    setEditor(next);
  };
  const openReview = (change: Change) => {
    setEditor(undefined);
    sheets.open({ kind: "plan", make: async () => ({ change }), onClose: refresh });
  };

  const pullAgain = async (image: Selection) => {
    request.current?.abort();
    const current = new AbortController();
    request.current = current;
    setFailure("");
    setQuick({ image, state: "pending" });
    try {
      const response = await client.planDockerMaintenance(pullRequest(image.serverId, image.alias, "", image.imageId), {
        signal: current.signal,
      });
      if (current.signal.aborted) return;
      if (response.change) {
        setQuick(undefined);
        openReview(response.change);
      } else {
        setQuick(response.digestCheck?.sameContent ? { image, state: "same" } : undefined);
      }
    } catch (error) {
      if (current.signal.aborted) return;
      setQuick(undefined);
      setFailure(sentence(error));
    }
  };

  const actionsFor = (image: Selection, detail = false): MenuAction[] => {
    const capabilities = mayPlan ? support.get(image.serverId) : undefined;
    return [
      ...(canRun
        ? [
            {
              id: "run-image",
              label: t`Run image`,
              icon: "play" as const,
              onSelect: () => openEditor({ kind: "run", image }),
            },
          ]
        : []),
      ...(capabilities?.canPull && image.alias
        ? [{ id: "pull", label: t`Pull again`, icon: "download" as const, onSelect: () => void pullAgain(image) }]
        : []),
      ...(capabilities?.canTag
        ? [
            {
              id: "add",
              label: t`Add tag`,
              icon: "plus" as const,
              onSelect: () => openEditor({ kind: "add", image }),
            },
            {
              id: "move",
              label: t`Move tag`,
              icon: "arrow-right" as const,
              onSelect: () => openEditor({ kind: "move", image }),
            },
          ]
        : []),
      ...(capabilities?.canPush
        ? [
            {
              id: "push",
              label: t`Push image`,
              icon: "arrow-up" as const,
              onSelect: () => openEditor({ kind: "push", image }),
            },
          ]
        : []),
      ...archives.forImage(image, detail),
    ];
  };
  const labelFor = (image: Selection) => {
    const label = image.alias || image.imageId.replace(/^sha256:/, "").slice(0, 12);
    return t`Actions for ${label}`;
  };
  const withActions = (image: Selection, detail: boolean, extra: readonly MenuAction[]) => [
    ...extra.filter((action) => !action.danger),
    ...actionsFor(image, detail),
    ...extra.filter((action) => action.danger),
  ];
  const rowMenu = (image: Selection, detail = false, extra: readonly MenuAction[] = [], onOpen?: () => void) => {
    if (capabilityPending && extra.length === 0) {
      return (
        <Loading label={t`Loading image actions`}>
          <Skeleton shape="pill" width={28} height={28} inline />
        </Loading>
      );
    }
    return (
      <ActionMenu
        label={labelFor(image)}
        className="docker-image-menu"
        actions={withActions(image, detail, extra)}
        onOpen={onOpen}
      />
    );
  };

  const rowResult = (image: Selection) => {
    if (
      quick?.image.serverId !== image.serverId ||
      quick.image.imageId !== image.imageId ||
      quick.image.alias !== image.alias
    )
      return null;
    return quick.state === "pending" ? (
      <Loading label={t`Comparing image digests`}>
        <Skeleton shape="pill" width={88} inline />
      </Loading>
    ) : (
      <ImageCurrent />
    );
  };

  return {
    rowMenu,
    withActions,
    labelFor,
    archiveMenu: archives.header,
    leases: archives.leases,
    rowResult,
    capabilityPending,
    canPull: mayPlan && pullServers.length > 0,
    refreshCapabilities: () => {
      if (mayPlan && servers.length > 0) void overview.refetch();
    },
    openPull: () => openEditor({ kind: "pull" }),
    openSearch: canSearch && pullServers.length ? () => openEditor({ kind: "search" }) : undefined,
    sheets: (
      <>
        {archives.sheets}
        {failure ? (
          <Callout title={t`Image couldn't be checked`} tone="warning">
            {failure}
          </Callout>
        ) : null}
        {editor?.kind === "push" ? (
          <DockerPushSheet image={editor.image} onReview={openReview} onClose={() => setEditor(undefined)} />
        ) : editor?.kind === "run" ? (
          <DockerRunSheet
            servers={servers.filter(({ id }) => id === editor.image.serverId)}
            imageId={editor.image.imageId}
            imageLabel={editor.image.alias}
            onClose={() => setEditor(undefined)}
          />
        ) : editor ? (
          <DockerImageActionSheet
            editor={editor}
            servers={pullServers}
            review={(input, signal) => client.planDockerMaintenance(input, { signal })}
            onReview={openReview}
            onClose={() => setEditor(undefined)}
          />
        ) : null}
      </>
    ),
  };
}

function ImageCurrent() {
  const { t } = useLingui();
  return (
    <span role="status" className="docker-image-current">
      <StatusBadge status="healthy" label={t`Up to date`} labelIsState />
    </span>
  );
}

function DockerImageActionSheet({
  editor,
  servers,
  review,
  onReview,
  onClose,
}: {
  editor: Editor;
  servers: readonly ImageServer[];
  review: (
    input: PlanInput,
    signal: AbortSignal,
  ) => Promise<{ change?: Change; digestCheck?: { sameContent: boolean } }>;
  onReview: (change: Change) => void;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const [reference, setReference] = useState(() =>
    editor.kind === "add" && editor.image.alias ? `${splitTag(editor.image.alias).repository}:` : "",
  );
  const [result, setResult] = useState<"pending" | "same" | "unchanged">();
  const [failure, setFailure] = useState("");
  const field = useRef<HTMLInputElement>(null);
  const request = useRef<AbortController | undefined>(undefined);
  useEffect(() => () => request.current?.abort(), []);

  const searching = editor.kind === "search";
  const pulling = editor.kind === "pull" || searching;
  const busy = result === "pending";
  const edited = () => {
    setResult(undefined);
    setFailure("");
  };
  const pull = useDockerPull(servers, pulling, busy, field, edited, searching);
  const valid = pulling
    ? pull.valid
    : !!reference.trim() && !/[:/@]$/.test(reference.trim()) && !reference.includes("@");

  const submit = async () => {
    if (!valid || busy) return;
    const current = new AbortController();
    request.current = current;
    setResult("pending");
    setFailure("");
    try {
      const response = await review(
        !editor.image
          ? pullRequest(pull.serverId, pull.reference, pull.loginId)
          : {
              serverIds: [editor.image.serverId],
              operation: {
                case: "tag",
                value: {
                  sourceKey: { serverId: editor.image.serverId, imageId: editor.image.imageId },
                  destination: reference.trim(),
                  moveExisting: editor.kind === "move",
                },
              },
            },
        current.signal,
      );
      if (current.signal.aborted) return;
      if (response.change) onReview(response.change);
      else setResult(response.digestCheck?.sameContent ? "same" : "unchanged");
    } catch (error) {
      if (current.signal.aborted) return;
      setResult(undefined);
      setFailure(sentence(error));
    }
  };

  const title = searching
    ? t`Search registries`
    : pulling
      ? t`Pull image`
      : editor.kind === "add"
        ? t`Add tag`
        : t`Move tag`;
  return (
    <Sheet
      open
      title={title}
      subtitle={editor.image ? editor.image.alias || editor.image.imageId : undefined}
      onClose={onClose}
      initialFocusRef={field}
      footer={
        result === "same" || result === "unchanged" ? (
          <Button variant="primary" onClick={onClose}>
            <Trans>Done</Trans>
          </Button>
        ) : (
          <>
            <Button variant="ghost" onClick={onClose}>
              <Trans>Cancel</Trans>
            </Button>
            <Button
              variant="primary"
              icon={pulling ? "search" : "commit"}
              loading={busy}
              disabled={!valid || busy}
              onClick={() => void submit()}
            >
              {pulling ? (
                pull.installed ? (
                  <Trans>Check for updates</Trans>
                ) : (
                  <Trans>Review pull</Trans>
                )
              ) : (
                <Trans>Review plan</Trans>
              )}
            </Button>
          </>
        )
      }
    >
      <fieldset className="docker-image-fields" disabled={busy}>
        {pulling ? (
          pull.fields
        ) : (
          <TextField
            ref={field}
            label={editor.kind === "move" ? t`Tag to move` : t`New tag`}
            placeholder="example/web:latest"
            mono
            value={reference}
            onChange={(event) => {
              edited();
              setReference(event.target.value);
            }}
          />
        )}
      </fieldset>
      {busy && !pulling ? (
        <Loading label={t`Checking the tag`} className="docker-image-compare">
          <SkeletonFacts count={3} layout="rows" />
        </Loading>
      ) : null}
      {result === "same" ? <ImageCurrent /> : null}
      {result === "unchanged" ? <Callout title={t`This tag already points to the image`} tone="healthy" /> : null}
      {failure ? (
        <Callout title={t`Image action needs attention`} tone="warning">
          {failure}
        </Callout>
      ) : null}
    </Sheet>
  );
}
