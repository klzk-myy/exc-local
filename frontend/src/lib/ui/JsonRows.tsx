/**
 * Verbatim JSON rows for opaque/audit payloads — surfaces where the
 * backend shape is displayed honestly rather than guessed (audit trails,
 * decorated views, regreport detail). Scrollable region carries
 * tabIndex for keyboard access (axe scrollable-region-focusable).
 */
export function JsonRows({ rows, empty = 'No rows.' }: { rows: unknown[]; empty?: string }) {
  return (
    <ul
      className="max-h-64 overflow-y-auto rounded border border-neutral-800 p-2 font-mono text-xs text-neutral-300"
      tabIndex={0}
    >
      {rows.map((row, i) => (
        <li key={i} className="py-0.5">
          {JSON.stringify(row)}
        </li>
      ))}
      {rows.length === 0 && <li className="text-neutral-500">{empty}</li>}
    </ul>
  );
}
