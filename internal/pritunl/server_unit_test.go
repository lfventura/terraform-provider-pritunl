package pritunl

import (
	"encoding/json"
	"testing"
)

// The request body states ovpn_dco for both values, false included.
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
