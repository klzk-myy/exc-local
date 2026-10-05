import { beforeEach, describe, expect, it, vi } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Route, Routes } from 'react-router';

import { resetSessionForTests } from '@/lib/auth/session';
import { installFetchMock, renderApp, signInForTests } from '@/test/accountMocks';

import KycPage from './KycPage';
import { validateDocFile, TIER_LIMITS } from './api';

vi.mock('@/app/runtime', () => import('@/test/accountMocks').then((m) => m.runtimeModule()));

beforeEach(() => {
  window.localStorage.clear();
  resetSessionForTests();
  signInForTests();
});

function renderKyc() {
  return renderApp(
    <Routes>
      <Route path="/kyc" element={<KycPage />} />
      <Route path="/login" element={<div>login</div>} />
    </Routes>,
    '/kyc',
  );
}

function file(name: string, size: number, type: string) {
  return new File(['x'.repeat(Math.min(size, 64))], name, { type });
}

/** Merged ops-matrix matching the default static grid — keeps the wizard
 * on the same doc slots the earlier tests exercise. */
const REQUIREMENTS = {
  policy: {
    tier: 'T1',
    liveness_required: false,
    biometric_required: false,
    rescreen_cadence: 'NONE',
    reverify_months: 0,
    manual_review_sla_hours: 48,
    daily_withdrawal_usd: '10000',
    daily_trading_usd: null,
  },
  documents: [
    { document_type: 'GOVERNMENT_ID', doc_group: 'IDENTITY', required: true },
    {
      document_type: 'PROOF_OF_ADDRESS',
      doc_group: 'ADDRESS',
      required: true,
      max_doc_age_days: 90,
    },
    { document_type: 'SELFIE', doc_group: 'LIVENESS', required: false },
  ],
};

describe('KycPage', () => {
  it('shows the status tracker with tier limits for a T1 account', async () => {
    installFetchMock({
      'GET /api/v1/kyc/status': {
        body: {
          tier: 'T1',
          status: 'APPROVED',
          documents: [
            { type: 'GOVERNMENT_ID', status: 'APPROVED', submitted_at: '2026-01-01T00:00:00Z' },
          ],
        },
      },
    });
    renderKyc();
    expect(await screen.findByText('Verification status')).toBeInTheDocument();
    expect(screen.getByText('T1')).toBeInTheDocument();
    expect(screen.getAllByText('APPROVED').length).toBeGreaterThan(0);
    expect(screen.getByText('$10,000 / day withdrawal')).toBeInTheDocument();
    // Approved + not expired → no upload wizard
    expect(screen.queryByText('Personal details')).not.toBeInTheDocument();
  });

  it('shows the re-verification prompt and wizard on EXPIRED', async () => {
    installFetchMock({
      'GET /api/v1/kyc/status': {
        body: {
          tier: 'T2',
          status: 'EXPIRED',
          re_verification_due: '2026-02-01T00:00:00Z',
          documents: [{ type: 'GOVERNMENT_ID', status: 'EXPIRED' }],
        },
      },
    });
    renderKyc();
    expect(await screen.findByRole('alert')).toHaveTextContent('Re-verification required');
    expect(screen.getByText(/degraded to T0 limits/)).toBeInTheDocument();
    expect(screen.getByText('1. Personal details')).toBeInTheDocument();
  });

  it('walks the wizard and submits documents as base64 JSON', async () => {
    const calls = installFetchMock({
      'GET /api/v1/kyc/status': { body: { tier: 'T0', status: 'PENDING', documents: [] } },
      'GET /api/v1/kyc/requirements': { body: REQUIREMENTS },
      'POST /api/v1/kyc/submit': { status: 200, body: {} },
    });
    renderKyc();
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText(/First name/), 'Ada');
    await user.type(screen.getByLabelText(/Last name/), 'Trader');
    await user.type(screen.getByLabelText(/Date of birth/), '1990-01-01');
    await user.type(screen.getByLabelText(/Nationality/), 'CH');
    await user.type(screen.getByLabelText(/Residential address/), 'Bahnhofstrasse 1');
    await user.click(screen.getByRole('button', { name: 'Next' }));

    await user.upload(
      await screen.findByLabelText(/Government-issued ID/),
      file('passport.png', 1024, 'image/png'),
    );
    await user.click(screen.getByRole('button', { name: 'Next' }));
    await user.upload(
      await screen.findByLabelText(/Proof of address/),
      file('bill.pdf', 1024, 'application/pdf'),
    );
    await user.click(screen.getByRole('button', { name: 'Next' }));
    // Optional SELFIE step — skipped without an attachment.
    await user.click(await screen.findByRole('button', { name: 'Next' }));
    await user.click(await screen.findByRole('button', { name: 'Submit for verification' }));

    await waitFor(() => {
      const c = calls.find((x) => x.url.includes('/kyc/submit'));
      const body = JSON.parse(c?.init?.body as string) as {
        personal: { first_name: string };
        documents: { type: string; data_base64: string }[];
      };
      expect(body.personal.first_name).toBe('Ada');
      expect(body.documents.map((d) => d.type)).toEqual(
        expect.arrayContaining(['GOVERNMENT_ID', 'PROOF_OF_ADDRESS']),
      );
      expect(body.documents[0]?.data_base64.length).toBeGreaterThan(0);
    });
  });

  it('rejects an oversize file client-side', async () => {
    installFetchMock({
      'GET /api/v1/kyc/status': { body: { tier: 'T0', status: 'PENDING', documents: [] } },
      'GET /api/v1/kyc/requirements': { body: REQUIREMENTS },
    });
    renderKyc();
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText(/First name/), 'A');
    await user.type(screen.getByLabelText(/Last name/), 'B');
    await user.type(screen.getByLabelText(/Date of birth/), '1990-01-01');
    await user.type(screen.getByLabelText(/Nationality/), 'CH');
    await user.type(screen.getByLabelText(/Residential address/), 'X');
    await user.click(screen.getByRole('button', { name: 'Next' }));
    const big = new File([new Uint8Array(11 * 1024 * 1024)], 'huge.png', { type: 'image/png' });
    await user.upload(await screen.findByLabelText(/Government-issued ID/), big);
    expect(await screen.findByRole('alert')).toHaveTextContent('10 MB');
  });
  it('renders the matrix-driven checklist and tier policy card', async () => {
    installFetchMock({
      'GET /api/v1/kyc/status': { body: { tier: 'T2', status: 'REJECTED', documents: [] } },
      'GET /api/v1/kyc/requirements': {
        body: {
          policy: {
            tier: 'T2',
            liveness_required: true,
            rescreen_cadence: 'WEEKLY',
            reverify_months: 12,
            manual_review_sla_hours: 24,
            daily_withdrawal_usd: '100000',
          },
          documents: [
            { document_type: 'PASSPORT', doc_group: 'IDENTITY', required: true },
            { document_type: 'SELFIE', doc_group: 'LIVENESS', required: true },
            { document_type: 'QUESTIONNAIRE', doc_group: 'PROFILE', required: false },
          ],
        },
      },
    });
    renderKyc();
    // Policy card — verbatim matrix fields.
    expect(await screen.findByText(/Requirements for your tier — T2/)).toBeInTheDocument();
    expect(screen.getByText('Every 12 months')).toBeInTheDocument();
    expect(screen.getByText('WEEKLY')).toBeInTheDocument();
    expect(screen.getByText(/2 required documents · 1 optional/)).toBeInTheDocument();
    // Wizard steps reflect the matrix: PASSPORT (unmapped type rendered
    // verbatim) + SELFIE required, QUESTIONNAIRE optional — no static PoA.
    expect(screen.getByText('2. PASSPORT')).toBeInTheDocument();
    expect(screen.getByText('3. Selfie / liveness photo')).toBeInTheDocument();
    expect(screen.getByText('4. Appropriateness questionnaire')).toBeInTheDocument();
    expect(screen.queryByText(/Proof of address/)).not.toBeInTheDocument();
  });

  it('falls back to the default checklist when requirements fail', async () => {
    installFetchMock({
      'GET /api/v1/kyc/status': { body: { tier: 'T0', status: 'PENDING', documents: [] } },
      'GET /api/v1/kyc/requirements': {
        status: 503,
        body: { type: 'error', code: 'UNAVAILABLE', message: 'down' },
      },
    });
    renderKyc();
    const user = userEvent.setup();
    await user.type(await screen.findByLabelText(/First name/), 'A');
    await user.type(screen.getByLabelText(/Last name/), 'B');
    await user.type(screen.getByLabelText(/Date of birth/), '1990-01-01');
    await user.type(screen.getByLabelText(/Nationality/), 'CH');
    await user.type(screen.getByLabelText(/Residential address/), 'X');
    await user.click(screen.getByRole('button', { name: 'Next' }));
    expect(
      await screen.findByText(/requirements service is unavailable — showing the default/),
    ).toBeInTheDocument();
    expect(await screen.findByLabelText(/Government-issued ID/)).toBeInTheDocument();
  });
});

describe('validateDocFile', () => {
  it('enforces type + ≤10 MB', () => {
    expect(validateDocFile({ name: 'a.png', size: 1024, type: 'image/png' })).toBeNull();
    expect(validateDocFile({ name: 'a.gif', size: 1024, type: 'image/gif' })).toMatch(/JPEG/);
    expect(validateDocFile({ name: 'a.png', size: 11 * 1024 * 1024, type: 'image/png' })).toMatch(
      /10 MB/,
    );
  });
  it('tier limits table matches canonical values', () => {
    expect(TIER_LIMITS.T1.withdrawal).toContain('$10,000');
    expect(TIER_LIMITS.T2.withdrawal).toContain('$100,000');
    expect(TIER_LIMITS.T2.reverify).toContain('12 months');
    expect(TIER_LIMITS.INSTITUTIONAL.reverify).toContain('24 months');
  });
});
