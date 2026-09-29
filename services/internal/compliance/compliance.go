// Package compliance holds AML/KYC/surveillance logic.
// PHASE-12 (Tasks 12.3.4/12.3.13) owns the KYC SUBMISSION intake +
// operations matrix implemented in kyc.go / upload.go / store.go /
// taxcerts.go. PHASE-14 owns the review lifecycle (approve/reject,
// auto-downgrade, re-verification enforcement); PHASE-17: surveillance
// signal emission; PHASE-21: alerts, enforcement and regulatory
// reporting (MiFID II, EMIR, Dodd-Frank, FinCEN, CRS/FATCA).
package compliance
