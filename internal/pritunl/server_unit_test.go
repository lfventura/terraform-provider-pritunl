package pritunl

import (
	"encoding/json"
	"testing"
)

// Pritunl only changes ovpn_dco when the request carries the key, so turning
// the offload off needs an explicit false in the body.
func TestServerMarshalJSONWritesOvpnDcoFalse(t *testing.T) {
	for _, want := range []bool{true, false} {
		data, err := (&Server{Name: "s", OvpnDco: want}).MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatal(err)
		}
		got, ok := body["ovpn_dco"]
		if !ok {
			t.Fatalf("ovpn_dco=%v: the key is missing from %s", want, data)
		}
		if got != want {
			t.Errorf("ovpn_dco: got %v, want %v", got, want)
		}
	}
}
