package balancer

import "time"

// RequestContext carries per-request information needed for routing decisions.
// Fields beyond AffinityKey are forward-compatibility scaffolding;
// they are nil/zero until ACL/rate-limiting/cost-aware features are enabled.
type RequestContext struct {
	AffinityKey    string
	IsStreaming    bool
	EstimatedSize  int           // approximate token count (totalChars / 4)
	StaleThreshold time.Duration // staleness cutoff for scraped metrics (set by balancer)
}
