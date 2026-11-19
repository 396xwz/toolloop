package main

import (
	"fmt"
	"time"
)

func currentTimeContext(now time.Time) string {
	zone, _ := now.Zone()
	return fmt.Sprintf("Runtime clock (host local time): %s (%s; timezone: %s, %s).\nUse this current date/time as the reference for today and now, rather than dates assumed from training data. No tool is needed just to determine the current date/time.",
		now.Format(time.RFC3339), now.Weekday(), now.Location(), zone)
}
