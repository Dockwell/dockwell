// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeState, type ServiceInfo, ServiceService } from "@dockwell/api";
import { Button, Callout, cx, Icon, PHONE_QUERY, Sheet, TextField, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { useChangeSheet } from "../changes/ChangeSheet";
import { imageOf, nameOf } from "./app";

/**
 * Deploys another image: one field, then the plan. Everything else about the
 * app stays, so nothing here has to read a secret back. Dockwell refuses a
 * bad image at the field, and an image it runs already, before there is a
 * plan to review. Git sources come with phase 2.
 */
export function DeploySheet({ service, onClose }: { service: ServiceInfo; onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(ServiceService, transport), [transport]);
  const name = nameOf(service);
  const current = imageOf(service);
  const [image, setImage] = useState(current);
  const [busy, setBusy] = useState(false);
  const [fieldError, setFieldError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [hidden, setHidden] = useState(false);
  const field = useRef<HTMLInputElement>(null);

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy) return;
    const wanted = image.trim();
    if (wanted === "") {
      setFieldError(t`Name the image to run, such as ${current}.`);
      return;
    }
    setBusy(true);
    setFieldError(undefined);
    setFailure(undefined);
    let change: Change | undefined;
    try {
      change = (await client.deployService({ id: service.id, image: wanted, summary: t`Deploy ${wanted} to ${name}` }))
        .change;
    } catch (err) {
      if (reasonOf(err) === "invalid") setFieldError(sentence(err));
      else setFailure(sentence(err));
      setBusy(false);
      return;
    }
    setBusy(false);
    setHidden(true);
    sheet.open({
      kind: "plan",
      make: () => Promise.resolve({ change }),
      onClose: (after) => {
        // Cancelling comes back here, with what you typed; anything else is under way or over.
        if (after && after.state === ChangeState.PLANNED) setHidden(false);
        else onClose();
      },
    });
  };

  return (
    <Sheet
      open={!hidden}
      initialFocusRef={field}
      title={t`Deploy ${name}`}
      subtitle={t`It runs the new image with the rest as it is`}
      onClose={onClose}
      className={cx("app-sheet", phone && "is-phone")}
      footer={
        <>
          <span className="dw-sheet-note">
            <Icon name="commit" size={14} />
            <Trans>Nothing runs until you apply the plan</Trans>
          </span>
          <Button variant="ghost" size={phone ? "lg" : "md"} onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            size={phone ? "lg" : "md"}
            loading={busy}
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
        <TextField
          label={t`Image`}
          mono
          ref={field}
          value={image}
          error={fieldError}
          hint={
            <Trans>
              It runs <code>{current}</code> now.
            </Trans>
          }
          onChange={(event) => {
            setImage(event.target.value);
            setFieldError(undefined);
          }}
        />
      </form>
    </Sheet>
  );
}
