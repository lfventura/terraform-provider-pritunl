package provider

import (
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// pritunlNameSafeChars is the NAME_SAFE_CHARS set of Pritunl: the characters
// besides letters and numbers that its filter_str keeps.
const pritunlNameSafeChars = "-=_@.:/!#$&*+?^|~"

// pritunlFilterStr mirrors Pritunl's filter_str, which from 1.34.4763 rewrites
// every route comment before storing it: only what Python's str.isalnum
// accepts (letters and numbers, superscripts and fractions included) and
// pritunlNameSafeChars survive, so spaces, commas and parentheses are dropped.
func pritunlFilterStr(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || strings.ContainsRune(pritunlNameSafeChars, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// suppressRouteCommentDiff treats a route comment as unchanged when it differs
// from the stored one only by what Pritunl's filter_str removes. Releases
// before 1.34.4763 store the comment as written, later ones store it filtered,
// and both read back as the same comment.
func suppressRouteCommentDiff(_, oldValue, newValue string, _ *schema.ResourceData) bool {
	return pritunlFilterStr(oldValue) == pritunlFilterStr(newValue)
}

// ssoURLKey is the form two server_sso_url values are compared in. Pritunl
// releases before 1.34.4763 store the value as a URL, adding https:// when the
// scheme is missing; later ones store only the lowercased host and port. A
// value made of a host alone, with or without a scheme, is therefore keyed by
// its host and port, which both releases agree on. A value carrying a path, a
// query or a fragment keeps them in its key: older releases store and use
// them. A value that does not parse is keyed by itself, trimmed.
func ssoURLKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	raw := s
	if !strings.Contains(raw, "://") {
		if ip := net.ParseIP(raw); ip != nil && strings.Contains(raw, ":") {
			raw = "[" + raw + "]"
		}
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return s
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil {
			return s
		}
		host += ":" + strconv.Itoa(n)
	}
	if (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == "" {
		return host
	}
	return strings.ToLower(u.Scheme) + "://" + host + u.EscapedPath() + "?" + u.RawQuery + "#" + u.Fragment
}

// suppressServerSSOURLDiff treats server_sso_url as unchanged when the
// configured and the stored values have the same ssoURLKey.
func suppressServerSSOURLDiff(_, oldValue, newValue string, _ *schema.ResourceData) bool {
	return ssoURLKey(oldValue) == ssoURLKey(newValue)
}
