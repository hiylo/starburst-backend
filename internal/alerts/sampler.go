package alerts

// Metrics is a snapshot of host resource usage, expressed as percentages
// (0-100). A zero value for a metric means it could not be measured yet (e.g.
// the CPU sampler needs two samples to compute a delta).
type Metrics struct {
	CPU  float64 `json:"cpuPct"`
	Mem  float64 `json:"memPct"`
	Disk float64 `json:"diskPct"`
}

// Sampler reads host resource usage. Implementations are platform-specific so
// the monitor stays testable and the binary keeps building off Linux.
type Sampler interface {
	Sample() Metrics
}

// NewSampler returns the platform-specific sampler for the current OS.
func NewSampler() Sampler {
	return newPlatformSampler()
}
