// SPDX-License-Identifier: AGPL-3.0-only

import { ChangeState } from "@dockwell/api";
import { Button, Sheet, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { type FormEvent, useEffect, useRef, useState } from "react";

import { useChangeSheet } from "../changes/ChangeSheet";
import type { ProjectView } from "./useProjects";

/** A project's name as URLs and the CLI hold it (as the command bar checks it). */
const NAME = /^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$/;

/**
 * The URL name for what someone typed: lowercase, dashes for what isn't a letter
 * or a digit, starting with a letter. What they typed stays as its display name
 * where the two differ, so nobody has to write a slug by hand.
 */
export function projectNameOf(typed: string): string {
  const slug = typed
    .trim()
    .toLowerCase()
    .normalize("NFKD")
    .replace(/\p{M}/gu, "")
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  if (slug) return (/^[a-z]/.test(slug) ? slug : `project-${slug}`).slice(0, 63).replace(/-+$/, "");

  const letters = typed.normalize("NFKC").match(/[\p{L}\p{N}]/gu);
  if (!letters) return "";
  const codes = letters.map((letter) => letter.codePointAt(0)?.toString(16));
  return `project-${codes.join("-")}`.slice(0, 63).replace(/-+$/, "");
}

/**
 * A new, empty project, by name only: its production environment comes with it in
 * the same plan (ADR 0074), and applying the plan lands on the project's page,
 * where its first app goes.
 */
export function NewProjectSheet({
  taken,
  onClose,
}: {
  /** The names projects already hold, checked as you type. */
  taken: ReadonlySet<string>;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const navigate = useNavigate();
  const [typed, setTyped] = useState("");
  const [editingName, setEditingName] = useState(false);
  const [override, setOverride] = useState<string>();
  const field = useRef<HTMLInputElement>(null);
  const urlName = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (editingName) urlName.current?.focus();
  }, [editingName]);
  const shown = typed.trim();
  const derived = projectNameOf(typed);
  const name = (override ?? derived).trim();
  const clash = name !== "" && taken.has(name) ? t`There's a project named ${name} already` : undefined;
  const unusable = shown !== "" && derived === "" ? t`Use letters or digits in its name` : undefined;
  const nameError =
    (override !== undefined || name !== "") && !NAME.test(name)
      ? t`Use lowercase letters, digits and dashes, starting with a letter, such as shop.`
      : clash;
  const valid = shown !== "" && name !== "" && !unusable && !nameError;
  const create = (event: FormEvent) => {
    event.preventDefault();
    if (!valid) return;
    onClose();
    sheet.open({
      kind: "propose",
      summary: t`Create the project ${name}`,
      edits: [
        {
          resource: { kind: "project", name },
          spec: { kind: { case: "project", value: shown !== name ? { displayName: shown } : {} } },
        },
      ],
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) {
          void navigate({ to: "/projects/$projectId", params: { projectId: name } });
        }
      },
    });
  };
  return (
    <Sheet
      open
      title={t`New project`}
      onClose={onClose}
      initialFocusRef={field}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button type="submit" form="new-project" variant="primary" disabled={!valid}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <form id="new-project" onSubmit={create}>
        <TextField
          ref={field}
          label={t`Name`}
          placeholder={t`shop`}
          value={typed}
          maxLength={63}
          error={unusable ?? (editingName ? undefined : nameError)}
          hint={
            !editingName && !nameError && derived !== "" && derived !== shown
              ? t`Named ${name} in URLs and the CLI`
              : undefined
          }
          onChange={(event) => setTyped(event.target.value)}
        />
        {editingName ? (
          <TextField
            ref={urlName}
            label={t`Name in URLs`}
            value={override ?? derived}
            maxLength={63}
            error={nameError}
            onChange={(event) => setOverride(event.target.value)}
          />
        ) : derived !== "" && (derived !== shown || clash) ? (
          <Button type="button" variant="ghost" size="sm" onClick={() => setEditingName(true)}>
            <Trans>Change name in URLs</Trans>
          </Button>
        ) : null}
      </form>
    </Sheet>
  );
}

/** A project's display name, changed from its row as the same plan its page's Settings opens. */
export function RenameProjectSheet({ view, onClose }: { view: ProjectView; onClose: () => void }) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const [name, setName] = useState(view.project.displayName);
  const field = useRef<HTMLInputElement>(null);
  const shown = name.trim();
  const changed = shown !== view.project.displayName;
  const rename = (event: FormEvent) => {
    event.preventDefault();
    if (!changed) return;
    onClose();
    sheet.open({
      kind: "propose",
      summary: shown ? t`Show ${view.name} as ${shown}` : t`Show ${view.name} by its name`,
      // The engine keeps what it was installed from and what it serves (ADR 0074).
      edits: [
        {
          resource: { kind: "project", name: view.name },
          spec: { kind: { case: "project", value: { displayName: shown } } },
        },
      ],
    });
  };
  return (
    <Sheet
      open
      title={t`Rename ${view.label}`}
      onClose={onClose}
      initialFocusRef={field}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button type="submit" form="rename-project" variant="primary" disabled={!changed}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <form id="rename-project" onSubmit={rename}>
        <TextField
          ref={field}
          label={t`Display name`}
          placeholder={view.name}
          value={name}
          maxLength={63}
          hint={t`URLs and the CLI keep its name, ${view.name}.`}
          onChange={(event) => setName(event.target.value)}
        />
      </form>
    </Sheet>
  );
}
