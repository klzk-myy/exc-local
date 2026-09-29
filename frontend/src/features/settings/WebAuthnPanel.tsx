/**
 * WebAuthn/passkey panel — Task 10.3.22 item 2 (Phase-12 Task 12.3.7).
 *
 * Ceremony (two POSTs on the same registered path):
 *   POST /account/webauthn/register {stage:'begin'}    → creation options
 *   navigator.credentials.create(options)
 *   POST /account/webauthn/register {stage:'finish',credential}
 *   POST /account/webauthn/authenticate likewise for assertion.
 *
 * A successful assertion elevates the session server-side
 * (two_factor_verified + amr ["fido2"]). The route table pins no list/
 * delete endpoint yet — registered passkeys are tracked locally by
 * credential id (deviation documented in the delivery report).
 */
import { useState } from 'react';
import { useMutation } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ErrorBox, Field, btnGhost, btnPrimary, cardCls, inputCls } from '@/lib/ui';

import * as api from './api';

interface LocalPasskey {
  credentialId: string;
  name: string;
  registeredAt: string;
}

const STORE_KEY = 'exc.passkeys.v1';

function loadPasskeys(): LocalPasskey[] {
  try {
    const raw = window.localStorage.getItem(STORE_KEY);
    if (raw === null) return [];
    const parsed = JSON.parse(raw) as LocalPasskey[];
    return Array.isArray(parsed) ? parsed : [];
  } catch {
    return [];
  }
}

function savePasskeys(ks: LocalPasskey[]) {
  try {
    window.localStorage.setItem(STORE_KEY, JSON.stringify(ks));
  } catch {
    // storage blocked — keys remain usable server-side regardless
  }
}

function bytesToB64(buf: ArrayBuffer): string {
  return btoa(String.fromCharCode(...new Uint8Array(buf)));
}

/** Convert the server's creation-options JSON into browser-typed options.
 * Only the fields the platform ceremony needs are coerced; unknown fields
 * pass through untouched. */
function toCreationOptions(raw: unknown): PublicKeyCredentialCreationOptions {
  const o = (raw ?? {}) as Record<string, unknown>;
  const src = (
    typeof o['publicKey'] === 'object' && o['publicKey'] !== null ? o['publicKey'] : o
  ) as Record<string, unknown>;
  const opts = { ...src } as Record<string, unknown>;
  if (typeof opts['challenge'] === 'string') {
    opts['challenge'] = Uint8Array.from(atob(opts['challenge']), (c) => c.charCodeAt(0));
  }
  const user = opts['user'] as Record<string, unknown> | undefined;
  if (user !== undefined && typeof user['id'] === 'string') {
    opts['user'] = {
      ...user,
      id: Uint8Array.from(atob(user['id']), (c) => c.charCodeAt(0)),
    };
  }
  const exclude = opts['excludeCredentials'];
  if (Array.isArray(exclude)) {
    opts['excludeCredentials'] = exclude.map((c) => ({
      ...(c as Record<string, unknown>),
      id: Uint8Array.from(atob(String((c as Record<string, unknown>)['id'])), (ch) =>
        ch.charCodeAt(0),
      ),
    }));
  }
  return opts as unknown as PublicKeyCredentialCreationOptions;
}

/** Serialize a PublicKeyCredential for the finish POST. */
function credentialToJSON(cred: Credential): Record<string, unknown> {
  const pk = cred as PublicKeyCredential;
  const res: Record<string, unknown> = {
    id: pk.id,
    rawId: bytesToB64(pk.rawId),
    type: pk.type,
  };
  // Attestation vs assertion response share the ArrayBuffer fields; pull
  // them dynamically so one serializer serves both ceremonies.
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

export default function WebAuthnPanel() {
  const supported = typeof window !== 'undefined' && 'PublicKeyCredential' in window;
  const [name, setName] = useState('');
  const [keys, setKeys] = useState<LocalPasskey[]>(() => loadPasskeys());
  const [status, setStatus] = useState<string | null>(null);

  const register = useMutation({
    mutationFn: async () => {
      const optsRaw = await api.webauthnRegisterBegin(apiClient, name);
      const cred = await navigator.credentials.create({
        publicKey: toCreationOptions(optsRaw),
      });
      if (cred === null) throw new Error('passkey creation cancelled');
      await api.webauthnRegisterFinish(apiClient, credentialToJSON(cred), name);
      return cred;
    },
    onSuccess: (cred) => {
      const entry = {
        credentialId: cred.id,
        name: name === '' ? 'Passkey' : name,
        registeredAt: new Date().toISOString(),
      };
      const next = [...keys, entry];
      setKeys(next);
      savePasskeys(next);
      setName('');
      setStatus('Passkey registered.');
    },
  });

  const authenticate = useMutation({
    mutationFn: async () => {
      const optsRaw = (await api.webauthnAuthenticateBegin(apiClient)) as Record<string, unknown>;
      const src = (
        typeof optsRaw['publicKey'] === 'object' && optsRaw['publicKey'] !== null
          ? optsRaw['publicKey']
          : optsRaw
      ) as Record<string, unknown>;
      const opts = { ...src } as Record<string, unknown>;
      if (typeof opts['challenge'] === 'string') {
        opts['challenge'] = Uint8Array.from(atob(opts['challenge']), (c) => c.charCodeAt(0));
      }
      const cred = await navigator.credentials.get({
        publicKey: opts as unknown as PublicKeyCredentialRequestOptions,
      });
      if (cred === null) throw new Error('passkey assertion cancelled');
      return api.webauthnAuthenticateFinish(apiClient, credentialToJSON(cred));
    },
    onSuccess: () => {
      setStatus('Passkey verified — this session is now FIDO2-authenticated.');
    },
  });

  return (
    <div className={cardCls}>
      <h3 className="mb-2 text-sm font-semibold">Passkeys (WebAuthn)</h3>
      {!supported ? (
        <p className="text-sm text-neutral-400" role="status">
          This browser does not support WebAuthn — use a browser with passkey support to register a
          hardware key or platform authenticator.
        </p>
      ) : (
        <>
          {status !== null && (
            <p className="mb-2 text-sm text-emerald-400" role="status">
              {status}
            </p>
          )}
          <ErrorBox error={register.error ?? authenticate.error} />
          {keys.length > 0 && (
            <ul className="mb-3 text-sm text-neutral-300">
              {keys.map((k) => (
                <li key={k.credentialId} className="flex items-center justify-between py-1">
                  <span>
                    {k.name}{' '}
                    <span className="text-xs text-neutral-500">
                      registered {new Date(k.registeredAt).toLocaleDateString()}
                    </span>
                  </span>
                  <button
                    type="button"
                    className={btnGhost}
                    onClick={() => {
                      const next = keys.filter((x) => x.credentialId !== k.credentialId);
                      setKeys(next);
                      savePasskeys(next);
                    }}
                  >
                    Remove
                  </button>
                </li>
              ))}
            </ul>
          )}
          <form
            onSubmit={(e) => {
              e.preventDefault();
              setStatus(null);
              register.mutate();
            }}
          >
            <Field
              label="Passkey name"
              hint="e.g. “MacBook Touch ID” — helps you recognize it later."
            >
              {(id, describedBy, invalid) => (
                <input
                  id={id}
                  aria-describedby={describedBy}
                  aria-invalid={invalid}
                  className={inputCls}
                  value={name}
                  onChange={(e) => {
                    setName(e.target.value);
                  }}
                />
              )}
            </Field>
            <button type="submit" className={btnPrimary} disabled={register.isPending}>
              {register.isPending ? 'Waiting for authenticator…' : 'Register passkey'}
            </button>
            <button
              type="button"
              className={`${btnGhost} ml-2`}
              disabled={authenticate.isPending}
              onClick={() => {
                setStatus(null);
                authenticate.mutate();
              }}
            >
              {authenticate.isPending ? 'Waiting…' : 'Verify with passkey'}
            </button>
          </form>
          <p className="mt-3 text-xs text-neutral-500">
            A successful passkey assertion elevates this session (FIDO2 second factor). Passkey
            deletion currently removes the local label only — the server-side list/delete endpoint
            is pending Phase-12.
          </p>
        </>
      )}
    </div>
  );
}
