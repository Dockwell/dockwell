// SPDX-License-Identifier: AGPL-3.0-only

import { type ReactNode, useEffect, useState } from "react";

import { cx } from "../../lib/cx";
import type { Status } from "../../lib/status";
import { useUIStrings } from "../../lib/strings";
import { Button } from "../Button/Button";
import { StatusBadge } from "../StatusBadge/StatusBadge";
import "./DnsRecord.css";

export interface DnsRecordProps {
  /** The record's type, such as "A", "AAAA" or "CNAME". */
  type: string;
  /** The name it's for, such as "blog.example.com". */
  name: string;
  /** What it points at: an address, or a name for a CNAME. */
  value: string;
  /**
   * What a check of DNS found, as a state and a short word, such as "Points here" or
   * "Not set yet". Left out, nothing has been checked, and nothing says so.
   */
  state?: { status: Status; word: string } | undefined;
  /** Checks again now. Left out, there is no button, as when the record is already there. */
  onCheck?: (() => void) | undefined;
  /** True while a check is on its way. */
  checking?: boolean;
  /** One short line under the state, when something about this record needs saying. */
  note?: ReactNode;
  className?: string;
}

/**
 * One DNS record a person makes at their DNS provider: its type, name and value, the
 * value ready to copy, and what a live check of DNS says of it, with a way to check
 * again. It says what to make and whether it's there, not how DNS works.
 */
export function DnsRecord({ type, name, value, state, onCheck, checking = false, note, className }: DnsRecordProps) {
  const strings = useUIStrings().dnsRecord;
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(timer);
  }, [copied]);
  const copy = () => {
    void (async () => {
      try {
        await navigator.clipboard.writeText(value);
        setCopied(true);
      } catch {
        // Refused, or no clipboard, as on plain HTTP: the value stays on screen to select.
      }
    })();
  };
  return (
    <div className={cx("dw-dns", className)}>
      <div className="dw-dns-row">
        <code className="dw-dns-type">{type}</code>
        <code className="dw-dns-name">{name}</code>
        <code className="dw-dns-value">{value}</code>
        <Button
          variant="ghost"
          size="sm"
          icon={copied ? "check" : "copy"}
          aria-label={copied ? strings.copied(value) : strings.copy(value)}
          onClick={copy}
        />
        <span className="dw-sr" aria-live="polite">
          {copied ? strings.copied(value) : ""}
        </span>
      </div>
      {state || onCheck ? (
        <div className="dw-dns-state">
          {/* A status, so the record arriving is said to someone who can't see it change. */}
          <span role="status">
            {state ? <StatusBadge status={state.status} label={state.word} labelIsState /> : null}
          </span>
          {onCheck ? (
            <Button variant="ghost" size="sm" icon="refresh" loading={checking} onClick={onCheck}>
              {strings.check}
            </Button>
          ) : null}
        </div>
      ) : null}
      {note ? <p className="dw-dns-note">{note}</p> : null}
    </div>
  );
}
