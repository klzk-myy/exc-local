import { describe, expect, it } from 'vitest';

import { visibleRange } from './virtual';

const ROW = 24;

describe('visibleRange', () => {
  it('top of book: window starts at 0 with overscan end', () => {
    const r = visibleRange({
      count: 20,
      rowHeight: ROW,
      scrollTop: 0,
      viewportHeight: 240,
      overscan: 2,
    });
    expect(r.start).toBe(0);
    expect(r.end).toBe(12); // ceil(240/24)=10 + 2 overscan
    expect(r.topPad).toBe(0);
    expect(r.bottomPad).toBe((20 - 12) * ROW);
  });
  it('mid-scroll windows with overscan + spacer pads', () => {
    const r = visibleRange({
      count: 100,
      rowHeight: ROW,
      scrollTop: ROW * 50,
      viewportHeight: 240,
      overscan: 3,
    });
    expect(r.start).toBe(47);
    expect(r.end).toBe(63); // ceil((1200+240)/24)=60 + 3
    expect(r.topPad).toBe(47 * ROW);
    expect(r.bottomPad).toBe((100 - 63) * ROW);
    expect(r.topPad + (r.end - r.start) * ROW + r.bottomPad).toBe(100 * ROW);
  });
  it('clamps scrollTop + end at the tail', () => {
    const r = visibleRange({ count: 15, rowHeight: ROW, scrollTop: ROW * 50, viewportHeight: 240 });
    expect(r.end).toBe(15);
    expect(r.bottomPad).toBe(0);
  });
  it('empty/degenerate inputs render nothing', () => {
    expect(visibleRange({ count: 0, rowHeight: ROW, scrollTop: 0, viewportHeight: 240 }).end).toBe(
      0,
    );
    expect(visibleRange({ count: 5, rowHeight: 0, scrollTop: 0, viewportHeight: 240 }).end).toBe(0);
    expect(visibleRange({ count: 5, rowHeight: ROW, scrollTop: 0, viewportHeight: 0 }).end).toBe(0);
  });
  it('short book fits entirely — no virtual clipping', () => {
    const r = visibleRange({ count: 5, rowHeight: ROW, scrollTop: 0, viewportHeight: 240 });
    expect(r).toMatchObject({ start: 0, end: 5, topPad: 0, bottomPad: 0 });
  });
});
