package pritunl

// Subscription is the license state of a Pritunl instance as GET
// /subscription reports it. The license key itself is write-only: the API
// never returns it.
type Subscription struct {
	Active   bool   `json:"active"`
	Status   string `json:"status"`
	Plan     string `json:"plan"`
	Quantity int    `json:"quantity"`
}

// SubscriptionActivation is the body POST /subscription takes.
type SubscriptionActivation struct {
	License string `json:"license"`
}
