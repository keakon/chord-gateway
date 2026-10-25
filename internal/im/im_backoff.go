package im

import (
	"math/rand/v2"
	"time"
)

// JitterBackoff shares the calculation, not platform-specific retry and reset policies.
func JitterBackoff(delay time.Duration) time.Duration {
	return time.Duration(float64(delay) * (0.8 + rand.Float64()*0.4))
}
func ReconnectBackoff(attempt int) time.Duration {
	return JitterBackoff(min(time.Second<<min(max(attempt, 0), 5), 30*time.Second))
}
