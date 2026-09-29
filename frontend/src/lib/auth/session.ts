/**
 * Session store — the auth token + identity source of truth (Zustand).
 *
 * Owns:
 *   - access/refresh token pair + access-token expiry (ms epoch)
 *   - the `user` snapshot from POST /auth/login (roles, kyc tier); when
 *     the response omits it the snapshot is synthesized from JWT claims
 *   - decoded access-token claims + the venue admin-role hint
 *   - `authEpoch` — bumped on every session boundary (login, logout,
 *     auth failure) so optimistic state keyed on the epoch flushes
 *     (spec §21.4); token refresh deliberately does NOT bump
 *
 * The admin-role claim and `user.roles` are a UX gate ONLY — the server
 * remains the authorizer (spec §8.2); a tampered/missing claim must fail
 * safe by hiding privileged actions, and the admin surfaces still render
 * the 403 surface when the API denies.
 *
 * Persistence: `persistent` sessions live in localStorage, tab-scoped
 * sessions in sessionStorage. Refresh tokens travel in the JSON body per
 * the Phase-12 contract (src/lib/auth/refresh.ts).
 */
import { create } from 'zustand';

import { decodeJwtClaims, accessTokenExpiryMs, type JwtClaims } from './jwt';

/** §8.2 / Phase-07 Task 7.3.1 canonical admin roles — the only strings
 * that may gate admin surfaces client-side. */
export const ADMIN_ROLES = [
  'Super Admin',
  'Risk Manager',
  'Compliance Officer',
  'Finance Ops',
  'Support Agent',
  'Read-Only Auditor',
] as const;
export type VenueAdminRole = (typeof ADMIN_ROLES)[number];
const ADMIN_ROLE_SET: ReadonlySet<string> = new Set(ADMIN_ROLES);

/** Access-token claims relevant to the UI — display/expiry convenience,
 * not authorization (signature unverified client-side). */
export interface AccessClaims {
  /** Subject — trader/admin user id. */
  sub: string | null;
  /** Account id claim (trader surfaces). */
  accountId: number | null;
  /** Email claim when the issuer includes it. */
  email: string | null;
  /** Venue admin role — one of `ADMIN_ROLES`, else null. */
  adminRole: VenueAdminRole | null;
}

/** Identity snapshot delivered by POST /auth/login (normalized in
 * features/auth/api.ts `userFrom`). Roles may carry the §8.2 admin
 * names OR product roles (trader surface). */
export interface SessionUser {
  userId: string | null;
  email: string | null;
  accountId: number | null;
  roles?: string[];
  kycTier: string | null;
}

export interface SessionInput {
  accessToken: string;
  refreshToken: string | null;
  /** ms epoch; null ⇒ derive from the JWT `exp` claim. */
  accessTokenExpiresAt: number | null;
  /** Login-response identity; null ⇒ synthesized from JWT claims. */
  user: SessionUser | null;
  /** true ⇒ localStorage (survives browser restart); false ⇒ sessionStorage. */
  persistent: boolean;
}

interface SessionState {
  accessToken: string | null;
  refreshToken: string | null;
  accessTokenExpiresAt: number | null;
  user: SessionUser | null;
  persistent: boolean;
  /** Decoded access-token claims — null for non-JWT/opaque tokens. */
  claims: AccessClaims | null;
  /** Convenience accessor equal to `claims?.adminRole ?? null`. */
  adminRole: VenueAdminRole | null;
  /** Bumped on session boundaries; optimistic state keys on it. */
  authEpoch: number;
  /** Legacy single-token setter (Wave-1 wiring + the WS auth-failure
   * path): `null` is a full sign-out; a token replaces the access half
   * and clears the refresh pair (a bare token cannot carry one). */
  setAccessToken: (token: string | null) => void;
  /** Full login setter — the auth feature's write path. */
  setSession: (input: SessionInput) => void;
  /** Refresh seam — swap the access token (+ rotated refresh token),
   * keeping the user snapshot. expiresAt=null ⇒ JWT `exp` fallback.
   * Does not bump authEpoch (same session, fresher token). */
  applyRefresh: (
    accessToken: string,
    refreshToken?: string | null,
    accessTokenExpiresAt?: number | null,
  ) => void;
  clearSession: () => void;
  bumpAuthEpoch: () => void;
}

// ---------------------------------------------------------------------------
// Claim extraction
// ---------------------------------------------------------------------------

function firstString(...vals: unknown[]): string | null {
  for (const v of vals) {
    if (typeof v === 'string' && v.length > 0) return v;
    if (Array.isArray(v)) {
      const s = v.find((x): x is string => typeof x === 'string' && x.length > 0);
      if (s !== undefined) return s;
    }
  }
  return null;
}

/** Pull the venue admin role out of decoded JWT claims. Unknown names
 * map to null — an unverifiable/foreign claim must not unlock the admin
 * navigation (the server still rejects the API call). */
export function extractAdminRole(c: JwtClaims | null): VenueAdminRole | null {
  if (c === null) return null;
  const candidate = firstString(c.role, c.roles);
  if (candidate === null || !ADMIN_ROLE_SET.has(candidate)) return null;
  return candidate as VenueAdminRole;
}

/** Decode an access token's payload into `AccessClaims`, or null when
 * the token is opaque (non-JWT, malformed segment, non-JSON payload). */
export function decodeAccessClaims(token: string | null): AccessClaims | null {
  const c = decodeJwtClaims(token);
  if (c === null) return null;
  return {
    sub: c.sub ?? null,
    accountId: c.account_id ?? null,
    email: c.email ?? null,
    adminRole: extractAdminRole(c),
  };
}

/** Synthesize the user snapshot from claims when the login response did
 * not carry one (opaque user field tolerated by the Phase-12 contract). */
function userFromClaims(c: JwtClaims | null): SessionUser | null {
  if (c === null) return null;
  return {
    userId: c.sub ?? null,
    email: c.email ?? null,
    accountId: c.account_id ?? null,
    roles: c.roles ?? (c.role !== undefined ? [c.role] : undefined),
    kycTier: c.kyc_tier ?? null,
  };
}

// ---------------------------------------------------------------------------
// Persistence (localStorage for `persistent`, sessionStorage otherwise)
// ---------------------------------------------------------------------------

const STORAGE_KEY = 'exc.session.v1';

interface StoredSession {
  accessToken: string;
  refreshToken: string | null;
  accessTokenExpiresAt: number | null;
  user: SessionUser | null;
}

interface Hydrated {
  session: StoredSession;
  persistent: boolean;
}

function readStore(s: Storage | undefined): StoredSession | null {
  if (s === undefined) return null;
  try {
    const raw = s.getItem(STORAGE_KEY);
    if (raw === null) return null;
    const parsed = JSON.parse(raw) as Partial<StoredSession>;
    if (typeof parsed.accessToken !== 'string') return null;
    return {
      accessToken: parsed.accessToken,
      refreshToken: typeof parsed.refreshToken === 'string' ? parsed.refreshToken : null,
      accessTokenExpiresAt:
        typeof parsed.accessTokenExpiresAt === 'number' ? parsed.accessTokenExpiresAt : null,
      user: parsed.user ?? null,
    };
  } catch {
    return null; // corrupt blob or blocked storage — treat as absent
  }
}

function hydrate(): Hydrated | null {
  if (typeof window === 'undefined') return null;
  try {
    // sessionStorage wins — a tab-scoped login in this tab supersedes any
    // stale persistent blob written by another tab.
    const tab = readStore(window.sessionStorage);
    if (tab !== null) return { session: tab, persistent: false };
    const persisted = readStore(window.localStorage);
    if (persisted !== null) return { session: persisted, persistent: true };
  } catch {
    // privacy mode — session stays memory-only
  }
  return null;
}

function persist(st: StoredSession | null, persistent: boolean): void {
  if (typeof window === 'undefined') return;
  try {
    window.sessionStorage.removeItem(STORAGE_KEY);
    window.localStorage.removeItem(STORAGE_KEY);
    if (st !== null) {
      (persistent ? window.localStorage : window.sessionStorage).setItem(
        STORAGE_KEY,
        JSON.stringify(st),
      );
    }
  } catch {
    // storage full/blocked — keep the in-memory session
  }
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

const initial = hydrate();

export const useSessionStore = create<SessionState>()((set, get) => {
  const claimsOf = (token: string | null) => {
    const claims = decodeAccessClaims(token);
    return { claims, adminRole: claims?.adminRole ?? null };
  };

  const writeSession = (
    accessToken: string | null,
    refreshToken: string | null,
    expiresAt: number | null,
    user: SessionUser | null,
    persistent: boolean,
    bumpEpoch: boolean,
  ): void => {
    persist(
      accessToken !== null
        ? { accessToken, refreshToken, accessTokenExpiresAt: expiresAt, user }
        : null,
      persistent,
    );
    const claims = decodeAccessClaims(accessToken);
    set((s) => ({
      accessToken,
      refreshToken,
      accessTokenExpiresAt: expiresAt,
      user,
      persistent,
      claims,
      adminRole: claims?.adminRole ?? null,
      authEpoch: bumpEpoch ? s.authEpoch + 1 : s.authEpoch,
    }));
  };

  return {
    accessToken: initial?.session.accessToken ?? null,
    refreshToken: initial?.session.refreshToken ?? null,
    accessTokenExpiresAt: initial?.session.accessTokenExpiresAt ?? null,
    user: initial?.session.user ?? null,
    persistent: initial?.persistent ?? false,
    ...claimsOf(initial?.session.accessToken ?? null),
    authEpoch: 0,

    setAccessToken: (token) => {
      if (token === null) {
        get().clearSession();
        return;
      }
      const prev = get();
      writeSession(token, null, accessTokenExpiryMs(token), prev.user, prev.persistent, true);
    },

    setSession: (input) => {
      const raw = decodeJwtClaims(input.accessToken);
      const user = input.user ?? userFromClaims(raw);
      const expiresAt = input.accessTokenExpiresAt ?? accessTokenExpiryMs(input.accessToken);
      writeSession(input.accessToken, input.refreshToken, expiresAt, user, input.persistent, true);
    },

    applyRefresh: (accessToken, refreshToken, accessTokenExpiresAt) => {
      const prev = get();
      writeSession(
        accessToken,
        refreshToken ?? prev.refreshToken,
        accessTokenExpiresAt ?? accessTokenExpiryMs(accessToken),
        prev.user,
        prev.persistent,
        false,
      );
    },

    clearSession: () => {
      writeSession(null, null, null, null, get().persistent, true);
    },

    bumpAuthEpoch: () => {
      set((s) => ({ authEpoch: s.authEpoch + 1 }));
    },
  };
});

/** Test seam — reset the store to signed-out without a reload. Storage
 * is cleared too so persistence tests start clean. */
export function resetSessionForTests(): void {
  persist(null, false);
  useSessionStore.setState({
    accessToken: null,
    refreshToken: null,
    accessTokenExpiresAt: null,
    user: null,
    persistent: false,
    claims: null,
    adminRole: null,
    authEpoch: 0,
  });
}
