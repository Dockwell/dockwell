// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import {
  CloudProvider,
  NetworkShape,
  type PublicAddress,
  PublicAddressSource,
  Reachable,
  type Server,
  type ServerNetwork,
} from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";

import { useTimeWords } from "../../lib/time";

/** The providers by name, which aren't words to translate. */
const PROVIDERS: Partial<Record<CloudProvider, string>> = {
  [CloudProvider.AWS]: "AWS",
  [CloudProvider.GCP]: "Google Cloud",
  [CloudProvider.AZURE]: "Azure",
  [CloudProvider.DIGITALOCEAN]: "DigitalOcean",
  [CloudProvider.HETZNER]: "Hetzner",
  [CloudProvider.ORACLE]: "Oracle Cloud",
};

export function providerName(provider: CloudProvider | undefined): string | undefined {
  return provider === undefined ? undefined : PROVIDERS[provider];
}

/** What's known of a server's public addresses, IPv4 first, as each family has its own. */
export function publicAddressesOf(network: ServerNetwork | undefined): PublicAddress[] {
  return [network?.ipv4, network?.ipv6].filter((address) => address !== undefined);
}

/**
 * The public address to show as the server's, or nothing. Only a way in is one: a way
 * out may be a router's or a NAT gateway's, which is no address of the server's.
 */
export function publicAddressOf(server: Server): string | undefined {
  return publicAddressesOf(server.network).find((address) => !address.unproven)?.address;
}

/** What to say of a server's network: which shape it has, how its address is known, and what's proved of the way in. */
export function useNetworkWords() {
  const { t } = useLingui();
  const time = useTimeWords();
  return {
    /** Which of the shapes a server comes in this one has, or nothing while that isn't known. */
    shape(network: ServerNetwork | undefined): string | undefined {
      const provider = providerName(network?.provider);
      switch (network?.shape) {
        case NetworkShape.INTERNET:
          return t`On the internet`;
        case NetworkShape.CLOUD_NAT:
          return provider ? t`Behind ${provider}'s NAT` : t`Behind a cloud's NAT`;
        case NetworkShape.LOCAL_NAT:
          return t`Likely a home or office network`;
        case NetworkShape.CARRIER_NAT:
          return t`Behind carrier NAT`;
        default:
          return undefined;
      }
    },
    /** How one of its public addresses is known, and since when. */
    how(address: PublicAddress, network: ServerNetwork, now: number): string {
      const when = address.knownTime ? time.ago(timestampDate(address.knownTime), now) : "";
      const by = address.setBy?.name ?? "";
      const domain = address.domain;
      const provider = providerName(network.provider);
      switch (address.source) {
        case PublicAddressSource.SET_BY_PERSON:
          return by ? t`Set by ${by}, ${when}` : t`Set by a person, ${when}`;
        case PublicAddressSource.ON_INTERFACE:
          return t`On one of this server's interfaces`;
        case PublicAddressSource.PROVED_BY_DOMAIN:
          return t`Proved by the certificate for ${domain}, as of ${when}`;
        case PublicAddressSource.CLOUD_METADATA:
          if (address.unproven) {
            return t`A metadata service says so, ${when}, and this machine doesn't name that cloud, so it may not be a way in`;
          }
          return provider ? t`${provider}'s metadata says so, ${when}` : t`The cloud's metadata says so, ${when}`;
        case PublicAddressSource.SEEN_BY_CONTROL_PLANE:
          return t`Seen as its agent connected, ${when}. That's its way out, and may not be a way in`;
        case PublicAddressSource.LOOKED_UP:
          return network.shape === NetworkShape.LOCAL_NAT
            ? t`Looked up, ${when}. That's likely your router's address, a way out and not a way in`
            : t`Looked up, ${when}. That's its way out, and may not be a way in`;
        default:
          return "";
      }
    },
    /** What's proved of the internet reaching it, in a state's shape and word, and the proof. */
    reachable(network: ServerNetwork | undefined, now: number): { status: Status; word: string; proof: string } {
      const domain = network?.reachableDomain ?? "";
      const when = network?.reachableTime ? time.ago(timestampDate(network.reachableTime), now) : "";
      switch (network?.reachable) {
        case Reachable.YES:
          return {
            status: "healthy",
            word: t`Yes`,
            proof: t`A certificate came for ${domain} on port 80 or 443, as of ${when}`,
          };
        case Reachable.NO:
          return {
            status: "down",
            word: t`No`,
            proof: t`The certificate authority couldn't connect for ${domain} on port 80 or 443, as of ${when}`,
          };
        default:
          return {
            status: "unknown",
            word: t`Not checked`,
            proof: t`Only something outside connecting proves a way in, such as a certificate for a domain here`,
          };
      }
    },
  };
}
