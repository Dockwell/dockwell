// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeState, type ServiceInfo, ServiceService } from "@dockwell/api";
import { Button, Callout, cx, Icon, PHONE_QUERY, SelectField, Sheet, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { useRuntimes } from "../websites/website";
import { useApp } from "./AppContext";
import { failedAtField, isMoving, nameOf } from "./app";
import { languageName, newerBuild, runtimeOf, versionsOf } from "./runtime";

/**
 * Runs an app from a folder on another version of its language, or on the newest
 * build of its own: one choice, then the plan, which replaces its container. The rest
 * of the app stays as it is, so nothing here reads a secret back (ADR 0058).
 */
export function RuntimeSheet({ service, onClose }: { service: ServiceInfo; onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(ServiceService, transport), [transport]);
  const runtimes = useRuntimes().data;
  const name = nameOf(service);
  const rt = runtimeOf(service);
  const language = rt?.language ?? "";
  const current = rt?.version ?? "";
  const versions = versionsOf(language, runtimes);
  const newer = newerBuild(service, runtimes);
  const [version, setVersion] = useState(current);
  const [busy, setBusy] = useState(false);
  const [fieldError, setFieldError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [plan, setPlan] = useState<Opening>();
  const field = useRef<HTMLSelectElement>(null);
  const { state } = useApp();
  const moving = isMoving(state);
  const word = languageName(language, runtimes);
  const chosen = versions.find((v) => v.version === version);
  // The version it has, on the newest build: nothing to plan.
  const same = version === current && !newer;

  const fix = (reason: string) =>
    reason === "exits" || reason === "command"
      ? t`Check its logs for what the install or the start said, then try again.`
      : undefined;

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy || moving || same) return;
    setBusy(true);
    setFieldError(undefined);
    setFailure(undefined);
    let change: Change | undefined;
    try {
      const release = chosen?.release ?? version;
      change = (
        await client.changeServiceRuntime({
          id: service.id,
          version,
          summary:
            version === current
              ? t`Update ${name} to ${word} ${release}`
              : t`Run ${name} on ${word} ${version}, from ${current}`,
        })
      ).change;
    } catch (err) {
      if (reasonOf(err) === "invalid") setFieldError(sentence(err));
      else setFailure(sentence(err));
      setBusy(false);
      return;
    }
    setBusy(false);
    setPlan({
      kind: "plan",
      make: () => Promise.resolve({ change }),
      retryLabel: t`Choose again`,
      onDone: onClose,
      fix,
      onClose: (after) => {
        if (after?.state === ChangeState.FAILED) {
          setFieldError(failedAtField(after, name, fix));
          setPlan(undefined);
        } else if (!after || after.state === ChangeState.PLANNED) setPlan(undefined);
        else onClose();
      },
    });
  };

  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      initialFocusRef={field}
      title={t`Change ${name}'s version`}
      subtitle={t`It runs what's in its folder on the new version, with the rest as it is`}
      onClose={onClose}
      className={cx("app-sheet", phone && "is-phone")}
      footer={
        <>
          <span className="dw-sheet-note">
            <Icon name="commit" size={14} />
            {same ? (
              <Trans>It runs this version's newest build already</Trans>
            ) : (
              <Trans>Nothing runs until you apply the plan</Trans>
            )}
          </span>
          <Button variant="ghost" size={phone ? "lg" : "md"} onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            size={phone ? "lg" : "md"}
            loading={busy}
            disabled={moving || same}
            kbd={phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵"}
            onClick={() => void submit()}
          >
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <form
        className="app-form"
        onSubmit={(event) => void submit(event)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) void submit(event);
        }}
      >
        {failure ? <Callout tone="critical">{failure}</Callout> : null}
        <SelectField
          ref={field}
          label={t`${word} version`}
          value={version}
          error={fieldError}
          options={versions.map((v) => ({
            value: v.version,
            label:
              // On an older build, the one it runs isn't the release listed.
              v.version !== current
                ? `${v.version} · ${v.release}`
                : newer
                  ? t`${v.version}, the one it runs`
                  : t`${v.version} · ${v.release}, the one it runs`,
          }))}
          hint={
            newer && version === current
              ? t`It runs an older build of ${current}. The plan updates it to ${newer.release}.`
              : t`Each start installs again, so check the app's lockfile works with the new version.`
          }
          onChange={(event) => {
            setVersion(event.target.value);
            setFieldError(undefined);
          }}
        />
        <button type="submit" hidden />
      </form>
    </Sheet>
  );
}
