// SPDX-License-Identifier: AGPL-3.0-only

import type { Server } from "@dockwell/api";
import { useLingui } from "@lingui/react/macro";
import { type ReactNode, useMemo } from "react";

import { LabelChips } from "./LabelChips";
import { fleetLabels, isProblem, labelSuggestions, labelText, MAX_LABELS, parseLabel } from "./labels";

/**
 * A server's labels to edit, as chips, offering the labels the fleet uses: a key
 * typed again takes its new value, and a server takes at most MAX_LABELS.
 */
export function LabelsField({
  labels,
  onChange,
  fleet,
  label,
  hint,
  shownLabel,
  optional,
}: {
  labels: Readonly<Record<string, string>>;
  onChange: (next: Record<string, string>) => void;
  /** The servers whose labels it offers. */
  fleet: readonly Server[];
  label: string;
  hint?: ReactNode;
  shownLabel?: boolean;
  optional?: boolean;
}) {
  const { t } = useLingui();
  const used = useMemo(() => fleetLabels(fleet), [fleet]);
  const keys = Object.keys(labels).sort();
  return (
    <LabelChips
      label={label}
      chips={keys.map((key) => ({ text: labelText(key, labels[key] ?? ""), key, value: labels[key] || undefined }))}
      onAdd={(text) => {
        const read = parseLabel(text);
        if (isProblem(read)) return;
        onChange({ ...labels, [read.key]: read.value });
      }}
      onRemove={(i) => {
        const gone = keys[i];
        onChange(Object.fromEntries(Object.entries(labels).filter(([k]) => k !== gone)));
      }}
      check={(text) => {
        const read = parseLabel(text);
        if (isProblem(read)) return read;
        return !Object.hasOwn(labels, read.key) && keys.length >= MAX_LABELS ? { kind: "too-many" } : undefined;
      }}
      suggestions={labelSuggestions(used, labels)}
      placeholder={t`key:value`}
      hint={hint}
      shownLabel={shownLabel}
      optional={optional}
    />
  );
}
