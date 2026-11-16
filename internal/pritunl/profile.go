package pritunl

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// UserProfile is the connection material of one user on one server, taken
// from the OpenVPN profile Pritunl serves for it. Only the parts that stay the
// same between downloads are kept: Pritunl writes a fresh `setenv UV_ID` and
// `setenv UV_NAME` into every download, so the raw file is never exposed.
type UserProfile struct {
	CACert       string
	UserCert     string
	UserKey      string
	TLSAuthKey   string
	KeyDirection string
	Remotes      []ProfileRemote
}

type ProfileRemote struct {
	Host     string
	Port     int
	Protocol string
}

var profileBlockPattern = regexp.MustCompile(`(?s)<(ca|cert|key|tls-auth)>\s*(.*?)\s*</(?:ca|cert|key|tls-auth)>`)

// ParseUserProfile reads the inline blocks, the key-direction and the remote
// lines of an OpenVPN profile.
func ParseUserProfile(conf string) (UserProfile, error) {
	var profile UserProfile

	for _, match := range profileBlockPattern.FindAllStringSubmatch(conf, -1) {
		value := match[2] + "\n"
		switch match[1] {
		case "ca":
			profile.CACert = value
		case "cert":
			profile.UserCert = value
		case "key":
			profile.UserKey = value
		case "tls-auth":
			profile.TLSAuthKey = value
		}
	}

	for _, line := range strings.Split(conf, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "key-direction":
			if len(fields) > 1 {
				profile.KeyDirection = fields[1]
			}
		case "remote":
			if len(fields) < 3 {
				return UserProfile{}, fmt.Errorf("profile remote line without a port: %q", line)
			}
			port, err := strconv.Atoi(fields[2])
			if err != nil {
				return UserProfile{}, fmt.Errorf("profile remote line with an invalid port: %q", line)
			}
			remote := ProfileRemote{Host: fields[1], Port: port}
			if len(fields) > 3 {
				remote.Protocol = fields[3]
			}
			profile.Remotes = append(profile.Remotes, remote)
		}
	}

	if profile.CACert == "" || profile.UserCert == "" || profile.UserKey == "" {
		return UserProfile{}, fmt.Errorf("profile is missing the ca, cert or key block")
	}

	return profile, nil
}
