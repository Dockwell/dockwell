// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeState, type ServiceInfo, ServiceService } from "@dockwell/api";
import { Button, Callout, cx, Icon, PHONE_QUERY, Sheet, TextField, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useEffect, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { useApp } from "./AppContext";
import { imageOf, isMoving, nameOf, tidyMessage } from "./app";

/**
 * Deploys another image: one field, then the plan. Everything else about the
 * app stays, so nothing here has to read a secret back. Dockwell refuses a
 * bad image at the field, and an image it runs already, before there is a
 * plan to review. Git sources come with phase 2.
 */
export function DeploySheet({ service, onClose }: { service: ServiceInfo; onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(ServiceService, transport), [transport]);
  const name = nameOf(service);
  const current = imageOf(service);
  const [image, setImage] = useState(current);
  const [busy, setBusy] = useState(false);
  const [fieldError, setFieldError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  // The plan under review takes this sheet over, and Cancel brings the form back as it was.
  const [plan, setPlan] = useState<Opening>();
  const field = useRef<HTMLInputElement>(null);
  const { state } = useApp();
  // One change at a time: this one's plan couldn't apply until that one ends.
  const moving = isMoving(state);
  const again = image.trim() === current;

  // Back from a plan that failed, what it said is at the field, ready to change.
  const reveal = useRef(false);
  useEffect(() => {
    if (plan || !reveal.current) return;
    reveal.current = false;
    field.current?.focus();
    field.current?.select();
  }, [plan]);

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy || moving) return;
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
    setPlan({
      kind: "plan",
      make: () => Promise.resolve({ change }),
      retryLabel: t`Edit and try again`,
      onDone: onClose,
      fix: (reason) => {
        // This form has an image and nothing else, so that is what there is to check here. A
        // check that fails says what it answered, and what to do about that, which is the server's.
        if (reason === "exits" || reason === "command") {
          return t`Check the image and tag, then deploy again.`;
        }
        return reason === "port" ? t`Stop what uses it on this server, then deploy again.` : undefined;
      },
      onClose: (after) => {
        // Cancelling comes back here, with what you typed, as failing does, saying why
        // at the field. Anything else is under way or over.
        if (after?.state === ChangeState.FAILED) {
          setFieldError(tidyMessage(after.error));
          reveal.current = true;
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
            disabled={moving}
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
            moving ? (
              <Trans>
                <code>{current}</code> is still deploying. You can review this plan once that ends.
              </Trans>
            ) : again ? (
              <Trans>
                Pulls <code>{current}</code> again, and replaces {name} only if that's a newer image.
              </Trans>
            ) : (
              <Trans>
                It runs <code>{current}</code> now.
              </Trans>
            )
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
