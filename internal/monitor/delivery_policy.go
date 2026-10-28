package monitor

import (
	"time"

	"tibo-monitor/internal/config"
	"tibo-monitor/internal/store"
)

func (m *Monitor) initDelivery(now time.Time) {
	m.deliveryOnce.Do(func() {
		if m.StartedAt.IsZero() {
			m.StartedAt = now.UTC()
		}
		m.Store.SetDeliveryPolicy(store.DeliveryPolicy{Since: m.deliverySince(), Replay: !m.SendSince.IsZero(), Debug: m.Debug})
	})
}

func (m *Monitor) deliverySince() time.Time {
	if !m.SendSince.IsZero() {
		return m.SendSince
	}
	return m.StartedAt
}

func (m *Monitor) scanCutoff(now time.Time) time.Time {
	if !m.SendSince.IsZero() {
		return m.SendSince
	}
	return now.Add(-m.Config.Poll.CatchUpWindow)
}

// First due instant for this date; DST behavior is specified in tibo-architecture.
func dailyScheduledAt(now time.Time, cfg config.DailyReport) (time.Time, error) {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	clock, err := time.Parse("15:04", cfg.Time)
	if err != nil {
		return time.Time{}, err
	}
	local := now.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	wanted := clock.Hour()*60 + clock.Minute()
	for at := day.Add(-24 * time.Hour); at.Before(day.Add(48 * time.Hour)); at = at.Add(time.Minute) {
		wall := at.In(loc)
		if wall.Year() == local.Year() && wall.YearDay() == local.YearDay() && wall.Hour()*60+wall.Minute() >= wanted {
			return at, nil
		}
	}
	return time.Time{}, nil
}
