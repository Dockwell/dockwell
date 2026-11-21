// SPDX-License-Identifier: AGPL-3.0-only

import { type BackupDestinationInfo, type ConnectionInfo, LinkKind } from "@dockwell/api";
import { BrandMark } from "@dockwell/ui";

import { type MailProviderId, mailPlaceOf, mailProvider } from "./mail";
import { type ProviderId, placeOf, provider } from "./providers";

/** A storage provider's own mark, or the product's archive icon for another S3-compatible store. */
export function StorageMark({ id, size }: { id?: ProviderId | undefined; size?: number }) {
  return <BrandMark brand={id ? provider(id).brand : undefined} icon="archive" size={size} />;
}

/** A mail service's own mark, or the product's envelope for another mail server. */
export function MailMark({ id, size }: { id?: MailProviderId | undefined; size?: number }) {
  return <BrandMark brand={id ? mailProvider(id).brand : undefined} icon="mail" size={size} />;
}

/** A connection's mark: its storage provider's or mail service's, from where it points. */
export function ConnectionMark({ connection, size }: { connection: ConnectionInfo; size?: number }) {
  if (connection.kind === LinkKind.SMTP) {
    return <MailMark id={connection.host ? mailPlaceOf(connection.host).provider : undefined} size={size} />;
  }
  return (
    <StorageMark
      id={connection.endpoint ? placeOf(connection.endpoint, connection.region).provider : undefined}
      size={size}
    />
  );
}

/** A standalone legacy backup destination's storage provider's mark. */
export function DestinationMark({ destination, size }: { destination: BackupDestinationInfo; size?: number }) {
  return <StorageMark id={destination.endpoint ? placeOf(destination.endpoint).provider : undefined} size={size} />;
}
