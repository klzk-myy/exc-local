/**
 * JWT payload decode — display/expiry use ONLY (no signature verification;
 * the server is the authority, this is a client-side convenience for
 * expiry scheduling and role hints, Task 10.3.21).
 *
 * The Phase-12 login response is the source of truth; the JWT is decoded
 * to recover `exp` (access-token lifetime scheduling) and identity/role
 * hints (`sub`, `account_id`, `roles`, `amr`) when the response omits them.
 */
export interface JwtClaims {
  sub?: string;
  exp?: number; // seconds since epoch
  iat?: number;
  account_id?: number;
  email?: string;
  roles?: string[];
  role?: string;
  amr?: string[];
  sid?: string;
  kyc_tier?: string;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}

function toStringArray(v: unknown): string[] | undefined {
  if (!Array.isArray(v)) return undefined;
  const out = v.filter((x): x is string => typeof x === 'string');
  return out.length > 0 ? out : undefined;
}

/** base64url → string. Returns null on malformed input (never throws —
 * a malformed token is opaque bearer material, not a parse failure). */
function decodeSegment(seg: string): string | null {
  const b64 = seg.replace(/-/g, '+').replace(/_/g, '/');
  const pad = b64.length % 4 === 0 ? '' : '='.repeat(4 - (b64.length % 4));
  try {
    return atob(b64 + pad);
  } catch {
    return null;
  }
}

/** Decode a JWT payload segment into claims, or null for non-JWT input. */
export function decodeJwtClaims(token: string | null): JwtClaims | null {
  if (!token) return null;
  const parts = token.split('.');
  if (parts.length !== 3) return null;
  const payloadSeg = parts[1];
  if (payloadSeg === undefined) return null;
  const json = decodeSegment(payloadSeg);
  if (json === null) return null;
  let raw: unknown;
  try {
    raw = JSON.parse(json);
  } catch {
    return null;
  }
  if (!isRecord(raw)) return null;
  const claims: JwtClaims = {};
  if (typeof raw['sub'] === 'string') claims.sub = raw['sub'];
  if (typeof raw['exp'] === 'number') claims.exp = raw['exp'];
  if (typeof raw['iat'] === 'number') claims.iat = raw['iat'];
  if (typeof raw['account_id'] === 'number') claims.account_id = raw['account_id'];
  if (typeof raw['email'] === 'string') claims.email = raw['email'];
  if (typeof raw['role'] === 'string') claims.role = raw['role'];
  if (typeof raw['kyc_tier'] === 'string') claims.kyc_tier = raw['kyc_tier'];
  if (typeof raw['sid'] === 'string') claims.sid = raw['sid'];
  const roles = toStringArray(raw['roles']);
  if (roles) claims.roles = roles;
  const amr = toStringArray(raw['amr']);
  if (amr) claims.amr = amr;
  return claims;
}

/** Access-token expiry in ms-epoch, or null when unknown (non-JWT token
 * or no exp claim — callers treat null as "no scheduled expiry"). */
export function accessTokenExpiryMs(token: string | null): number | null {
  const exp = decodeJwtClaims(token)?.exp;
  return typeof exp === 'number' ? exp * 1000 : null;
}

/** Role names carried on the token (`roles` array or singular `role`),
 * normalized to the §8.2 role-name set for RequireRole gating. */
export function tokenRoles(token: string | null): string[] {
  const c = decodeJwtClaims(token);
  if (!c) return [];
  if (c.roles) return c.roles;
  return c.role !== undefined ? [c.role] : [];
}
