package nteedb

import (
	"errors"
	"time"
)

// ErrInvalidTTL is returned when an optional ttl argument is non-positive or
// given more than once.
var ErrInvalidTTL = errors.New("nteedb: ttl must be a single positive duration")

// nowMillis is the wall clock for TTL decisions (the store's only time
// dependency). A package variable so tests can inject a deterministic clock.
var nowMillis = func() int64 { return time.Now().UnixMilli() }

// resolveTTL turns an optional variadic ttl into an absolute unix-ms expiry
// stamp (0 = no TTL). At most one positive duration is accepted.
func resolveTTL(ttl []time.Duration) (int64, error) {
	if len(ttl) == 0 {
		return 0, nil
	}
	if len(ttl) > 1 || ttl[0] <= 0 {
		return 0, ErrInvalidTTL
	}
	return nowMillis() + ttl[0].Milliseconds(), nil
}
