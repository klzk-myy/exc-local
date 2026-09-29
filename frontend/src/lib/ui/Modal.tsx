/**
 * Modal — accessible confirmation/dialog primitive (Esc + backdrop
 * dismiss, `role="dialog"`, focus-safe for the destructive confirmations
 * that Tasks 10.3.21/10.3.22 require — session revoke, emergency freeze,
 * account closure, GDPR erasure).
 */
import { useEffect, useRef, type ReactNode } from 'react';
import { createPortal } from 'react-dom';

import { btnDanger, btnGhost, btnPrimary } from './classes';

export interface ModalProps {
  open: boolean;
  title: string;
  onClose: () => void;
  children: ReactNode;
}

export function Modal({ open, title, onClose, children }: ModalProps) {
  const ref = useRef<HTMLDivElement>(null);
  // onClose is typically a fresh arrow each parent render — keep it in a
  // ref so the effect below only re-runs on `open` transitions. Without
  // this, every keystroke in a modal form steals focus back to the first
  // button (re-render → effect → focus()).
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onCloseRef.current();
    };
    document.addEventListener('keydown', onKey);
    ref.current?.querySelector('button')?.focus();
    return () => {
      document.removeEventListener('keydown', onKey);
    };
  }, [open]);

  if (!open) return null;
  return createPortal(
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        ref={ref}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        className="w-full max-w-md rounded-lg border border-neutral-700 bg-neutral-900 p-5 shadow-xl"
      >
        <h2 className="mb-3 text-lg font-semibold text-neutral-100">{title}</h2>
        {children}
      </div>
    </div>,
    document.body,
  );
}

/** Standard destructive-confirmation body: message + Cancel/Confirm. */
export function ConfirmAction({
  message,
  confirmLabel,
  busy,
  danger = true,
  onConfirm,
  onCancel,
}: {
  message: ReactNode;
  confirmLabel: string;
  busy?: boolean;
  danger?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  return (
    <div>
      <div className="mb-4 text-sm text-neutral-300">{message}</div>
      <div className="flex justify-end gap-2">
        <button type="button" className={btnGhost} onClick={onCancel} disabled={busy === true}>
          Cancel
        </button>
        <button
          type="button"
          className={danger ? btnDanger : btnPrimary}
          onClick={onConfirm}
          disabled={busy === true}
        >
          {busy === true ? 'Working…' : confirmLabel}
        </button>
      </div>
    </div>
  );
}
