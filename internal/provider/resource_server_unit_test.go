package provider

import "testing"

func TestValidateOvpnDco(t *testing.T) {
	cases := []struct {
		ovpnDco bool
		cipher  string
		wantErr bool
	}{
		{true, "none", true},
		{true, "aes256", false},
		{true, "chacha20poly1205", false},
		{false, "none", false},
	}
	for _, c := range cases {
		err := validateOvpnDco(c.ovpnDco, c.cipher)
		if (err != nil) != c.wantErr {
			t.Errorf("ovpn_dco=%v cipher=%q: got error %v, want error %v", c.ovpnDco, c.cipher, err, c.wantErr)
		}
	}
}

func TestServerStatus(t *testing.T) {
	for in, want := range map[string]string{"pending": "offline", "offline": "offline", "online": "online"} {
		if got := serverStatus(in); got != want {
			t.Errorf("serverStatus(%q) = %q, want %q", in, got, want)
		}
	}
}
