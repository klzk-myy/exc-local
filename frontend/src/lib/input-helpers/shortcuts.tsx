/**
 * Keyboard shortcut manager (Task 10.3.29 item 6).
 *
 * Scope-aware global registry — trading/admin/funding scopes plus the
 * `?` help overlay. Canonical bindings (task text):
 *   Ctrl+Enter submit · Esc cancel/close · Ctrl+D dead-man toggle ·
 *   Ctrl+B buy · Ctrl+S sell · Ctrl+W close-all (confirmed) ·
 *   1–9 workspace panel focus · `/` symbol search · `?` help overlay
 *
 * LITE mode (Task 10.3.9) exposes only Ctrl+Enter, Esc, `/`.
 * Shortcuts are configurable — `rebind` replaces a binding's key.
 * Focus guard: plain keys never fire while typing in a field; only
 * chords (Ctrl/Alt/Meta) and Escape do.
 */
import { useEffect, useMemo, useState, type ReactNode } from 'react';

export interface ShortcutBinding {
  /** Stable id ('order.submit', 'palette.focus', …). */
  id: string;
  /** Normalized chord: 'ctrl+enter', 'escape', '/', '1'..'9'. */
  key: string;
  description: string;
  /** 'trading' | 'admin' | 'funding' | 'global' — dispatch skips
   * shortcuts whose scope isn't in the active scope set. */
  scope: string;
  /** LITE-mode surfaces expose only lite:true bindings. */
  lite?: boolean;
  action: () => void;
}

export type UiMode = 'LITE' | 'PRO';

function isEditableTarget(t: EventTarget | null): boolean {
  if (!(t instanceof HTMLElement)) return false;
  const tag = t.tagName;
  return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || t.isContentEditable;
}

/** Normalize a KeyboardEvent into a chord token like 'ctrl+enter'. */
export function chordOf(
  e: KeyboardEvent | { key: string; ctrlKey: boolean; metaKey: boolean; altKey: boolean },
): string {
  const parts: string[] = [];
  if (e.ctrlKey || e.metaKey) parts.push('ctrl');
  if (e.altKey) parts.push('alt');
  parts.push(e.key.toLowerCase());
  return parts.join('+');
}

export class ShortcutRegistry {
  private readonly bindings = new Map<string, ShortcutBinding>();
  private readonly listeners = new Set<() => void>();
  private scopes = new Set<string>(['global', 'trading']);
  private mode: UiMode = 'PRO';
  private detach: (() => void) | null = null;
  private helpVisible = false;
  private readonly helpListeners = new Set<() => void>();

  register(b: ShortcutBinding): () => void {
    this.bindings.set(b.id, b);
    this.emit();
    return () => {
      this.bindings.delete(b.id);
      this.emit();
    };
  }

  unregister(id: string): void {
    if (this.bindings.delete(id)) this.emit();
  }

  rebind(id: string, key: string): void {
    const b = this.bindings.get(id);
    if (b) {
      this.bindings.set(id, { ...b, key: key.toLowerCase() });
      this.emit();
    }
  }

  setScopes(scopes: readonly string[]): void {
    this.scopes = new Set(['global', ...scopes]);
  }

  setMode(mode: UiMode): void {
    this.mode = mode;
  }

  list(): ShortcutBinding[] {
    return [...this.bindings.values()].filter((b) => this.mode === 'PRO' || b.lite === true);
  }

  /** Resolve a chord to a binding in an active scope. */
  resolve(chord: string, editableFocus: boolean): ShortcutBinding | undefined {
    for (const b of this.bindings.values()) {
      if (!this.scopes.has(b.scope)) continue;
      if (this.mode === 'LITE' && b.lite !== true) continue;
      if (b.key !== chord) continue;
      // Plain keys (no modifier) never fire from editable targets —
      // Escape and chorded keys are exempt.
      const chorded = chord.includes('+') || chord === 'escape';
      if (editableFocus && !chorded) continue;
      return b;
    }
    return undefined;
  }

  /** Dispatch a raw keyboard event. Returns true when consumed. */
  dispatch(e: KeyboardEvent): boolean {
    const chord = chordOf(e);
    const editable = isEditableTarget(e.target);
    // '?' help overlay — never steal the character while typing.
    if (chord === '?' && !editable) {
      this.toggleHelp();
      e.preventDefault();
      return true;
    }
    const b = this.resolve(chord, editable);
    if (!b) return false;
    e.preventDefault();
    b.action();
    return true;
  }

  /** Attach to a DOM event target (window for the global registry). */
  attach(target: {
    addEventListener: (t: string, f: (e: Event) => void) => void;
    removeEventListener: (t: string, f: (e: Event) => void) => void;
  }): () => void {
    const handler = (e: Event) => {
      if (e instanceof KeyboardEvent || 'key' in e) {
        this.dispatch(e as KeyboardEvent);
      }
    };
    target.addEventListener('keydown', handler);
    this.detach = () => {
      target.removeEventListener('keydown', handler);
    };
    return this.detach;
  }

  onChange(fn: () => void): () => void {
    this.listeners.add(fn);
    return () => {
      this.listeners.delete(fn);
    };
  }

  // Help overlay ('?') — the registry owns its visibility so every
  // surface shares one overlay instance.
  get helpOpen(): boolean {
    return this.helpVisible;
  }
  toggleHelp(): void {
    this.helpVisible = !this.helpVisible;
    for (const fn of this.helpListeners) fn();
  }
  setHelp(open: boolean): void {
    this.helpVisible = open;
    for (const fn of this.helpListeners) fn();
  }
  onHelpChange(fn: () => void): () => void {
    this.helpListeners.add(fn);
    return () => {
      this.helpListeners.delete(fn);
    };
  }

  private emit(): void {
    for (const fn of this.listeners) fn();
  }
}

/** App-wide shared registry. */
export const shortcutRegistry = new ShortcutRegistry();

/** React: register bindings for the component's lifetime. Re-register
 * when any id:key pair changes (bindings are cheap value objects). */
export function useShortcuts(bindings: readonly ShortcutBinding[]): void {
  const depsKey = bindings.map((b) => `${b.id}:${b.key}`).join('|');
  useEffect(() => {
    const offs = bindings.map((b) => shortcutRegistry.register(b));
    return () => {
      for (const off of offs) off();
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [depsKey]);
}

/** Mount the registry's global keydown listener once. */
export function useShortcutListener(target: Window | Document = window): void {
  useEffect(() => shortcutRegistry.attach(target), [target]);
}

// ---------------------------------------------------------------------------
// '?' help overlay — discoverable surface for every registered binding
// ---------------------------------------------------------------------------

function useRegistryVersion(): ShortcutBinding[] {
  const [, setTick] = useState(0);
  useEffect(
    () =>
      shortcutRegistry.onChange(() => {
        setTick((t) => t + 1);
      }),
    [],
  );
  return shortcutRegistry.list();
}

function useHelpOpen(): boolean {
  const [open, setOpen] = useState(shortcutRegistry.helpOpen);
  useEffect(
    () =>
      shortcutRegistry.onHelpChange(() => {
        setOpen(shortcutRegistry.helpOpen);
      }),
    [],
  );
  return open;
}

export function ShortcutHelpOverlay(): ReactNode {
  const open = useHelpOpen();
  const bindings = useRegistryVersion();
  const grouped = useMemo(() => {
    const m = new Map<string, ShortcutBinding[]>();
    for (const b of bindings) {
      const l = m.get(b.scope) ?? [];
      l.push(b);
      m.set(b.scope, l);
    }
    return m;
  }, [bindings]);

  if (!open) return null;
  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
      role="presentation"
      onClick={() => {
        shortcutRegistry.setHelp(false);
      }}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-label="Keyboard shortcuts"
        className="w-full max-w-lg rounded-lg border border-neutral-700 bg-neutral-900 p-5"
      >
        <h2 className="mb-3 text-lg font-semibold text-neutral-100">Keyboard shortcuts</h2>
        {bindings.length === 0 && (
          <p className="text-sm text-neutral-500">No shortcuts registered on this surface.</p>
        )}
        {[...grouped.entries()].map(([scope, list]) => (
          <section key={scope} className="mb-3">
            <h3 className="mb-1 text-xs font-semibold uppercase tracking-wide text-neutral-500">
              {scope}
            </h3>
            <ul className="space-y-0.5">
              {list.map((b) => (
                <li key={b.id} className="flex items-center justify-between text-sm">
                  <span className="text-neutral-300">{b.description}</span>
                  <kbd className="rounded border border-neutral-700 bg-neutral-950 px-1.5 py-0.5 font-mono text-xs text-neutral-300">
                    {b.key}
                  </kbd>
                </li>
              ))}
            </ul>
          </section>
        ))}
        <p className="mt-3 text-xs text-neutral-500">
          Press <kbd className="font-mono">?</kbd> or click anywhere to close. Shortcuts are
          configurable in workspace preferences.
        </p>
      </div>
    </div>
  );
}
