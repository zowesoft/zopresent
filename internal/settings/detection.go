package settings

// Auto-projection keys. The switch is the operator's standing choice and is off
// until they turn it on; the pause is what F9 toggles, and the app writes it
// back to "true" at every start so nothing projects before someone is watching.
const (
	KeyAutoProjectionEnabled = "auto_projection_enabled"
	KeyAutoProjectionPaused  = "auto_projection_paused"
)

// Per-type enablement. Explicit references are the reliable baseline and ship
// enabled; every other type stays off until the operator has calibrated it on
// labeled audio (cmd/asr-bench prints each type's sweep and recommendation) and
// turns it on. Multiple references ride the explicit lane — several passages in
// one breath are several explicit cards — so they follow its switch.
const (
	KeyDetectionTypeExplicit   = "detection_type_explicit_enabled"
	KeyDetectionTypeContextual = "detection_type_contextual_enabled"
	KeyDetectionTypeQuote      = "detection_type_quote_enabled"
	KeyDetectionTypeCorrection = "detection_type_correction_enabled"
)
