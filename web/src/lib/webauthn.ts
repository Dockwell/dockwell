// SPDX-License-Identifier: AGPL-3.0-only

/**
 * Passkeys through the browser's own WebAuthn API. The control plane sends
 * options as WebAuthn's JSON forms (PublicKeyCredentialCreationOptionsJSON
 * and PublicKeyCredentialRequestOptionsJSON) and takes the browser's answer
 * back as RegistrationResponseJSON or AuthenticationResponseJSON. Binary
 * fields travel as base64url without padding, which these helpers convert.
 */

/** Encodes bytes as base64url without padding. */
export function toBase64url(bytes: ArrayBuffer | Uint8Array): string {
  const view = bytes instanceof Uint8Array ? bytes : new Uint8Array(bytes);
  let binary = "";
  for (const b of view) binary += String.fromCharCode(b);
  return btoa(binary).replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "");
}

/** Decodes base64url, with or without padding. */
export function fromBase64url(text: string): Uint8Array<ArrayBuffer> {
  const base64 = text.replaceAll("-", "+").replaceAll("_", "/");
  const binary = atob(base64.padEnd(base64.length + ((4 - (base64.length % 4)) % 4), "="));
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
  return out;
}

interface DescriptorJSON {
  id: string;
  type: PublicKeyCredentialType;
  transports?: AuthenticatorTransport[];
}

interface CreationOptionsJSON
  extends Omit<PublicKeyCredentialCreationOptions, "challenge" | "user" | "excludeCredentials"> {
  challenge: string;
  user: { id: string; name: string; displayName: string };
  excludeCredentials?: DescriptorJSON[];
}

interface RequestOptionsJSON extends Omit<PublicKeyCredentialRequestOptions, "challenge" | "allowCredentials"> {
  challenge: string;
  allowCredentials?: DescriptorJSON[];
}

const descriptor = (d: DescriptorJSON): PublicKeyCredentialDescriptor => ({
  id: fromBase64url(d.id),
  type: d.type,
  ...(d.transports ? { transports: d.transports } : {}),
});

/** Turns PublicKeyCredentialCreationOptionsJSON into what navigator.credentials.create takes. */
export function parseCreationOptions(json: string): PublicKeyCredentialCreationOptions {
  const o = JSON.parse(json) as CreationOptionsJSON;
  return {
    ...o,
    challenge: fromBase64url(o.challenge),
    user: { ...o.user, id: fromBase64url(o.user.id) },
    excludeCredentials: (o.excludeCredentials ?? []).map(descriptor),
  };
}

/** Turns PublicKeyCredentialRequestOptionsJSON into what navigator.credentials.get takes. */
export function parseRequestOptions(json: string): PublicKeyCredentialRequestOptions {
  const o = JSON.parse(json) as RequestOptionsJSON;
  return {
    ...o,
    challenge: fromBase64url(o.challenge),
    allowCredentials: (o.allowCredentials ?? []).map(descriptor),
  };
}

/** Why a passkey ceremony didn't finish, in terms the screens can explain. */
export type PasskeyProblem =
  /** The person closed the prompt, or it timed out. */
  | "cancelled"
  /** This authenticator already holds a passkey for this account. */
  | "exists"
  /** The browser can't make or use passkeys here: no WebAuthn, or an insecure address. */
  | "unsupported"
  /** Anything else the browser reported. */
  | "failed";

export class PasskeyError extends Error {
  constructor(
    readonly problem: PasskeyProblem,
    message: string,
  ) {
    super(message);
    this.name = "PasskeyError";
  }
}

function problemOf(err: unknown): PasskeyError {
  if (err instanceof PasskeyError) return err;
  const name = err instanceof DOMException || err instanceof Error ? err.name : "";
  const message = err instanceof Error ? err.message : String(err);
  switch (name) {
    case "NotAllowedError":
    case "AbortError":
      return new PasskeyError("cancelled", message);
    case "InvalidStateError":
      return new PasskeyError("exists", message);
    case "SecurityError":
    case "NotSupportedError":
      return new PasskeyError("unsupported", message);
    default:
      return new PasskeyError("failed", message);
  }
}

/** True when this browser can use passkeys on this page. */
export function passkeysAvailable(): boolean {
  return (
    typeof window !== "undefined" && window.isSecureContext === true && typeof window.PublicKeyCredential === "function"
  );
}

function credentials(): CredentialsContainer {
  if (!passkeysAvailable()) {
    throw new PasskeyError("unsupported", "WebAuthn isn't available on this page");
  }
  return navigator.credentials;
}

const b64 = (buf: ArrayBuffer | null | undefined) => (buf ? toBase64url(buf) : undefined);

/** Creates a passkey for options and returns RegistrationResponseJSON. */
export async function createPasskey(optionsJson: string, signal?: AbortSignal): Promise<string> {
  try {
    const cred = (await credentials().create({
      publicKey: parseCreationOptions(optionsJson),
      ...(signal ? { signal } : {}),
    })) as PublicKeyCredential | null;
    if (!cred) throw new PasskeyError("cancelled", "no passkey was created");
    const res = cred.response as AuthenticatorAttestationResponse;
    return JSON.stringify({
      id: cred.id,
      rawId: toBase64url(cred.rawId),
      type: cred.type,
      response: {
        clientDataJSON: toBase64url(res.clientDataJSON),
        attestationObject: toBase64url(res.attestationObject),
        transports: typeof res.getTransports === "function" ? res.getTransports() : [],
      },
      clientExtensionResults: cred.getClientExtensionResults(),
      authenticatorAttachment: cred.authenticatorAttachment ?? undefined,
    });
  } catch (err) {
    throw problemOf(err);
  }
}

/**
 * Which authenticator the browser should offer first (WebAuthn Level 3
 * hints): this device's own, or a security key or phone. Browsers that don't
 * know hints ignore them.
 */
export type PasskeyHint = "client-device" | "security-key" | "hybrid";

/** Signs in with a passkey for options and returns AuthenticationResponseJSON. */
export async function getPasskey(
  optionsJson: string,
  hints: PasskeyHint[] = [],
  signal?: AbortSignal,
): Promise<string> {
  try {
    const publicKey: PublicKeyCredentialRequestOptions & { hints?: PasskeyHint[] } = parseRequestOptions(optionsJson);
    if (hints.length > 0) publicKey.hints = hints;
    const cred = (await credentials().get({
      publicKey,
      ...(signal ? { signal } : {}),
    })) as PublicKeyCredential | null;
    if (!cred) throw new PasskeyError("cancelled", "no passkey was chosen");
    const res = cred.response as AuthenticatorAssertionResponse;
    return JSON.stringify({
      id: cred.id,
      rawId: toBase64url(cred.rawId),
      type: cred.type,
      response: {
        clientDataJSON: toBase64url(res.clientDataJSON),
        authenticatorData: toBase64url(res.authenticatorData),
        signature: toBase64url(res.signature),
        userHandle: b64(res.userHandle),
      },
      clientExtensionResults: cred.getClientExtensionResults(),
      authenticatorAttachment: cred.authenticatorAttachment ?? undefined,
    });
  } catch (err) {
    throw problemOf(err);
  }
}
