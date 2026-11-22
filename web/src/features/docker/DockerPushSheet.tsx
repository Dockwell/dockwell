// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeState, type DockerImagePush, DockerService } from "@dockwell/api";
import { Button, Callout, Loading, SelectField, Sheet, Skeleton, Switch, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { RouterButtonLink } from "../../lib/links";
import { splitTag } from "../apps/ImageField";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { registryOf, registryPushEdit, useRegistryLogins } from "../registries/registries";
import { shortImageId } from "./imageIdentity";

export function DockerPushSheet({
  image,
  defaults,
  onReview,
  onClose,
}: {
  image: { serverId: string; imageId: string; alias: string };
  defaults?: DockerImagePush;
  onReview: (change: Change) => void;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const client = createClient(DockerService, useTransport());
  const logins = useRegistryLogins();
  const eligible = (logins.data?.logins ?? []).filter((login) => login.projects.length === 0);
  const source = (defaults?.destination ?? image.alias).split("@")[0] ?? "";
  const host = registryOf(source);
  const first = source.split("/")[0] ?? "";
  const { repository: defaultRepo, tag: defaultTag } = splitTag(
    source.includes("/") && (first.includes(".") || first.includes(":") || first === "localhost")
      ? source.slice(source.indexOf("/") + 1)
      : source,
  );
  const [picked, pick] = useState<string>();
  const sameHost = eligible.filter((login) => login.registry === host);
  const loginId =
    picked ??
    defaults?.loginId ??
    (sameHost.length === 1 ? sameHost[0]?.id : undefined) ??
    (eligible.length === 1 ? eligible[0]?.id : undefined) ??
    "";
  const login = eligible.find((item) => item.id === loginId);
  const [repository, setRepository] = useState(defaultRepo);
  const [tag, setTag] = useState(defaultTag ?? "latest");
  const [digestOnly, setDigestOnly] = useState(defaults?.digestOnly ?? false);
  const [failure, setFailure] = useState<{ text: string; conflict: boolean }>();
  const [busy, setBusy] = useState(false);
  const [permission, setPermission] = useState<Opening>();
  const request = useRef<AbortController | undefined>(undefined);
  const tagField = useRef<HTMLInputElement>(null);
  useEffect(() => () => request.current?.abort(), []);

  const valid =
    login?.allowPush &&
    /^[a-z0-9]+(?:(?:[._-]+|\/)[a-z0-9]+)*$/.test(repository.trim()) &&
    (digestOnly || /^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$/.test(tag.trim()));

  async function review() {
    if (!valid || busy || !login) return;
    setFailure(undefined);
    setBusy(true);
    const current = new AbortController();
    request.current = current;
    const destination = `${login.registry}/${repository.trim()}${digestOnly ? "" : `:${tag.trim()}`}`;
    try {
      const response = await client.planDockerMaintenance(
        {
          serverIds: [image.serverId],
          operation: {
            case: "push",
            value: {
              sourceKey: { serverId: image.serverId, imageId: image.imageId },
              destination,
              loginId: login.id,
              digestOnly,
              credentialProject: defaults?.credentialProject ?? "",
            },
          },
        },
        { signal: current.signal },
      );
      if (!current.signal.aborted && response.change) onReview(response.change);
    } catch (error) {
      if (!current.signal.aborted) {
        setFailure({ text: sentence(error), conflict: reasonOf(error) === "docker_push_conflict" });
      }
    } finally {
      if (!current.signal.aborted) setBusy(false);
    }
  }

  return (
    <Sheet
      open
      title={t`Push image`}
      subtitle={image.alias || shortImageId(image.imageId)}
      onClose={onClose}
      over={permission ? <PlanView opening={permission} /> : undefined}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            icon="arrow-up"
            disabled={!valid || busy}
            loading={busy}
            onClick={() => void review()}
          >
            <Trans>Review push</Trans>
          </Button>
        </>
      }
    >
      <fieldset className="docker-image-fields" disabled={busy}>
        {logins.isLoading ? (
          <Loading label={t`Loading registry logins`}>
            <Skeleton height={68} />
          </Loading>
        ) : (
          <SelectField
            label={t`Registry login`}
            value={loginId}
            options={[
              { value: "", label: t`Choose a registry login` },
              ...eligible.map((item) => ({ value: item.id, label: `${item.name} · ${item.registry}` })),
            ]}
            onChange={(event) => {
              pick(event.target.value);
              setFailure(undefined);
            }}
          />
        )}
        {login && !login.allowPush ? (
          <Button
            variant="secondary"
            size="sm"
            icon="key"
            onClick={() =>
              setPermission({
                kind: "propose",
                summary: t`Allow pushes with ${login.name}`,
                edits: [registryPushEdit(login.name, true)],
                onClose: (change) => {
                  setPermission(undefined);
                  if (change?.state === ChangeState.APPLIED) void logins.refetch();
                },
              })
            }
          >
            <Trans>Allow pushes with this login</Trans>
          </Button>
        ) : (
          <RouterButtonLink to="/settings/integrations" variant="ghost" size="sm" icon="settings">
            <Trans>Manage logins</Trans>
          </RouterButtonLink>
        )}
        {logins.isError ? (
          <Callout title={t`Registry logins couldn't be loaded`} tone="warning">
            <Button variant="ghost" size="sm" onClick={() => void logins.refetch()}>
              <Trans>Retry</Trans>
            </Button>
          </Callout>
        ) : null}
        <TextField
          label={t`Repository`}
          value={repository}
          onChange={(event) => {
            setRepository(event.target.value);
            setFailure(undefined);
          }}
          placeholder="team/image"
        />
        <Switch
          label={<Trans>Publish by digest</Trans>}
          checked={digestOnly}
          onChange={(checked) => {
            setDigestOnly(checked);
            setFailure(undefined);
          }}
        />
        {digestOnly ? null : (
          <TextField
            label={t`Tag`}
            ref={tagField}
            value={tag}
            onChange={(event) => {
              setTag(event.target.value);
              setFailure(undefined);
            }}
          />
        )}
      </fieldset>
      {busy ? (
        <Loading label={t`Preparing image and checking destination`} slow={t`Preparing image and checking destination`}>
          <Skeleton height={72} />
        </Loading>
      ) : failure ? (
        <Callout title={failure.conflict ? t`Choose another tag` : t`Push couldn't be reviewed`} tone="warning">
          {failure.conflict ? (
            <>
              <Trans>This destination serves a different digest.</Trans>{" "}
              <Button
                variant="ghost"
                size="sm"
                onClick={() => {
                  tagField.current?.focus();
                  tagField.current?.select();
                }}
              >
                <Trans>Edit tag</Trans>
              </Button>
            </>
          ) : (
            failure.text
          )}
        </Callout>
      ) : null}
    </Sheet>
  );
}
