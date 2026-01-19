package controllers

import "time"

const (
	// PendingTimeout is the duration after which a Pending resource transitions to Failed
	PendingTimeout = 10 * time.Minute
)
