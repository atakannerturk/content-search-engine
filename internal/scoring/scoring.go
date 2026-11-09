// Package scoring implements the content scoring formula of the assignment.
//
//	Final = (Base * TypeCoefficient) + Freshness + Engagement
//
//	Base        video: views / 1000 + likes / 100
//	            text:  reading_time + reactions / 50
//	Coefficient video: 1.5   text: 1.0
//	Freshness   published within 7 days: +5, within 30 days: +3,
//	            within 90 days: +1, otherwise +0 (boundaries inclusive)
//	Engagement  video: (likes / views) * 10
//	            text:  (reactions / reading_time) * 5
//
// The formula is applied verbatim; every operand is converted to float64
// before any division so that integer truncation never hides a term (with
// integer arithmetic likes / views would be 0 for every realistic video).
// Divisions by a zero denominator yield an engagement of 0, and negative
// metrics are treated as 0. Nothing is rounded here; presentation layers
// round for display only.
//
// Freshness is the only time-dependent term. Score therefore also reports
// FreshnessExpiresAt, the first instant at which the bonus decreases, so the
// persisted score can be refreshed exactly when it changes.
package scoring

import (
	"math"
	"time"

	"github.com/atakannerturk/content-search-engine/internal/domain"
)

// Type coefficients.
const (
	VideoCoefficient = 1.5
	TextCoefficient  = 1.0
)

// Base score divisors.
const (
	ViewsPerPoint     = 1000.0
	LikesPerPoint     = 100.0
	ReactionsPerPoint = 50.0
)

// Engagement weights.
const (
	VideoEngagementWeight = 10.0
	TextEngagementWeight  = 5.0
)

// Freshness bonuses and the windows they apply to. "One month" and "three
// months" are interpreted as 30 and 90 days so that the windows do not depend
// on the calendar month of publication.
const (
	FreshnessWeek    = 5.0
	FreshnessMonth   = 3.0
	FreshnessQuarter = 1.0

	WeekWindow    = 7 * 24 * time.Hour
	MonthWindow   = 30 * 24 * time.Hour
	QuarterWindow = 90 * 24 * time.Hour
)

// Score applies the formula to c as of now.
func Score(c domain.Content, now time.Time) domain.ScoreBreakdown {
	freshness, expiresAt := Freshness(c.PublishedAt, now)
	b := domain.ScoreBreakdown{
		Base:               finite(Base(c)),
		TypeCoefficient:    Coefficient(c.Type),
		Freshness:          freshness,
		Engagement:         finite(Engagement(c)),
		FreshnessExpiresAt: expiresAt,
	}
	b.Final = finite(b.Base*b.TypeCoefficient + b.Freshness + b.Engagement)
	return b
}

// Coefficient returns the type weight; unknown types weigh 1.
func Coefficient(t domain.ContentType) float64 {
	if t == domain.TypeVideo {
		return VideoCoefficient
	}
	return TextCoefficient
}

// Base computes the type-specific base score.
func Base(c domain.Content) float64 {
	m := c.Metrics
	switch c.Type {
	case domain.TypeVideo:
		return metric(m.Views)/ViewsPerPoint + metric(m.Likes)/LikesPerPoint
	case domain.TypeText:
		return metric(m.ReadingTimeMinutes) + metric(m.Reactions)/ReactionsPerPoint
	default:
		return 0
	}
}

// Engagement computes the interaction ratio term. A zero denominator
// (no views, no reading time) contributes 0 instead of +Inf or NaN.
func Engagement(c domain.Content) float64 {
	m := c.Metrics
	switch c.Type {
	case domain.TypeVideo:
		views := metric(m.Views)
		if views == 0 {
			return 0
		}
		return metric(m.Likes) / views * VideoEngagementWeight
	case domain.TypeText:
		minutes := metric(m.ReadingTimeMinutes)
		if minutes == 0 {
			return 0
		}
		return metric(m.Reactions) / minutes * TextEngagementWeight
	default:
		return 0
	}
}

// Freshness returns the recency bonus for an item published at publishedAt as
// of now, together with the instant at which that bonus next decreases (nil
// when it is already 0). Items dated in the future count as published now.
func Freshness(publishedAt, now time.Time) (float64, *time.Time) {
	age := now.Sub(publishedAt)
	if age < 0 {
		age = 0
	}
	switch {
	case age <= WeekWindow:
		return FreshnessWeek, ptr(publishedAt.Add(WeekWindow))
	case age <= MonthWindow:
		return FreshnessMonth, ptr(publishedAt.Add(MonthWindow))
	case age <= QuarterWindow:
		return FreshnessQuarter, ptr(publishedAt.Add(QuarterWindow))
	default:
		return 0, nil
	}
}

// metric converts an optional metric to float64, treating missing or negative
// values as 0.
func metric(v *int64) float64 {
	if v == nil || *v < 0 {
		return 0
	}
	return float64(*v)
}

// finite maps NaN and infinities to 0 so a score can always be stored and
// serialised.
func finite(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

func ptr(t time.Time) *time.Time { return &t }
