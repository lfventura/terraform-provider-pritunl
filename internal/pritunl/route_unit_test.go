package pritunl

import (
	"encoding/json"
	"strings"
	"testing"
)

// A bool behind omitempty never serializes false; Pritunl then seeds its own
// default for the field. Both route flags must always reach the API.
func TestRouteMarshalKeepsFalseBooleans(t *testing.T) {
	data, err := json.Marshal(Route{Network: "::/0"})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"\"nat\":false", "\"net_gateway\":false"} {
		if !strings.Contains(string(data), field) {
			t.Errorf("marshalled route misses %s: %s", field, data)
		}
	}
}
