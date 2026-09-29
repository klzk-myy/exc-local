/**
 * Test ports for the WS client — a FakeClock driving every timer slot and
 * a MockSocket recording outbound frames. Lives under src/ (not test/) so
 * feature-level tests in Wave-2 can reuse the kit.
 */
import type { Clock, TimerHandle, WsSocketLike } from './client';

export class FakeClock implements Clock {
  private t: number;
  private nextId = 1;
  private timers: { id: number; at: number; fn: () => void }[] = [];

  constructor(startMs = 1_700_000_000_000) {
    this.t = startMs;
  }

  now(): number {
    return this.t;
  }

  setTimeout(fn: () => void, ms: number): TimerHandle {
    const id = this.nextId++;
    this.timers.push({ id, at: this.t + ms, fn });
    return id;
  }

  clearTimeout(h: TimerHandle): void {
    this.timers = this.timers.filter((x) => x.id !== h);
  }

  /** Advance time, firing every timer whose deadline falls inside. */
  advance(ms: number): void {
    const target = this.t + ms;
    for (;;) {
      const due = this.timers
        .filter((x) => x.at <= target)
        .sort((a, b) => a.at - b.at || a.id - b.id)[0];
      if (!due) break;
      this.t = due.at;
      this.timers = this.timers.filter((x) => x.id !== due.id);
      due.fn();
    }
    this.t = target;
  }
}

export class MockSocket implements WsSocketLike {
  readyState = 0;
  onopen: (() => void) | null = null;
  onclose: ((ev: { code: number; reason: string }) => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((ev: { data: string }) => void) | null = null;

  readonly sent: string[] = [];
  closed = false;

  constructor(readonly url: string) {}

  send(data: string): void {
    this.sent.push(data);
  }

  close(code = 1000, reason = ''): void {
    if (this.closed) return;
    this.closed = true;
    this.readyState = 3;
    this.onclose?.({ code, reason });
  }

  // -- test-side helpers ---------------------------------------------------

  /** Server accepts the TCP/WS upgrade. */
  open(): void {
    this.readyState = 1;
    this.onopen?.();
  }

  /** Deliver one inbound frame (object → JSON, or raw text). */
  recv(frame: object | string): void {
    this.onmessage?.({ data: typeof frame === 'string' ? frame : JSON.stringify(frame) });
  }

  /** Server-side close (default 1006 abnormal — never a clean code). */
  serverClose(code = 1006, reason = ''): void {
    this.closed = true;
    this.readyState = 3;
    this.onclose?.({ code, reason });
  }

  /** All outbound frames parsed as JSON for assertion convenience. */
  sentFrames(): Record<string, unknown>[] {
    return this.sent.map((s) => JSON.parse(s) as Record<string, unknown>);
  }

  lastFrame(): Record<string, unknown> | undefined {
    return this.sentFrames().at(-1);
  }
}

export class SocketFactory {
  readonly sockets: MockSocket[] = [];
  make = (url: string): MockSocket => {
    const s = new MockSocket(url);
    this.sockets.push(s);
    return s;
  };
  latest(): MockSocket {
    const s = this.sockets.at(-1);
    if (!s) throw new Error('no socket dialed yet');
    return s;
  }
}
