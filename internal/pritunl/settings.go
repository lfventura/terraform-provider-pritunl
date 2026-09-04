package pritunl

// Settings holds the subset of the global Pritunl settings managed by this
// provider. PUT /settings is a partial update, it only touches the fields that
// are present in the request body, so declaring nothing else here is what keeps
// every unmanaged setting untouched.
//
// The certificate fields are pointers on purpose: omitempty drops them from the
// payload only when the pointer is nil ("leave this field alone"), while a
// non-nil pointer is always serialised, including when it holds an empty string
// ("set this field to that value").
type Settings struct {
	ServerCert *string `json:"server_cert,omitempty"`
	ServerKey  *string `json:"server_key,omitempty"`
	ServerPort int     `json:"server_port,omitempty"`
}

// resetCertificateSettings is only used to hand the certificate back to
// Pritunl. Resetting a field requires its key to be present in the body with a
// literal JSON null, and a nil pointer tagged with omitempty is dropped from the
// payload instead of being serialised as null, which is why these two fields
// cannot reuse the Settings struct above.
type resetCertificateSettings struct {
	ServerCert *string `json:"server_cert"`
	ServerKey  *string `json:"server_key"`
}
