/**
 * CopyButton — clipboard copy for deposit bank details / references /
 * TOTP secrets. Graceful when the async clipboard API is absent
 * (non-secure context, permission denied) — falls back to selecting the
 * text via a transient textarea.
 */
import { useState } from 'react';

import { btnGhost } from './classes';

async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // clipboard API unavailable (permission/secure-context) — execCommand
    // fallback for older environments.
    try {
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      // eslint-disable-next-line @typescript-eslint/no-deprecated -- jsdom lacks navigator.clipboard; textarea fallback is required in tests
      const ok = document.execCommand('copy');
      ta.remove();
      return ok;
    } catch {
      return false;
    }
  }
}

export function CopyButton({ text, label = 'Copy' }: { text: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      className={btnGhost}
      onClick={() => {
        void copyText(text).then((ok) => {
          if (ok) {
            setCopied(true);
            window.setTimeout(() => {
              setCopied(false);
            }, 1500);
          }
        });
      }}
    >
      {copied ? 'Copied' : label}
    </button>
  );
}
