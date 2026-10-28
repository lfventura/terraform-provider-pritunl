package pritunl

import (
	"strings"
	"testing"
)

const sampleProfile = `# {"user": "ci-runner", "organization": "tools", "server": "GateTools"}
setenv UV_ID 57cec138bb884ac8b64e1d058735b4d7
setenv UV_NAME argon-7522
client
dev tun
remote vpn.example.test 11194 udp
remote 192.0.2.10 11194
cipher AES-256-CBC
key-direction 1
<ca>
-----BEGIN CERTIFICATE-----
Q0EK
-----END CERTIFICATE-----
</ca>
<cert>
-----BEGIN CERTIFICATE-----
Q0VSVAo=
-----END CERTIFICATE-----
</cert>
<key>
-----BEGIN PRIVATE KEY-----
S0VZCg==
-----END PRIVATE KEY-----
</key>
<tls-auth>
-----BEGIN OpenVPN Static key V1-----
00112233
-----END OpenVPN Static key V1-----
</tls-auth>
`

func TestParseUserProfile(t *testing.T) {
	profile, err := ParseUserProfile(sampleProfile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	checks := map[string][2]string{
		"ca":       {profile.CACert, "-----BEGIN CERTIFICATE-----\nQ0EK\n-----END CERTIFICATE-----\n"},
		"cert":     {profile.UserCert, "-----BEGIN CERTIFICATE-----\nQ0VSVAo=\n-----END CERTIFICATE-----\n"},
		"key":      {profile.UserKey, "-----BEGIN PRIVATE KEY-----\nS0VZCg==\n-----END PRIVATE KEY-----\n"},
		"tls-auth": {profile.TLSAuthKey, "-----BEGIN OpenVPN Static key V1-----\n00112233\n-----END OpenVPN Static key V1-----\n"},
		"key-dir":  {profile.KeyDirection, "1"},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s: got %q, want %q", name, c[0], c[1])
		}
	}

	want := []ProfileRemote{{Host: "vpn.example.test", Port: 11194, Protocol: "udp"}, {Host: "192.0.2.10", Port: 11194}}
	if len(profile.Remotes) != len(want) {
		t.Fatalf("remotes: got %d, want %d", len(profile.Remotes), len(want))
	}
	for i := range want {
		if profile.Remotes[i] != want[i] {
			t.Errorf("remote %d: got %+v, want %+v", i, profile.Remotes[i], want[i])
		}
	}
}

func TestParseUserProfileIgnoresPerDownloadLines(t *testing.T) {
	other := strings.NewReplacer(
		"57cec138bb884ac8b64e1d058735b4d7", "ffffffffffffffffffffffffffffffff",
		"argon-7522", "neon-0001",
	).Replace(sampleProfile)

	a, err := ParseUserProfile(sampleProfile)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseUserProfile(other)
	if err != nil {
		t.Fatal(err)
	}
	if a.CACert != b.CACert || a.UserCert != b.UserCert || a.UserKey != b.UserKey || a.TLSAuthKey != b.TLSAuthKey {
		t.Error("blocks differ between downloads that only differ in per-download lines")
	}
}

func TestParseUserProfileRejectsIncompleteProfile(t *testing.T) {
	if _, err := ParseUserProfile("client\nremote vpn.example.test 1194 udp\n"); err == nil {
		t.Error("expected an error for a profile without the ca, cert and key blocks")
	}
	if _, err := ParseUserProfile(sampleProfile + "remote vpn.example.test\n"); err == nil {
		t.Error("expected an error for a remote line without a port")
	}
}
