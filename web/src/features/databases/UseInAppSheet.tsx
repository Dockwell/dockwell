// SPDX-License-Identifier: AGPL-3.0-only

import type { Database, DatabaseAccount } from "@dockwell/api";
import { Button, cx, PHONE_QUERY, SelectField, Sheet, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useRef, useState } from "react";
import { appOf, environmentOf, nameOf } from "../apps/app";
import { ConnectDatabaseSheet } from "../apps/ConnectDatabaseSheet";
import { usePlaces, useServices } from "../apps/useServices";
import { engineOfImage } from "./useDatabases";

/**
 * Gives one of a database's users to an app (#798): the app is the one choice here,
 * those beside the database first, and Connect a database opens on it with this
 * database, the user and what they own already chosen.
 */
export function UseInAppSheet({
  db,
  user,
  onClose,
  onAddVariable,
}: {
  db: Database;
  user: DatabaseAccount;
  onClose: () => void;
  /** Where the app's own variable is to be added, as Connect a database asks for one. */
  onAddVariable: (serviceId: string) => void;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const places = usePlaces();
  const field = useRef<HTMLSelectElement>(null);
  const here = db.resource?.scope ?? "";
  // An app is what doesn't run a database itself, beside this one first.
  const apps = (useServices().data ?? [])
    .filter((s) => s.id !== db.id && engineOfImage(appOf(s)?.image ?? "") === undefined)
    .sort((a, b) => Number(environmentOf(b) === here) - Number(environmentOf(a) === here));
  const [picked, setPicked] = useState<string>();
  const [going, setGoing] = useState(false);
  const app = apps.find((s) => s.id === picked) ?? apps[0];
  const name = user.name;
  if (going && app) {
    return (
      <ConnectDatabaseSheet
        service={app}
        preset={{
          server: db.id,
          database: user.databases[0],
          user: name,
          // Its secret is the app's to use only where they share an environment.
          password: environmentOf(app) === here && user.secret ? user.secret : undefined,
        }}
        onClose={onClose}
        onAddVariable={() => onAddVariable(app.id)}
      />
    );
  }
  return (
    <Sheet
      open
      initialFocusRef={field}
      title={t`Use ${name} in an app`}
      subtitle={t`The app signs in as ${name}, through a link that follows the database`}
      onClose={onClose}
      className={cx("app-sheet", phone && "is-phone")}
      footer={
        <>
          <Button variant="ghost" size={phone ? "lg" : "md"} onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" size={phone ? "lg" : "md"} disabled={!app} onClick={() => setGoing(true)}>
            <Trans>Continue</Trans>
          </Button>
        </>
      }
    >
      <SelectField
        ref={field}
        label={t`App`}
        value={app?.id ?? ""}
        disabled={apps.length === 0}
        hint={apps.length === 0 ? t`No app runs yet` : undefined}
        options={apps.map((s) => ({
          value: s.id,
          label: [nameOf(s), environmentOf(s) === here ? "" : places.label(environmentOf(s))]
            .filter(Boolean)
            .join(" · "),
        }))}
        onChange={(event) => setPicked(event.target.value)}
      />
    </Sheet>
  );
}
