package provider

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

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

// A server that never got its DH parameters is reported as such: on create
// the error says the resource is tainted and replaced on the next apply.
func TestServerStartDiagDhPending(t *testing.T) {
	dhErr := &pritunl.ServerDhPendingError{ServerId: "srv", ServerName: "vpn-a", Window: 10 * time.Minute}

	created := serverStartDiag(dhErr, true)
	if len(created) != 1 || created[0].Severity != diag.Error {
		t.Fatalf("one error expected, got %+v", created)
	}
	for _, want := range []string{`server "vpn-a" (srv)`, "pending after 10m0s", "never generated its DH parameters", "tainted", "next apply replaces it", "server_dh_timeout"} {
		if !strings.Contains(created[0].Summary+" "+created[0].Detail, want) {
			t.Errorf("the create error %q does not say %q", created[0].Detail, want)
		}
	}

	updated := serverStartDiag(fmt.Errorf("wrapped: %w", dhErr), false)
	if strings.Contains(updated[0].Detail, "tainted") || !strings.Contains(updated[0].Detail, "-replace") {
		t.Fatalf("an update keeps the server and says how to replace it, got %q", updated[0].Detail)
	}

	other := serverStartDiag(fmt.Errorf("boom"), true)
	if other[0].Summary != "Error on starting server: boom" {
		t.Fatalf("other failures keep their error, got %q", other[0].Summary)
	}
}
