package store

import "time"

// LLMCallCost is one model call's pricing inputs, for the daily spend
// report the self-run spend check compares with a provider's bill.
type LLMCallCost struct {
	At               time.Time
	Model            string
	Provider         string
	InputTokens      int
	OutputTokens     int
	EstimatedCostUSD float64
}
