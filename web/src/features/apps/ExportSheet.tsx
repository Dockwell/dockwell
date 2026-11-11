// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { ComposeService } from "@dockwell/api";
import { Button, Callout, cx, PHONE_QUERY, SegmentedControl, Sheet, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useEffect, useRef, useState } from "react";

import { sentence } from "../../lib/errors";

/** Saves what's made here as a file the browser downloads. */
function save(blob: Blob, name: string) {
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.click();
  URL.revokeObjectURL(url);
}

/**
 * The variables an export left out, by name, as an .env beside the file sets
 * them: "web.API_KEY" is API_KEY. A volume's driver options aren't one.
 */
export function envLeftOut(omitted: readonly string[]): string[] {
  const names = omitted.filter((o) => !o.startsWith("volumes.")).map((o) => o.slice(o.indexOf(".") + 1));
  return [...new Set(names)].sort();
}

/**
 * A service or an environment as a Compose file, to read, copy or save (PRD
 * principle 6). It never holds a secret: what it leaves out, it lists, and
 * saves as an .env to fill in.
 */
export function ExportSheet({
  environment,
  service,
  onClose,
}: {
  /** The environment, by ID. */
  environment: string;
  /** One service, by name, which the sheet can widen to its environment. */
  service?: string | undefined;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const [whole, setWhole] = useState(service === undefined);
  // Each time from the control plane: a file from before the last deploy would
  // be wrong, and nothing else reads it again when a change applies.
  const query = useQuery(
    ComposeService.method.exportCompose,
    { environment, service: whole ? "" : (service ?? "") },
    { staleTime: 0, gcTime: 0 },
  );
  const file = query.data?.compose ?? "";
  const omitted = query.data?.omitted ?? [];
  const [copied, setCopied] = useState(false);
  const code = useRef<HTMLPreElement>(null);

  useEffect(() => {
    if (!copied) return;
    const timer = setTimeout(() => setCopied(false), 2000);
    return () => clearTimeout(timer);
  }, [copied]);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(file);
      setCopied(true);
    } catch {
      // Clipboard access was refused, so select it for a person to copy.
      if (code.current) window.getSelection()?.selectAllChildren(code.current);
    }
  };

  const download = () =>
    save(
      new Blob([file], { type: "application/yaml" }),
      whole || !service ? "compose.yaml" : `${service}.compose.yaml`,
    );
  const left = envLeftOut(omitted);
  const saveEnv = () => save(new Blob([left.map((name) => `${name}=\n`).join("")], { type: "text/plain" }), ".env");

  return (
    <Sheet
      open
      title={t`Export as Compose`}
      subtitle={
        <Trans>
          A file <code>docker compose</code> runs without Dockwell, with no secrets in it
        </Trans>
      }
      onClose={onClose}
      className={cx("app-sheet", phone && "is-phone")}
      footer={
        <>
          <Button size={phone ? "lg" : "md"} icon="download" disabled={!file} onClick={download}>
            <Trans>Save</Trans>
          </Button>
          <Button
            variant="primary"
            size={phone ? "lg" : "md"}
            icon={copied ? "check" : "copy"}
            disabled={!file}
            onClick={() => void copy()}
          >
            {copied ? <Trans>Copied</Trans> : <Trans>Copy</Trans>}
          </Button>
        </>
      }
    >
      <div className="app-form">
        {service ? (
          <div>
            <SegmentedControl
              label={t`What to export`}
              options={[
                { value: "service", label: t`This app` },
                { value: "environment", label: t`Its whole project` },
              ]}
              value={whole ? "environment" : "service"}
              onChange={(value) => setWhole(value === "environment")}
            />
          </div>
        ) : null}
        {query.error ? (
          <Callout tone="critical" title={t`Can't export this yet`}>
            {sentence(query.error)}
          </Callout>
        ) : null}
        {omitted.length > 0 ? (
          <Callout
            tone="info"
            title={t`Left out as they may hold secrets`}
            actions={
              left.length > 0 ? (
                <Button size="sm" icon="download" onClick={saveEnv}>
                  <Trans>
                    Save an <code>.env</code> to fill in
                  </Trans>
                </Button>
              ) : undefined
            }
          >
            <ul className="export-omitted">
              {omitted.map((name) => (
                <li key={name}>
                  <code>{name}</code>
                </li>
              ))}
            </ul>
          </Callout>
        ) : null}
        <section
          className="export-file"
          aria-label={t`Compose file`}
          aria-busy={query.isPending}
          // biome-ignore lint/a11y/noNoninteractiveTabindex: what scrolls has to be reachable by keyboard
          tabIndex={0}
        >
          <pre ref={code}>{file}</pre>
        </section>
        <span className="dw-sr" aria-live="polite">
          {copied ? t`Copied the Compose file` : ""}
        </span>
      </div>
    </Sheet>
  );
}
