/**
 * WebAuthn browser helpers — shared by the passkey-sign-in flow
 * (POST /auth/passkey/assert) and the authenticated credential manager
 * (/account/webauthn/register|authenticate). Extracted from
 * features/settings/WebAuthnPanel.tsx (Task 10.5.3.24).
 */

export function webauthnSupported(): boolean {
  return typeof window !== 'undefined' && 'PublicKeyCredential' in window;
}

export function bytesToB64(buf: ArrayBuffer): string {
  return btoa(String.fromCharCode(...new Uint8Array(buf)));
}

const b64ToBytes = (s: string): Uint8Array<ArrayBuffer> =>
  Uint8Array.from(atob(s), (c) => c.charCodeAt(0));

/** Unwrap the server's options JSON — the publicKey may ride a
 * `publicKey` key or sit at the top level. */
function unwrapOptions(raw: unknown): Record<string, unknown> {
  const o = (raw ?? {}) as Record<string, unknown>;
  return (
    typeof o['publicKey'] === 'object' && o['publicKey'] !== null ? o['publicKey'] : o
  ) as Record<string, unknown>;
}

/** Convert the server's creation-options JSON into browser-typed options.
 * Only the fields the platform ceremony needs are coerced; unknown fields
 * pass through untouched. */
export function toCreationOptions(raw: unknown): PublicKeyCredentialCreationOptions {
  const opts = { ...unwrapOptions(raw) };
  if (typeof opts['challenge'] === 'string') {
    opts['challenge'] = b64ToBytes(opts['challenge']);
  }
  const user = opts['user'] as Record<string, unknown> | undefined;
  if (user !== undefined && typeof user['id'] === 'string') {
    opts['user'] = { ...user, id: b64ToBytes(user['id']) };
  }
  const exclude = opts['excludeCredentials'];
  if (Array.isArray(exclude)) {
    opts['excludeCredentials'] = exclude.map((c) => ({
      ...(c as Record<string, unknown>),
      id: b64ToBytes(String((c as Record<string, unknown>)['id'])),
    }));
  }
  return opts as unknown as PublicKeyCredentialCreationOptions;
}

/** Convert the server's request-options JSON into browser-typed options
 * (challenge + allowCredentials[].id are base64 strings over the wire). */
export function toRequestOptions(raw: unknown): PublicKeyCredentialRequestOptions {
  const opts = { ...unwrapOptions(raw) };
  if (typeof opts['challenge'] === 'string') {
    opts['challenge'] = b64ToBytes(opts['challenge']);
  }
  const allow = opts['allowCredentials'];
  if (Array.isArray(allow)) {
    opts['allowCredentials'] = allow.map((c) => ({
      ...(c as Record<string, unknown>),
      id: b64ToBytes(String((c as Record<string, unknown>)['id'])),
    }));
  }
  return opts as unknown as PublicKeyCredentialRequestOptions;
}

/** Serialize a PublicKeyCredential for the finish POST. Attestation and
 * assertion responses share the ArrayBuffer fields; pull them dynamically
 * so one serializer serves both ceremonies. */
export function credentialToJSON(cred: Credential): Record<string, unknown> {
  const pk = cred as PublicKeyCredential;
  const res: Record<string, unknown> = {
    id: pk.id,
    rawId: bytesToB64(pk.rawId),
    type: pk.type,
  };
  const r = pk.response as unknown as Record<string, unknown>;
  const buf = (v: unknown) => (v instanceof ArrayBuffer ? bytesToB64(v) : undefined);
  res['response'] = {
    clientDataJSON: buf(r['clientDataJSON']),
    attestationObject: buf(r['attestationObject']),
    authenticatorData: buf(r['authenticatorData']),
    signature: buf(r['signature']),
  };
  return res;
}
