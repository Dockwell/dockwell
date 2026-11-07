// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type PublicAddress, PublicAddressSource, type Server, ServerService } from "@dockwell/api";
import { Button, Callout, Sheet, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";

/**
 * Says what a server's public address is, for one Dockwell can't find out, as a
 * cloud's 1:1 NAT is on no interface. It wins over everything else Dockwell knows, so
 * every word about DNS uses it, and it goes in the activity log. It says nothing of
 * whether the internet can reach the server, which only something outside can show.
 */
export function SetPublicAddressSheet({
  server,
  current,
  onClose,
}: {
  server: Server;
  /** The address it changes, or nothing for a first one. */
  current?: PublicAddress;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const client = useMemo(() => createClient(ServerService, transport), [transport]);
  const field = useRef<HTMLInputElement>(null);
  const set = current?.source === PublicAddressSource.SET_BY_PERSON;
  const [address, setAddress] = useState(set ? (current?.address ?? "") : "");
  const [error, setError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [busy, setBusy] = useState(false);
  const name = server.name;

  const save = async (wanted: string) => {
    if (busy) return;
    setBusy(true);
    setError(undefined);
    setFailure(undefined);
    try {
      // With nothing to go by, clearing has to say which of the server's two it means.
      const ipv6 = wanted === "" && Boolean(current?.address.includes(":"));
      await client.setServerPublicAddress({ id: server.id, address: wanted, ipv6 });
      onClose();
    } catch (err) {
      if (reasonOf(err) === "invalid") {
        setError(sentence(err));
        field.current?.focus();
        field.current?.select();
      } else {
        setFailure(sentence(err));
      }
      setBusy(false);
    }
  };

  const submit = (event?: FormEvent) => {
    event?.preventDefault();
    const wanted = address.trim();
    if (wanted === "") {
      setError(t`Name the address, such as 203.0.113.7.`);
      return;
    }
    void save(wanted);
  };

  return (
    <Sheet
      open
      initialFocusRef={field}
      title={t`Set the public address of ${name}`}
      subtitle={t`What the internet knows this server by`}
      onClose={onClose}
      footer={
        <>
          <span className="dw-sheet-note">
            <Trans>This goes in the activity log</Trans>
          </span>
          {set ? (
            <Button variant="ghost" onClick={() => void save("")}>
              <Trans>Clear</Trans>
            </Button>
          ) : null}
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" loading={busy} onClick={() => submit()}>
            <Trans>Set address</Trans>
          </Button>
        </>
      }
    >
      <form onSubmit={submit}>
        {failure ? <Callout tone="critical">{failure}</Callout> : null}
        <TextField
          ref={field}
          label={t`Public address`}
          mono
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          value={address}
          error={error}
          hint={t`The address your provider's console shows. Every word about DNS uses it from now on.`}
          onChange={(event) => {
            setAddress(event.target.value);
            setError(undefined);
          }}
        />
      </form>
    </Sheet>
  );
}
