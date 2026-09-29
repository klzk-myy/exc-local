/**
 * React bindings for the WS client — Task 10.3.1 item 3 / Task 10.3.19.
 * `useWsStatus` is the single status surface: connection state, order-entry
 * lock, per-subscription staleness. `useChannel` wires a handler to the
 * subscription lifecycle for feature components.
 */
import { useEffect, useRef, useSyncExternalStore } from 'react';

import type { WsClient, WsClientStatus } from './client';
import type { EventFrame, SnapshotFrame } from './protocol';

export function useWsStatus(client: WsClient): WsClientStatus {
  return useSyncExternalStore(
    (cb) => client.onStatusChange(() => cb()),
    () => client.getStatus(),
    () => client.getStatus(),
  );
}

/**
 * Subscribe `channel` for the lifetime of the component; `onMessage` is
 * ref-stable (latest closure wins, no re-subscribe on render).
 */
export function useChannel(
  client: WsClient,
  channel: string | null,
  onMessage: (frame: EventFrame | SnapshotFrame) => void,
): void {
  const handlerRef = useRef(onMessage);
  handlerRef.current = onMessage;
  useEffect(() => {
    if (channel === null) return;
    return client.subscribe(channel, (f) => {
      handlerRef.current(f);
    });
  }, [client, channel]);
}
