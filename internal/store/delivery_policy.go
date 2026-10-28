package store

import "time"

// DeliveryPolicy is immutable while workers run. Configure it before any scans
// or claims; a zero policy preserves the low-level store's unrestricted behavior.
type DeliveryPolicy struct {
	Since  time.Time
	Replay bool
	Debug  bool
}

func (s *Store) SetDeliveryPolicy(policy DeliveryPolicy) { s.delivery = policy }

func (s *Store) beforeDelivery(at time.Time) bool {
	return !s.delivery.Since.IsZero() && at.Before(s.delivery.Since)
}

func (s *Store) deliveryMillis() int64 {
	if s.delivery.Since.IsZero() {
		return -1 << 63
	}
	// Conservative millisecond boundary; see tibo-architecture.
	ms := s.delivery.Since.UnixMilli()
	if time.UnixMilli(ms).Before(s.delivery.Since) {
		ms++
	}
	return ms
}
