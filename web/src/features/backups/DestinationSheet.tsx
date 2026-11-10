// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type BackupDestinationInfo,
  BackupDestinationProblem,
  BackupService,
  ChangeService,
  type TestBackupDestinationResponse,
} from "@dockwell/api";
import { Advanced, Button, Callout, SelectField, Sheet, StatusBadge, Switch, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { CopyButton } from "../system/CopyCommand";

/** Storage a person picks, each with how its endpoint and region are made. */
export type Provider = "r2" | "b2" | "s3" | "wasabi" | "spaces" | "other";

interface ProviderShape {
  label: string;
  /** The regions to pick from; none for R2, which has one, and for other storage, which is typed. */
  regions: string[];
  /** The endpoint, from the region or R2's account ID. */
  endpoint: (place: string) => string;
  /** The region requests are signed for, from the one picked. */
  signs: (place: string) => string;
}

export const PROVIDERS: Record<Provider, ProviderShape> = {
  r2: {
    label: "Cloudflare R2",
    regions: [],
    endpoint: (account) => (account ? `https://${account}.r2.cloudflarestorage.com` : ""),
    signs: () => "auto",
  },
  b2: {
    label: "Backblaze B2",
    regions: ["us-west-001", "us-west-002", "us-west-004", "us-east-005", "eu-central-003"],
    endpoint: (region) => `https://s3.${region}.backblazeb2.com`,
    signs: (region) => region,
  },
  s3: {
    label: "Amazon S3",
    regions: [
      "us-east-1",
      "us-east-2",
      "us-west-1",
      "us-west-2",
      "ca-central-1",
      "eu-west-1",
      "eu-west-2",
      "eu-west-3",
      "eu-central-1",
      "eu-north-1",
      "ap-south-1",
      "ap-southeast-1",
      "ap-southeast-2",
      "ap-northeast-1",
      "sa-east-1",
    ],
    endpoint: (region) => `https://s3.${region}.amazonaws.com`,
    signs: (region) => region,
  },
  wasabi: {
    label: "Wasabi",
    regions: [
      "us-east-1",
      "us-east-2",
      "us-central-1",
      "us-west-1",
      "ca-central-1",
      "eu-central-1",
      "eu-central-2",
      "eu-west-1",
      "eu-west-2",
      "ap-northeast-1",
      "ap-southeast-1",
    ],
    endpoint: (region) => `https://s3.${region}.wasabisys.com`,
    signs: (region) => region,
  },
  spaces: {
    label: "DigitalOcean Spaces",
    regions: ["nyc3", "sfo3", "ams3", "fra1", "lon1", "sgp1", "syd1", "blr1", "tor1"],
    endpoint: (region) => `https://${region}.digitaloceanspaces.com`,
    // Spaces signs for us-east-1 whatever the region, as its S3 API asks.
    signs: () => "us-east-1",
  },
  other: { label: "", regions: [], endpoint: (typed) => typed.trim(), signs: () => "" },
};

const ORDER: Provider[] = ["r2", "b2", "s3", "wasabi", "spaces", "other"];

/** A passphrase no one chose: 32 random bytes, as letters and digits a person can copy. */
function makePassphrase(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(32));
  return btoa(String.fromCharCode(...bytes)).replace(/[+/=]/g, "");
}

/** A destination's name, not yet taken: the provider's, then numbered. */
function freeName(base: string, taken: readonly BackupDestinationInfo[]): string {
  const names = new Set(taken.map((d) => d.name));
  if (!names.has(base)) return base;
  for (let i = 2; ; i++) if (!names.has(`${base}-${i}`)) return `${base}-${i}`;
}

/**
 * Adds storage backups go to, as a plan. A person picks the provider, which makes
 * the endpoint from a region or an account ID; with the keys in, Find buckets lists
 * the buckets to pick from, and Test writes and deletes a file in the one picked,
 * all before anything is planned. The name, a folder in the bucket and a passphrase
 * of one's own are under Advanced. What signs in goes into the plan sealed.
 */
export function DestinationSheet({
  onClose,
  existing = [],
}: {
  onClose: () => void;
  /** The destinations there are, so a new one's name isn't taken. */
  existing?: readonly BackupDestinationInfo[];
}) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const client = useMemo(() => createClient(BackupService, transport), [transport]);
  const may = useMay();
  const mayPlan = may(ChangeService.method.proposeChange, { install: true });
  const mayTest = may(BackupService.method.testBackupDestination);
  const [provider, setProvider] = useState<Provider>("r2");
  const [place, setPlace] = useState("");
  const [typedRegion, setTypedRegion] = useState("");
  const [accessKey, setAccessKey] = useState("");
  const [secretKey, setSecretKey] = useState("");
  const [bucket, setBucket] = useState("");
  const [typing, setTyping] = useState(false);
  const [typedName, setTypedName] = useState<string>();
  const [prefix, setPrefix] = useState("");
  const [made] = useState(makePassphrase);
  const [own, setOwn] = useState(false);
  const [typed, setTyped] = useState("");
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState<"list" | "test">();
  const [answer, setAnswer] = useState<{ said: TestBackupDestinationResponse; bucket: string }>();
  const [failure, setFailure] = useState<string>();

  const shape = PROVIDERS[provider];
  const region = shape.regions.length > 0 ? place || shape.regions[0] || "" : place;
  const endpoint = shape.endpoint(region);
  const signs = provider === "other" ? typedRegion.trim() : shape.signs(region);
  const name = typedName ?? freeName(provider === "other" ? "storage" : provider, existing);
  const passphrase = own ? typed : made;
  const signsIn = endpoint !== "" && accessKey !== "" && secretKey !== "";
  const ready = name !== "" && signsIn && bucket !== "" && (own ? typed.length >= 16 : saved);
  const listed = answer?.said.listed ? answer.said.buckets : undefined;

  const forget = () => setAnswer(undefined);
  const ask = async (what: "list" | "test") => {
    setBusy(what);
    setFailure(undefined);
    try {
      const said = await client.testBackupDestination({
        destination: { endpoint, region: signs, bucket: what === "test" ? bucket : "", prefix, accessKey, secretKey },
      });
      setAnswer({ said, bucket: what === "test" ? bucket : "" });
      // The first bucket the keys see, when none was picked yet.
      if (what === "list" && !bucket && said.buckets[0]) setBucket(said.buckets[0]);
    } catch (err) {
      setFailure(sentence(err));
    } finally {
      setBusy(undefined);
    }
  };

  const review = () =>
    sheet.open({
      kind: "propose",
      summary: t`Send backups to ${name}`,
      edits: [
        {
          resource: { kind: "backup.destination", name },
          spec: {
            kind: {
              case: "backupDestination",
              value: { endpoint, bucket, region: signs, prefix, accessKey, secretKey, passphrase },
            },
          },
        },
      ],
      onClose: (change) => {
        if (change) onClose();
      },
    });

  const problem = answer?.said.problem ?? BackupDestinationProblem.UNSPECIFIED;
  const problemWords: Partial<Record<BackupDestinationProblem, string>> = {
    [BackupDestinationProblem.UNREACHABLE]: t`Nothing answered at ${endpoint}`,
    [BackupDestinationProblem.KEYS_REFUSED]: t`The storage refused these keys`,
    [BackupDestinationProblem.NO_BUCKET]: t`There's no bucket ${bucket} there`,
    [BackupDestinationProblem.CANT_WRITE]: t`These keys can't write to ${bucket}`,
  };
  const folder = prefix.trim().replace(/^\/+|\/+$/g, "");
  const summary = [
    t`Named ${name}`,
    folder ? t`in ${folder}/` : t`at the bucket's top`,
    own ? t`your passphrase` : t`a passphrase made for you`,
  ].join(" · ");

  return (
    <Sheet
      open
      onClose={onClose}
      title={t`Add a destination`}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" disabled={!ready} onClick={review} {...mayPlan.off}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="backup-sheet">
        <SelectField
          label={t`Storage`}
          value={provider}
          options={ORDER.map((p) => ({
            value: p,
            label: p === "other" ? t`MinIO or other S3 storage` : PROVIDERS[p].label,
          }))}
          onChange={(event) => {
            setProvider(event.target.value as Provider);
            setPlace("");
            forget();
          }}
        />
        {provider === "r2" ? (
          <TextField
            label={t`Account ID`}
            mono
            autoComplete="off"
            spellCheck={false}
            value={place}
            hint={t`On the R2 page of your Cloudflare dashboard.`}
            onChange={(event) => {
              setPlace(event.target.value.trim());
              forget();
            }}
          />
        ) : provider === "other" ? (
          <div className="backup-sheet-row">
            <TextField
              label={t`Endpoint`}
              mono
              autoComplete="off"
              spellCheck={false}
              value={place}
              placeholder="https://storage.example.com"
              onChange={(event) => {
                setPlace(event.target.value);
                forget();
              }}
            />
            <TextField
              label={t`Region`}
              mono
              optional
              value={typedRegion}
              placeholder="auto"
              onChange={(event) => {
                setTypedRegion(event.target.value);
                forget();
              }}
            />
          </div>
        ) : (
          <SelectField
            label={t`Region`}
            value={region}
            options={shape.regions.map((r) => ({ value: r, label: r }))}
            onChange={(event) => {
              setPlace(event.target.value);
              forget();
            }}
          />
        )}
        <div className="backup-sheet-row">
          <TextField
            label={t`Access key`}
            mono
            autoComplete="off"
            spellCheck={false}
            value={accessKey}
            onChange={(event) => {
              setAccessKey(event.target.value.trim());
              forget();
            }}
          />
          <TextField
            label={t`Secret key`}
            mono
            type="password"
            autoComplete="off"
            value={secretKey}
            onChange={(event) => {
              setSecretKey(event.target.value.trim());
              forget();
            }}
          />
        </div>
        {listed && listed.length > 0 && !typing ? (
          <div className="backup-sheet-row is-end">
            <SelectField
              label={t`Bucket`}
              value={bucket}
              options={listed.map((b) => ({ value: b, label: b }))}
              onChange={(event) => {
                setBucket(event.target.value);
                setAnswer((was) => (was ? { ...was, bucket: "" } : was));
              }}
            />
            <Button variant="ghost" size="sm" onClick={() => setTyping(true)}>
              <Trans>Type a name</Trans>
            </Button>
          </div>
        ) : (
          <div className="backup-sheet-row is-end">
            <TextField
              label={t`Bucket`}
              mono
              autoComplete="off"
              spellCheck={false}
              value={bucket}
              onChange={(event) => {
                setBucket(event.target.value.trim());
                setAnswer((was) => (was ? { ...was, bucket: "" } : was));
              }}
            />
            <Button
              variant="secondary"
              size="sm"
              disabled={!signsIn}
              loading={busy === "list"}
              onClick={() => {
                setTyping(false);
                void ask("list");
              }}
              {...mayTest.off}
            >
              <Trans>Find buckets</Trans>
            </Button>
          </div>
        )}
        <div className="backup-sheet-test" aria-live="polite">
          <Button
            size="sm"
            icon="check"
            disabled={!signsIn || bucket === ""}
            loading={busy === "test"}
            onClick={() => void ask("test")}
            {...mayTest.off}
          >
            <Trans>Test</Trans>
          </Button>
          {answer?.said.wrote && answer.bucket === bucket ? (
            <StatusBadge status="healthy" label={t`Wrote and deleted a test file in ${bucket}`} labelIsState />
          ) : answer && problem !== BackupDestinationProblem.UNSPECIFIED ? (
            <StatusBadge status="down" label={problemWords[problem] ?? ""} labelIsState />
          ) : answer?.said.listed && answer.bucket === "" ? (
            <StatusBadge
              status="healthy"
              label={
                answer.said.buckets.length === 0
                  ? t`Signed in · these keys list no bucket`
                  : plural(answer.said.buckets.length, { one: "Signed in · # bucket", other: "Signed in · # buckets" })
              }
              labelIsState
            />
          ) : null}
        </div>
        {answer?.said.said && problem !== BackupDestinationProblem.UNSPECIFIED ? (
          <p className="backup-sheet-said">{answer.said.said}</p>
        ) : null}
        {failure ? (
          <Callout tone="critical" title={t`No test`}>
            {failure}
          </Callout>
        ) : null}

        {own ? null : (
          <Callout tone="warning" title={t`Keep this passphrase somewhere else`}>
            <p className="backup-sheet-line">{t`Nothing else opens these backups, and Dockwell shows it only now.`}</p>
            <code className="backup-passphrase">
              {Array.from(made.matchAll(/.{1,6}/g), (group) => (
                <span key={group.index}>{group[0]}</span>
              ))}
            </code>
            <CopyButton
              text={made}
              label={t`Copy the passphrase`}
              copiedLabel={t`Passphrase copied`}
              variant="secondary"
            />
            <label className="backup-saved">
              <input type="checkbox" checked={saved} onChange={(e) => setSaved(e.target.checked)} />
              <Trans>I've kept a copy of the passphrase</Trans>
            </label>
          </Callout>
        )}

        <Advanced summary={summary}>
          <TextField
            label={t`Name`}
            mono
            value={name}
            hint={t`What schedules and the CLI call it.`}
            onChange={(event) => setTypedName(event.target.value)}
          />
          <TextField
            label={t`Folder in the bucket`}
            mono
            optional
            value={prefix}
            placeholder="dockwell"
            onChange={(event) => {
              setPrefix(event.target.value);
              forget();
            }}
          />
          <Switch
            checked={own}
            onChange={setOwn}
            label={t`Use a passphrase I already have`}
            description={t`For storage an earlier install sealed with it.`}
          />
          {own ? (
            <TextField
              label={t`Passphrase`}
              mono
              type="password"
              autoComplete="off"
              value={typed}
              hint={t`At least 16 characters.`}
              onChange={(event) => setTyped(event.target.value)}
            />
          ) : null}
        </Advanced>
      </div>
    </Sheet>
  );
}
