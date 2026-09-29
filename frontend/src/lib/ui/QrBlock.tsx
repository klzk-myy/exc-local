/**
 * QrBlock — inline-SVG QR code (qrcode lib's SVG output — no <canvas>, so
 * jsdom-safe). Used for TOTP otpauth URIs and deposit payment payloads.
 */
import { useEffect, useState } from 'react';
import QRCode from 'qrcode';

export function QrBlock({
  value,
  size = 180,
  label = 'QR code',
}: {
  value: string;
  size?: number;
  label?: string;
}) {
  const [svg, setSvg] = useState<string | null>(null);
  useEffect(() => {
    let live = true;
    void QRCode.toString(value, { type: 'svg', margin: 1, width: size })
      .then((s) => {
        if (live) setSvg(s);
      })
      .catch(() => {
        if (live) setSvg(null);
      });
    return () => {
      live = false;
    };
  }, [value, size]);
  if (svg === null) return null;
  return (
    <div
      className="inline-block rounded bg-white p-2"
      aria-label={label}
      role="img"
      // SVG produced locally from a known payload — not user HTML.
      dangerouslySetInnerHTML={{ __html: svg }}
    />
  );
}
