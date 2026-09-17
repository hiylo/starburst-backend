//go:build !linux

package alerts

// stubSampler returns zero metrics on platforms where /proc is unavailable.
type stubSampler struct{}

func newPlatformSampler() Sampler { return stubSampler{} }

func (stubSampler) Sample() Metrics { return Metrics{} }
