import { describe, expect, it, vi } from 'vitest';

import { chordOf, ShortcutRegistry } from './shortcuts';

function ev(
  key: string,
  mods: Partial<KeyboardEvent> = {},
  target: EventTarget | null = null,
): KeyboardEvent {
  const e = new KeyboardEvent('keydown', { key, ...mods });
  Object.defineProperty(e, 'target', { value: target });
  return e;
}

const inputEl = () => document.createElement('input');

describe('chordOf', () => {
  it('normalizes modifiers', () => {
    expect(chordOf({ key: 'Enter', ctrlKey: true, metaKey: false, altKey: false })).toBe(
      'ctrl+enter',
    );
    expect(chordOf({ key: 's', ctrlKey: true, metaKey: false, altKey: false })).toBe('ctrl+s');
    expect(chordOf({ key: 'Escape', ctrlKey: false, metaKey: false, altKey: false })).toBe(
      'escape',
    );
    expect(chordOf({ key: '/', ctrlKey: false, metaKey: false, altKey: false })).toBe('/');
  });
});

describe('ShortcutRegistry', () => {
  it('dispatches a registered binding in an active scope', () => {
    const r = new ShortcutRegistry();
    const action = vi.fn();
    r.register({
      id: 'order.submit',
      key: 'ctrl+enter',
      description: 'submit',
      scope: 'trading',
      action,
    });
    expect(r.dispatch(ev('Enter', { ctrlKey: true }, inputEl()))).toBe(true);
    expect(action).toHaveBeenCalled();
  });

  it('blocks plain keys from editable targets but allows chords/Escape', () => {
    const r = new ShortcutRegistry();
    const slash = vi.fn();
    const esc = vi.fn();
    r.register({ id: 'search', key: '/', description: 'search', scope: 'global', action: slash });
    r.register({
      id: 'cancel',
      key: 'escape',
      description: 'cancel',
      scope: 'global',
      action: esc,
    });
    expect(r.dispatch(ev('/', {}, inputEl()))).toBe(false);
    expect(slash).not.toHaveBeenCalled();
    expect(r.dispatch(ev('/', {}, null))).toBe(true);
    expect(slash).toHaveBeenCalled();
    expect(r.dispatch(ev('Escape', {}, inputEl()))).toBe(true);
    expect(esc).toHaveBeenCalled();
  });

  it('scope gating: inactive-scope bindings never fire', () => {
    const r = new ShortcutRegistry();
    const admin = vi.fn();
    r.register({
      id: 'admin.freeze',
      key: 'ctrl+f',
      description: 'freeze',
      scope: 'admin',
      action: admin,
    });
    r.setScopes(['trading']);
    expect(r.dispatch(ev('f', { ctrlKey: true }))).toBe(false);
    r.setScopes(['trading', 'admin']);
    expect(r.dispatch(ev('f', { ctrlKey: true }))).toBe(true);
  });

  it('LITE mode suppresses non-lite bindings', () => {
    const r = new ShortcutRegistry();
    const pro = vi.fn();
    const lite = vi.fn();
    r.register({ id: 'p', key: 'ctrl+b', description: 'buy', scope: 'trading', action: pro });
    r.register({
      id: 'l',
      key: 'ctrl+enter',
      description: 'submit',
      scope: 'trading',
      lite: true,
      action: lite,
    });
    r.setMode('LITE');
    expect(r.dispatch(ev('b', { ctrlKey: true }))).toBe(false);
    expect(r.dispatch(ev('Enter', { ctrlKey: true }))).toBe(true);
    expect(r.list().map((b) => b.id)).toEqual(['l']);
  });

  it('rebind replaces the key', () => {
    const r = new ShortcutRegistry();
    const action = vi.fn();
    r.register({ id: 'x', key: 'ctrl+x', description: 'x', scope: 'trading', action });
    r.rebind('x', 'ctrl+shift+x'.replace('+shift', '')); // → ctrl+x stays…
    r.rebind('x', 'ctrl+y');
    expect(r.dispatch(ev('x', { ctrlKey: true }))).toBe(false);
    expect(r.dispatch(ev('y', { ctrlKey: true }))).toBe(true);
  });

  it("'?' toggles the help overlay (except while typing)", () => {
    const r = new ShortcutRegistry();
    expect(r.helpOpen).toBe(false);
    r.dispatch(ev('?'));
    expect(r.helpOpen).toBe(true);
    r.dispatch(ev('?', {}, inputEl()));
    expect(r.helpOpen).toBe(true); // not toggled while typing
    r.dispatch(ev('?'));
    expect(r.helpOpen).toBe(false);
  });

  it('unsubscribe removes the binding', () => {
    const r = new ShortcutRegistry();
    const action = vi.fn();
    const off = r.register({ id: 't', key: 'ctrl+t', description: 't', scope: 'trading', action });
    off();
    expect(r.dispatch(ev('t', { ctrlKey: true }))).toBe(false);
  });
});
