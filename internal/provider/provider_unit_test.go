package provider

import (
	"testing"
	"time"

	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

func TestParseServerStartTimeout(t *testing.T) {
	valid := map[string]time.Duration{"90s": 90 * time.Second, "10m": 10 * time.Minute, "5m0s": 5 * time.Minute}
	for value, want := range valid {
		got, err := parseServerStartTimeout(value)
		if err != nil || got != want {
			t.Errorf("%q: got %s, %v, want %s", value, got, err, want)
		}
	}

	for _, value := range []string{"", "5", "ten minutes", "0s", "-1m"} {
		if _, err := parseServerStartTimeout(value); err == nil {
			t.Errorf("%q must be refused", value)
		}
	}
}

func TestProviderDefaultServerStartTimeout(t *testing.T) {
	// the environment override would replace the default this test checks
	t.Setenv("PRITUNL_SERVER_START_TIMEOUT", "")

	schema := Provider().Schema["server_start_timeout"]
	value, err := schema.DefaultFunc()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := parseServerStartTimeout(value.(string)); err != nil || got != pritunl.DefaultServerStartTimeout {
		t.Fatalf("the default must be %s, got %v (%v)", pritunl.DefaultServerStartTimeout, value, err)
	}
}

// The meta the provider builds is still a pritunl.Client for every resource,
// and carries the window; a bare client gets the default.
func TestProviderMetaCarriesServerStartTimeout(t *testing.T) {
	var meta interface{} = &providerMeta{Client: pritunl.NewClient("https://localhost", "t", "s", true), serverStartTimeout: 7 * time.Minute}

	if _, ok := meta.(pritunl.Client); !ok {
		t.Fatal("the provider meta must satisfy pritunl.Client")
	}
	if got := serverStartTimeout(meta); got != 7*time.Minute {
		t.Fatalf("the configured window must be used, got %s", got)
	}
	if got := serverStartTimeout(pritunl.NewClient("https://localhost", "t", "s", true)); got != pritunl.DefaultServerStartTimeout {
		t.Fatalf("a bare client must get the default window, got %s", got)
	}
}
