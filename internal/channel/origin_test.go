package channel

import "testing"

func TestOrigin_ZeroValueIsUnspecified(t *testing.T) {
	var m Message
	if m.Origin != OriginUnspecified || m.Origin.String() != "unspecified" {
		t.Fatalf("zero origin = %v", m.Origin)
	}

	for o, want := range map[Origin]string{OriginHuman: "human", OriginHarness: "harness", OriginScripted: "scripted"} {
		if o.String() != want {
			t.Errorf("%d.String() = %q, want %q", o, o.String(), want)
		}
	}
}
