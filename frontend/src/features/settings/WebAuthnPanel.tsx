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
import {
  credentialToJSON,
  toCreationOptions,
  toRequestOptions,
  webauthnSupported,
} from '@/lib/auth/webauthn';
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

export default function WebAuthnPanel() {
  const supported = webauthnSupported();
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
      const optsRaw = await api.webauthnAuthenticateBegin(apiClient);
      const cred = await navigator.credentials.get({
        publicKey: toRequestOptions(optsRaw),
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
