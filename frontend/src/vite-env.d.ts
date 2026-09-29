/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** Override REST base URL; defaults to same-origin `/api/v1`. */
  readonly VITE_API_URL?: string;
  /** Override WS endpoint; defaults to same-origin `/ws/v1` (ws:/wss: by page scheme). */
  readonly VITE_WS_URL?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
