// Package day defines what "today" means for AOTD: the calendar date in
// Central European Time (CET/CEST).
package day

import (
	"time"
	_ "time/tzdata" // don't depend on the host's zoneinfo
)

var Location = mustLoad("Europe/Berlin")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

const Layout = "2006-01-02"

// Of returns the CET date of t as YYYY-MM-DD.
func Of(t time.Time) string {
	return t.In(Location).Format(Layout)
}

// Today returns the current CET date.
func Today() string { return Of(time.Now()) }

// Pretty formats a YYYY-MM-DD date for display, e.g. "Thu 1 Oct 2026".
func Pretty(d string) string {
	t, err := time.ParseInLocation(Layout, d, Location)
	if err != nil {
		return d
	}
	return t.Format("Mon 2 Jan 2006")
}
