import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { ConfirmModal, CONFIRM_PHRASES, RISK_DISCLOSURES } from './confirm';

const base = {
  open: true,
  title: 'Confirm action',
  onConfirm: vi.fn(),
  onCancel: vi.fn(),
};

describe('<ConfirmModal>', () => {
  it('renders nothing when closed', () => {
    render(<ConfirmModal {...base} open={false} severity="LOW" />);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('LOW severity confirms on a single click', () => {
    const onConfirm = vi.fn();
    render(<ConfirmModal {...base} onConfirm={onConfirm} severity="LOW" />);
    fireEvent.click(screen.getByRole('button', { name: 'Confirm' }));
    expect(onConfirm).toHaveBeenCalled();
  });

  it('injects registered disclosures verbatim', () => {
    render(<ConfirmModal {...base} severity="MEDIUM" disclosures={['gridBot', 'capitalLoss']} />);
    expect(screen.getByText(RISK_DISCLOSURES.gridBot)).toBeInTheDocument();
    expect(screen.getByText(RISK_DISCLOSURES.capitalLoss)).toBeInTheDocument();
  });

  it('HIGH severity requires typing the phrase', () => {
    const onConfirm = vi.fn();
    render(
      <ConfirmModal
        {...base}
        onConfirm={onConfirm}
        severity="HIGH"
        requirePhrase={CONFIRM_PHRASES['closeAllPositions']}
      />,
    );
    const btn = screen.getByRole('button', { name: 'Confirm' });
    expect(btn).toBeDisabled();
    fireEvent.change(screen.getByLabelText(/to confirm/i), {
      target: { value: 'CLOSE ALL' },
    });
    expect(btn).toBeEnabled();
    fireEvent.click(btn);
    expect(onConfirm).toHaveBeenCalled();
  });

  it('dual-control renders the pending-approval notice and label', () => {
    render(<ConfirmModal {...base} severity="MEDIUM" dualControl />);
    expect(screen.getByText(/second approver/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Submit for approval' })).toBeInTheDocument();
  });

  it('Escape and Cancel invoke onCancel', () => {
    const onCancel = vi.fn();
    render(<ConfirmModal {...base} onCancel={onCancel} severity="LOW" />);
    fireEvent.keyDown(screen.getByRole('dialog').parentElement!, { key: 'Escape' });
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onCancel).toHaveBeenCalledTimes(2);
  });
});
