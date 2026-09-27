// Package settlement holds FX settlement logic: value-date calculation,
// T+0/T+1/T+2 cycles, holiday calendars and nostro reconciliation.
// PHASE-03 owner: settlement engine (Tasks 3.3.x); banking rails land in
// Phase-11, CLS/PvP and PB reconciliation in Phase-24.
package settlement

// Settlement cycles per spec §6.3.
const (
	CycleSameDay = "T+0" // e.g. USD/CAD, USD/MXN
	CycleT1      = "T+1" // most spot FX
	CycleT2      = "T+2" // some exotic pairs
)
