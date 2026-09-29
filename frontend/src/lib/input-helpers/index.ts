/**
 * Centralized input-helper framework (Phase-10 Task 10.3.29).
 * Barrel — every surface imports from '@/lib/input-helpers'.
 */
export * from './decimal';
export * from './instruments';
export * from './validation';
export { useInputHelper, type InputHelper } from './useInputHelper';
export * from './format';
export {
  previewOrder,
  previewFunding,
  deriveGrid,
  debounce,
  type OrderPreviewRequest,
  type OrderPreviewResult,
  type FundingEstimate,
  type GridDerivation,
} from './preview';
export {
  PreviewPanel,
  useOrderPreview,
  severityForRisk,
  type OrderPreviewState,
} from './PreviewPanel';
export * from './defaults';
export {
  Autocomplete,
  SymbolAutocomplete,
  AmountPresets,
  fuzzyScore,
  fuzzyRank,
  pairCategory,
  presetAmount,
  AMOUNT_PRESETS,
  type AutocompleteProps,
} from './autocomplete';
export {
  ShortcutRegistry,
  shortcutRegistry,
  chordOf,
  useShortcuts,
  useShortcutListener,
  type ShortcutBinding,
  type UiMode,
} from './shortcuts';
export { ShortcutHelpOverlay } from './shortcuts';
export {
  HELP_REGISTRY,
  GLOSSARY,
  fieldHelp,
  HelpTooltip,
  GlossaryList,
  type FieldHelp,
  type GlossaryTerm,
} from './help';
export {
  ConfirmModal,
  useConfirmState,
  CONFIRM_PHRASES,
  RISK_DISCLOSURES,
  type ConfirmSeverity,
  type ConfirmModalProps,
  type RiskDisclosureKey,
  type PendingConfirm,
} from './confirm';
export {
  parseCsvOrders,
  parseBeneficiaryLines,
  parseScaledLadder,
  splitCsvLine,
  csvOrderNotional,
  CSV_ORDER_COLUMNS,
  type BulkRow,
  type BulkParseResult,
  type CsvOrder,
  type BeneficiaryRow,
  type ScaledLeg,
} from './bulk';
export * from './convert';
export { isNotImplemented, isBackendStub, UnavailablePanel } from './availability';
export { InputField, useValidatedField, type InputFieldProps, type ValidatedField } from './fields';
export { downloadFile, saveBlob, type DownloadResult } from './download';
export {
  ROUTE_CONTRACTS,
  ROUTE_KEYS,
  routeContract,
  type RouteContract,
  type RouteParamSchema,
} from './generated/route-contracts';
