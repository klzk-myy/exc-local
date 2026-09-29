import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { ApiError } from '@/lib/api';

import { isNotImplemented, UnavailablePanel } from './availability';

describe('isNotImplemented', () => {
  it('detects the gateway 501 envelope', () => {
    const err = new ApiError({
      type: 'error',
      error: 'NOT_IMPLEMENTED',
      message: 'route registered, handler pending',
      status: 501,
    });
    expect(isNotImplemented(err)).toBe(true);
  });
  it('also catches a 501 carrying a different code', () => {
    const err = new ApiError({ type: 'error', error: 'INTERNAL_ERROR', message: 'x', status: 501 });
    expect(isNotImplemented(err)).toBe(true);
  });
  it('rejects ordinary errors', () => {
    expect(isNotImplemented(new Error('x'))).toBe(false);
    expect(
      isNotImplemented(
        new ApiError({ type: 'error', error: 'INVALID_REQUEST', message: 'x', status: 400 }),
      ),
    ).toBe(false);
  });
});

describe('<UnavailablePanel>', () => {
  it('names the feature and owning phase — never fabricates data', () => {
    render(<UnavailablePanel feature="Copy trading" owner="Phase-14 Task 14.3.14" />);
    expect(screen.getByRole('status')).toHaveTextContent('Copy trading');
    expect(screen.getByRole('status')).toHaveTextContent('Phase-14 Task 14.3.14');
  });
});
