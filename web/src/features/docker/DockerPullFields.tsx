// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { DockerObjectKind, DockerService, ImageAnswer, ImageService, type Server } from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  Loading,
  SelectField,
  Skeleton,
  SkeletonFacts,
  SkeletonRows,
  StatusBadge,
  Tag,
  TextField,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQuery as useLocalQuery } from "@tanstack/react-query";
import { type RefObject, useEffect, useState } from "react";

import { RouterButtonLink } from "../../lib/links";
import { splitTag } from "../apps/ImageField";
import { useMay } from "../auth/may";
import { registryOf, useRegistryLogins } from "../registries/registries";

export type ImageServer = Pick<Server, "id" | "name"> & { registry: string };

const HUB = "docker.io";
const OTHER = "other";
const REPOSITORY = /^[a-z0-9]+(?:(?:[._-]+|\/)[a-z0-9]+)*$/;

function namedHost(reference: string) {
  const slash = reference.indexOf("/");
  if (slash < 0) return undefined;
  const first = reference.slice(0, slash).toLowerCase();
  return first === "localhost" || first.includes(".") || first.includes(":") ? registryOf(reference) : undefined;
}

function imageName(reference: string, host = HUB) {
  const named = namedHost(reference);
  const tail = named ? reference.slice(reference.indexOf("/") + 1) : reference;
  const at = tail.indexOf("@");
  const name = at < 0 ? tail : tail.slice(0, at);
  const { repository, tag } = splitTag(name);
  const registry = named ?? host;
  const path = registry === HUB && !repository.includes("/") ? `library/${repository}` : repository;
  return {
    registry,
    repository,
    canonical: `${registry}/${path}`,
    suffix: at < 0 ? `:${tag || "latest"}` : tail.slice(at),
  };
}

function registryAddress(value: string) {
  const trimmed = value.trim().toLowerCase();
  const valid = /^(?:\[[a-f0-9:]+\]|[a-z0-9]+(?:[.-][a-z0-9]+)*)(?::\d+)?$/.test(trimmed);
  return valid ? (namedHost(`${trimmed}/example`) ?? "") : "";
}

export function useDockerPull(
  servers: readonly ImageServer[],
  enabled: boolean,
  comparing: boolean,
  field: RefObject<HTMLInputElement | null>,
  edited: () => void,
) {
  const { t } = useLingui();
  const transport = useTransport();
  const [serverId, setServerId] = useState(servers[0]?.id ?? "");
  const [reference, setReference] = useState("");
  const [selected, setSelected] = useState<string>();
  const [other, setOther] = useState("");
  const [browse, setBrowse] = useState(false);
  const [pages, setPages] = useState([""]);
  const [visible, setVisible] = useState(8);
  const defaultHost = servers.find((server) => server.id === serverId)?.registry || HUB;
  const key = selected ?? defaultHost;
  const logins = useRegistryLogins(undefined, enabled);
  const eligible = (logins.data?.logins ?? []).filter((login) => !login.projects.length);
  const login = eligible.find((item) => item.id === key);
  const host =
    key === OTHER ? registryAddress(other) : (login?.registry ?? (key === HUB || key === defaultHost ? key : ""));
  const image = imageName(reference.trim(), host);
  const fallback = eligible.filter((item) => item.registry === host);
  const fallbackName = !login && fallback.length === 1 ? fallback[0]?.name : undefined;
  const may = useMay();

  const repository = host && REPOSITORY.test(image.repository) ? image.canonical : "";
  const [settled, setSettled] = useState("");
  useEffect(() => {
    const timer = setTimeout(() => setSettled(repository), 350);
    return () => clearTimeout(timer);
  }, [repository]);

  const localReady = enabled && !!serverId && !!repository && repository === settled;
  const ready = localReady && logins.isSuccess;
  const tags = useQuery(
    ImageService.method.listImageTags,
    { repository, server: serverId, registryLogin: login?.name },
    { enabled: ready && may(ImageService.method.listImageTags).allowed, retry: false },
  );
  const mayBrowse = may(ImageService.method.listRegistryRepositories).allowed;
  const repos = useQuery(
    ImageService.method.listRegistryRepositories,
    { registry: host, server: serverId, loginId: login?.id, pageToken: pages.at(-1) },
    { enabled: enabled && mayBrowse && browse && !!serverId && !!host && logins.isSuccess, retry: false },
  );

  const local = useLocalQuery({
    queryKey: ["docker-pull-local-tags", serverId, repository],
    enabled: localReady,
    retry: false,
    queryFn: async ({ signal }) => {
      const client = createClient(DockerService, transport);
      const references = new Set<string>();
      let pageToken = "";
      let complete = true;
      // Even a broad Engine search stays bounded. An incomplete read can prove presence only.
      for (let pageNumber = 0; pageNumber < 8; pageNumber++) {
        const page = await client.listDockerObjects(
          {
            serverIds: [serverId],
            kind: DockerObjectKind.IMAGE,
            filter: { search: image.repository.replace(/^library\//, "") },
            pageToken,
          },
          { signal },
        );
        complete &&= !page.unavailable.length;
        for (const row of page.objects) {
          if (row.detail.case !== "image") continue;
          complete &&= !row.detail.value.detailsTruncated;
          for (const alias of [...row.detail.value.tags, ...row.detail.value.digests]) {
            const parsed = imageName(alias);
            if (parsed.canonical === repository) references.add(parsed.suffix);
          }
        }
        pageToken = page.nextPageToken;
        if (!pageToken) break;
      }
      return { references, complete: complete && !pageToken };
    },
  });
  const installed = local.data?.references.has(image.suffix) ?? false;
  const valid =
    !!repository &&
    !!serverId &&
    !/[:/@]$/.test(reference.trim()) &&
    logins.isSuccess &&
    /^(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}|@sha256:[a-f0-9]{64})$/.test(image.suffix);
  const options = [
    { value: HUB, label: "Docker Hub · docker.io" },
    ...(defaultHost !== HUB ? [{ value: defaultHost, label: defaultHost }] : []),
    ...eligible.map(({ id, name, registry }) => ({ value: id, label: `${registry} · ${name}` })),
    { value: OTHER, label: t`Other registry…` },
  ];
  const resetBrowse = () => {
    setBrowse(false);
    setPages([""]);
  };
  const editReference = (text: string) => {
    edited();
    const named = namedHost(text.trim());
    if (named) {
      if (named !== host) {
        const matching = eligible.filter((item) => item.registry === named);
        setSelected(matching.length === 1 ? matching[0]?.id : named === HUB || named === defaultHost ? named : OTHER);
        setOther(named);
        resetBrowse();
      }
      text = text.trim().slice(text.trim().indexOf("/") + 1);
    }
    if (imageName(text, host).repository !== image.repository) setVisible(8);
    setReference(text);
  };

  return {
    serverId,
    reference: `${image.canonical}${image.suffix}`,
    loginId: login?.id,
    installed,
    valid,
    fields: (
      <>
        {servers.length > 1 ? (
          <SelectField
            label={t`Server`}
            value={serverId}
            options={servers.map(({ id, name }) => ({ value: id, label: name }))}
            onChange={(event) => {
              edited();
              setServerId(event.target.value);
              setSelected(undefined);
              resetBrowse();
            }}
          />
        ) : null}
        <div className="docker-registry-choice">
          <SelectField
            label={t`Registry`}
            value={key}
            options={options}
            onChange={(event) => {
              edited();
              setSelected(event.target.value);
              resetBrowse();
            }}
          />
          <div className="docker-registry-access">
            {logins.isPending ? (
              <Loading label={t`Loading registry logins`}>
                <SkeletonRows rows={1} lines={1} dense />
              </Loading>
            ) : logins.isError ? (
              <Button size="sm" onClick={() => void logins.refetch()}>
                <Trans>Retry registry logins</Trans>
              </Button>
            ) : login ? (
              <Tag icon="lock">{login.name}</Tag>
            ) : fallbackName ? (
              <Tag icon="lock" words>
                <Trans>{fallbackName} if needed</Trans>
              </Tag>
            ) : (
              <Tag icon="globe" words>
                <Trans>Public</Trans>
              </Tag>
            )}
            <RouterButtonLink to="/settings/integrations" variant="ghost" size="sm" icon="settings">
              <Trans>Manage logins</Trans>
            </RouterButtonLink>
          </div>
        </div>
        {key === OTHER ? (
          <TextField
            label={t`Registry address`}
            placeholder="registry.example.com"
            mono
            value={other}
            onChange={(event) => {
              edited();
              setOther(event.target.value);
              resetBrowse();
            }}
          />
        ) : null}
        <div className="docker-pull-reference">
          <TextField
            label={t`Image`}
            placeholder="nginx:latest"
            mono
            value={reference}
            ref={field}
            onChange={(event) => editReference(event.target.value)}
          />
          {mayBrowse ? (
            <Button
              variant="secondary"
              icon="search"
              disabled={!host || !logins.isSuccess}
              aria-expanded={browse}
              onClick={() => setBrowse(!browse)}
            >
              <Trans>Browse registry</Trans>
            </Button>
          ) : null}
        </div>
        {comparing ? (
          <Loading label={t`Comparing image digests`} className="docker-image-compare">
            <SkeletonFacts count={3} layout="rows" />
          </Loading>
        ) : browse ? (
          repos.isPending ? (
            <Loading label={t`Loading repositories`}>
              <SkeletonRows rows={3} lead="icon" lines={1} dense />
            </Loading>
          ) : repos.data?.enumerationSupported ? (
            <>
              <div className="docker-repository-choices">
                {repos.data.repositories.map(({ name }) => (
                  <Button
                    key={name}
                    className="docker-repository-choice"
                    icon="archive"
                    variant="ghost"
                    onClick={() => {
                      editReference(name);
                      setBrowse(false);
                      field.current?.focus();
                    }}
                  >
                    {name}
                  </Button>
                ))}
                {!repos.data.repositories.length ? (
                  <span className="docker-muted">
                    <Trans>No repositories on this page</Trans>
                  </span>
                ) : null}
              </div>
              {pages.length > 1 || repos.data.nextPageToken ? (
                <div className="docker-image-link-actions">
                  <Button disabled={pages.length === 1} onClick={() => setPages(pages.slice(0, -1))}>
                    <Trans>Previous</Trans>
                  </Button>
                  <Button
                    disabled={!repos.data.nextPageToken}
                    onClick={() => setPages([...pages, repos.data.nextPageToken])}
                  >
                    <Trans>Next</Trans>
                  </Button>
                </div>
              ) : null}
            </>
          ) : (
            <Callout
              title={t`Repository list unavailable`}
              tone="info"
              actions={
                <Button
                  size="sm"
                  onClick={() => {
                    setBrowse(false);
                    field.current?.focus();
                  }}
                >
                  <Trans>Enter image name</Trans>
                </Button>
              }
            />
          )
        ) : repository ? (
          <section className="docker-pull-tags" aria-label={t`Repository tags`}>
            <h3>
              <Trans>Tags</Trans>
            </h3>
            {(!ready && !logins.isError) || tags.isLoading ? (
              <Loading label={t`Loading image tags`}>
                <SkeletonRows rows={3} lead="icon" lines={2} dense />
              </Loading>
            ) : tags.data?.tags.length ? (
              <>
                <ul>
                  {tags.data.tags.slice(0, visible).map(({ name, digest }) => {
                    const onServer = local.data?.references.has(`:${name}`);
                    const active = image.suffix === `:${name}`;
                    return (
                      <li key={name}>
                        <Button
                          variant="ghost"
                          className="docker-pull-tag"
                          aria-pressed={active}
                          onClick={() => editReference(`${image.repository}:${name}`)}
                        >
                          <span className="docker-pull-tag-name">
                            <Icon name="archive" size={15} />
                            <code>{name}</code>
                          </span>
                          {onServer ? (
                            <StatusBadge status="healthy" label={<Trans>On this server</Trans>} labelIsState />
                          ) : local.isLoading ? (
                            <span role="img" aria-label={t`Checking local images`}>
                              <Skeleton shape="pill" width={74} inline />
                            </span>
                          ) : (
                            <span className="docker-muted">
                              {local.data?.complete ? (
                                <Trans>Not on this server</Trans>
                              ) : (
                                <Trans>Local status unavailable</Trans>
                              )}
                            </span>
                          )}
                          {digest ? (
                            <code className="docker-pull-tag-digest" title={digest}>
                              {digest}
                            </code>
                          ) : (
                            <span className="docker-pull-tag-digest">
                              <Trans>Digest unavailable</Trans>
                            </span>
                          )}
                        </Button>
                      </li>
                    );
                  })}
                </ul>
                {visible < tags.data.tags.length ? (
                  <Button size="sm" onClick={() => setVisible(visible + 8)}>
                    <Trans>Show more tags</Trans>
                  </Button>
                ) : null}
              </>
            ) : (
              <span className="docker-muted">
                {tags.data?.answer === ImageAnswer.FOUND ? <Trans>No tags</Trans> : <Trans>Tags unavailable</Trans>}
              </span>
            )}
          </section>
        ) : null}
      </>
    ),
  };
}
