package wire

// KindSealStatus is the kind of an OpenBao Transit seal status report (Decision
// 0013).
const KindSealStatus = "seal.status"

// SealStatusView is the transit seal's posture: whether the wrapped-DEK blob
// exists on disk, and whether the environment carries enough to reach a
// transit key. The token NEVER travels — SealAddress and SealKeyName are
// identifiers, exactly what the text report already prints; EnvError carries
// only the resolution failure's message, never the partial env values that
// caused it.
type SealStatusView struct {
	TransitFilePresent bool   `json:"transit_file_present"`
	EnvConfigured      bool   `json:"env_configured"`
	EnvError           string `json:"env_error,omitempty"`
	SealAddress        string `json:"seal_address,omitempty"`
	SealKeyName        string `json:"seal_key_name,omitempty"`
	OrphanBlob         bool   `json:"orphan_blob,omitempty"`
}
