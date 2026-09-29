import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { InputField, useValidatedField, type ValidatedField } from './fields';
import { RULE_COUNTDOWN_MS, RULE_SYMBOL } from './validation';

function SymbolField({ bad = false }: { bad?: boolean }) {
  const field = useValidatedField(RULE_SYMBOL, bad ? 'eurusd' : '');
  return (
    <div>
      <InputField field={field} label="Symbol" help="BASE/QUOTE pair" unit="FX" />
      <span data-testid="valid">{String(field.valid)}</span>
    </div>
  );
}

describe('useValidatedField + <InputField>', () => {
  it('no error until touched; error appears after blur on invalid value', () => {
    render(<SymbolField bad />);
    const input = screen.getByLabelText('Symbol');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    fireEvent.blur(input);
    expect(screen.getByRole('alert')).toHaveTextContent(/invalid format/);
    expect(input.getAttribute('aria-invalid')).toBe('true');
  });

  it('reports validity independent of touched state', () => {
    render(<SymbolField bad />);
    expect(screen.getByTestId('valid')).toHaveTextContent('false');
  });

  it('clears error once a valid value is typed after touch', () => {
    render(<SymbolField bad />);
    const input = screen.getByLabelText('Symbol');
    fireEvent.blur(input);
    fireEvent.change(input, { target: { value: 'EUR/USD' } });
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByTestId('valid')).toHaveTextContent('true');
  });

  it('integer bounds rule validates countdown range', () => {
    function C() {
      const f: ValidatedField = useValidatedField(RULE_COUNTDOWN_MS, '500');
      return <InputField field={f} label="Countdown" />;
    }
    render(<C />);
    fireEvent.blur(screen.getByLabelText('Countdown'));
    expect(screen.getByRole('alert')).toHaveTextContent(/below minimum 1000/);
  });
});
