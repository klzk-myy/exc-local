import { describe, expect, it } from 'vitest';

import { accessTokenExpiryMs, decodeJwtClaims, tokenRoles } from './jwt';

function b64url(obj: unknown): string {
  return btoa(JSON.stringify(obj)).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '');
}

function jwt(payload: Record<string, unknown>): string {
  return `${b64url({ alg: 'none' })}.${b64url(payload)}.sig`;
}

describe('decodeJwtClaims', () => {
  it('decodes the payload claims', () => {
    const t = jwt({
      sub: '42',
      exp: 2000,
      account_id: 7,
      roles: ['Support Agent'],
      amr: ['pwd', 'totp'],
    });
    const c = decodeJwtClaims(t);
    expect(c?.sub).toBe('42');
    expect(c?.exp).toBe(2000);
    expect(c?.account_id).toBe(7);
    expect(c?.roles).toEqual(['Support Agent']);
    expect(c?.amr).toEqual(['pwd', 'totp']);
  });

  it('returns null for non-JWT input', () => {
    expect(decodeJwtClaims('not-a-jwt')).toBeNull();
    expect(decodeJwtClaims(null)).toBeNull();
    expect(decodeJwtClaims('a.b.c')).toBeNull(); // bad b64/json
  });

  it('accessTokenExpiryMs converts exp seconds to ms', () => {
    expect(accessTokenExpiryMs(jwt({ exp: 2000 }))).toBe(2_000_000);
    expect(accessTokenExpiryMs('opaque')).toBeNull();
  });

  it('tokenRoles prefers the roles array, falls back to role', () => {
    expect(tokenRoles(jwt({ roles: ['Super Admin'] }))).toEqual(['Super Admin']);
    expect(tokenRoles(jwt({ role: 'Risk Manager' }))).toEqual(['Risk Manager']);
    expect(tokenRoles(jwt({}))).toEqual([]);
  });
});
