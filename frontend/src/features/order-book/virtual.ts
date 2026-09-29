/**
 * Hand-rolled virtualization (Task 10.3.2 item 1). No react-window dep
 * in the project — fixed-height rows make windowing a pure index
 * computation, so the math lives here and is unit-tested against
 * boundary cases (empty, partial page, mid/end scroll, overscan).
 */

export interface VirtualRange {
  /** First rendered row index (inclusive). */
  start: number;
  /** One past the last rendered row index. */
  end: number;
  /** Total row count. */
  count: number;
  /** Spacer heights preserving scroll geometry. */
  topPad: number;
  bottomPad: number;
}

export interface VirtualOptions {
  count: number;
  rowHeight: number;
  /** Current scrollTop of the viewport (px). */
  scrollTop: number;
  /** Viewport height (px). */
  viewportHeight: number;
  /** Extra rows rendered outside the viewport on each side. */
  overscan?: number;
}

export function visibleRange(o: VirtualOptions): VirtualRange {
  const { count, rowHeight, viewportHeight } = o;
  const overscan = Math.max(0, o.overscan ?? 2);
  if (count <= 0 || rowHeight <= 0 || viewportHeight <= 0) {
    return { start: 0, end: 0, count, topPad: 0, bottomPad: 0 };
  }
  const scrollTop = Math.max(0, Math.min(o.scrollTop, count * rowHeight));
  const first = Math.floor(scrollTop / rowHeight);
  const last = Math.min(count, Math.ceil((scrollTop + viewportHeight) / rowHeight));
  const start = Math.max(0, first - overscan);
  const end = Math.min(count, last + overscan);
  return {
    start,
    end,
    count,
    topPad: start * rowHeight,
    bottomPad: (count - end) * rowHeight,
  };
}
