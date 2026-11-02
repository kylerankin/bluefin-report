package ui

import (
	"strings"

	"github.com/projectbluefin/bluefin-report/internal/snapshot"
)

// familyName returns the family a metric belongs to: the full family name, or
// the metric name itself when it has no object.
func familyName(m snapshot.Metric) string {
	name := m.Name
	if m.Object != "" {
		name += " [" + m.Object + "]"
	}
	return name
}

// familyFor looks up a metric's family descriptions. It strips the "user: "
// prefix the metrics list prepends to user metrics so the lookup matches the
// family names describe reports for both scopes.
func familyFor(name string, fams map[string]snapshot.Family) snapshot.Family {
	if strings.HasPrefix(name, "user: ") {
		name = strings.TrimPrefix(name, "user: ")
	}
	return fams[name]
}

// metricTooltip builds the hover text for a metric row. A known family with a
// plain-words description shows the metric name and value, then its family
// description and type; an unknown or undescribed family falls back to
// "name = value".
func metricTooltip(name string, val string, fams map[string]snapshot.Family) string {
	tip := name + " = " + val
	if fam := familyFor(name, fams); fam.Description != "" {
		tip += "\n" + fam.Description
		if fam.Type != "" {
			tip += " · " + fam.Type
		}
	}
	return tip
}
