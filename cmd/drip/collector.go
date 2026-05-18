package main

import (
	"github.com/aymanhs/nanotdb/cmd/drip/collectors"
)

// Metric and Collector are defined in the collectors subpackage.
// These type aliases allow main package code to use short names.
type Metric = collectors.Metric
type Collector = collectors.Collector
