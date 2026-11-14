// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ChangeService, ChangeState, type MoveBlock, type ServiceInfo, ServiceService } from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Icon,
  Panel,
  PHONE_QUERY,
  SelectField,
  Sheet,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { useMay } from "../auth/may";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { environmentOf, nameOf } from "./app";
import { usePlaces } from "./useServices";

/** The choice that makes a project for the app, named for it. */
const NEW = "new";

/**
 * Moves an app to another project or environment, data, domains and all, in one plan
 * with undo (ADR 0084, #572). One choice, where it goes: a project of its own by
 * default for an admin, who alone makes one, and otherwise the first environment the
 * person may move it to; what holds it back shows as a line each, with the service
 * to move too beside it, as a button that does it.
 */
export function MoveSheet({ service, onClose }: { service: ServiceInfo; onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(ServiceService, transport), [transport]);
  const places = usePlaces();
  const may = useMay();
  const name = nameOf(service);
  const here = environmentOf(service);
  // A new project is an admin's to make; anyone else picks among where they may move it.
  const makes = may(ChangeService.method.proposeChange, { install: true }).allowed;
  const elsewhere = places.environments.filter(
    (e) => e.id !== here && may(ServiceService.method.moveService, { environment: e.id }).allowed,
  );
  const taken = (project: string) => places.environments.some((e) => e.project === project);
  const [picked, setWhere] = useState<string>();
  const where = picked ?? (makes ? NEW : (elsewhere[0]?.id ?? ""));
  const [project, setProject] = useState(name);
  const [also, setAlso] = useState<string[]>([]);
  const [blocks, setBlocks] = useState<MoveBlock[]>([]);
  const [busy, setBusy] = useState(false);
  const [fieldError, setFieldError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [plan, setPlan] = useState<Opening>();
  const field = useRef<HTMLSelectElement>(null);
  const others = also.join(", ");
  const clash = where === NEW && taken(project) ? t`There's a project named ${project} already` : undefined;

  const ask = async (more: string[], event?: FormEvent) => {
    event?.preventDefault();
    if (busy || clash) return;
    setBusy(true);
    setFieldError(undefined);
    setFailure(undefined);
    const to = elsewhere.find((e) => e.id === where);
    try {
      const res = await client.moveService({
        id: service.id,
        also: more,
        ...(where === NEW ? { newProject: project } : { toProject: to?.scope ?? "", toEnvironment: to?.id ?? "" }),
      });
      setBlocks(res.blocks);
      if (res.change) {
        const change = res.change;
        setPlan({
          kind: "plan",
          make: () => Promise.resolve({ change }),
          retryLabel: t`Choose again`,
          onDone: onClose,
          onClose: (after) => {
            if (!after || after.state === ChangeState.PLANNED || after.state === ChangeState.FAILED) setPlan(undefined);
            else onClose();
          },
        });
      }
    } catch (err) {
      if (reasonOf(err) === "invalid") setFieldError(sentence(err));
      else setFailure(sentence(err));
    }
    setBusy(false);
  };
  const moveToo = (svc: string) => {
    const more = [...also, svc];
    setAlso(more);
    void ask(more);
  };

  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      initialFocusRef={field}
      title={t`Move ${name}`}
      subtitle={t`With its data, domains and what only it uses`}
      onClose={onClose}
      className={cx("app-sheet", phone && "is-phone")}
      footer={
        <>
          <span className="dw-sheet-note">
            <Icon name="commit" size={14} />
            <Trans>Nothing moves until you apply the plan</Trans>
          </span>
          <Button variant="ghost" size={phone ? "lg" : "md"} onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            size={phone ? "lg" : "md"}
            loading={busy}
            disabled={Boolean(clash) || (where === NEW && !project) || where === ""}
            kbd={phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵"}
            onClick={() => void ask(also)}
          >
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <form
        className="app-form"
        onSubmit={(event) => void ask(also, event)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) void ask(also, event);
        }}
      >
        {failure ? <Callout tone="critical">{failure}</Callout> : null}
        {!makes && elsewhere.length === 0 && !places.isPending ? (
          <Callout tone="info" title={t`Nowhere else you hold`}>
            <Trans>Ask an admin for a role in the project it goes to.</Trans>
          </Callout>
        ) : null}
        <SelectField
          ref={field}
          label={t`Where it goes`}
          value={where}
          error={where === NEW ? undefined : fieldError}
          options={[
            ...(makes ? [{ value: NEW, label: t`A new project` }] : []),
            ...elsewhere.map((e) => ({ value: e.id, label: e.label || e.project })),
          ]}
          onChange={(event) => {
            setWhere(event.target.value);
            setBlocks([]);
            setFieldError(undefined);
          }}
        />
        {where === NEW ? (
          <TextField
            label={t`Project name`}
            value={project}
            error={clash ?? fieldError}
            onChange={(event) => {
              setProject(event.target.value.trim());
              setFieldError(undefined);
            }}
          />
        ) : null}
        {also.length > 0 ? (
          <p className="move-also">
            <Trans>Moves {others} with it</Trans>
          </p>
        ) : null}
        {blocks.length > 0 ? (
          <ul className="move-blocks" aria-label={t`What holds it back`}>
            {blocks.map((b) => {
              const service = b.moveToo;
              return (
                <li key={`${b.resource?.kind}/${b.resource?.name}/${b.why}`} className="move-block">
                  <span className="move-block-why">{b.why}</span>
                  {service ? (
                    <Button size="sm" variant="secondary" loading={busy} onClick={() => moveToo(service)}>
                      {t`Move ${service} too`}
                    </Button>
                  ) : null}
                </li>
              );
            })}
          </ul>
        ) : null}
        <button type="submit" hidden />
      </form>
    </Sheet>
  );
}

/** Moving the app, as a row of its settings: where it goes is the sheet's one choice. */
export function MoveApp({ service, title }: { service: ServiceInfo; title: string }) {
  const { t } = useLingui();
  const may = useMay();
  const [open, setOpen] = useState(false);
  const name = nameOf(service);
  const allowed = may(ServiceService.method.moveService, { environment: environmentOf(service) });
  return (
    <Panel title={title} className="app-move">
      <div className="app-danger-row">
        <div className="app-danger-text">
          <span className="app-danger-title">{t`Move ${name}`}</span>
          <span className="app-danger-what">
            <Trans>To another project, with its data and domains. You review a plan first.</Trans>
          </span>
        </div>
        <Button
          icon="arrow-right"
          onClick={() => setOpen(true)}
          off={allowed.allowed ? undefined : allowed.reason || undefined}
        >
          {t`Move ${name}`}
        </Button>
      </div>
      {open ? <MoveSheet service={service} onClose={() => setOpen(false)} /> : null}
    </Panel>
  );
}
