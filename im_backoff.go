package main

import (
	"math/rand/v2"
	"time"
)

// Share the calculation, not platform-specific retry and reset policies.
func jitterBackoff(delay time.Duration) time.Duration {
	return time.Duration(float64(delay) * (0.8 + rand.Float64()*0.4))
}
func reconnectBackoff(attempt int) time.Duration {
	return jitterBackoff(min(time.Second<<min(max(attempt, 0), 5), 30*time.Second))
}
