package jsonfeed

import (
	"encoding/json"
	"testing"
)

func TestFlexStringUnmarshal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    flexString
		wantErr bool
	}{
		{in: `"v1"`, want: "v1"},
		{in: `42`, want: "42"},
		{in: `-1.5`, want: "-1.5"},
		{in: `null`, want: ""},
		{in: `""`, want: ""},
		{in: `true`, wantErr: true},
		{in: `{"id":1}`, wantErr: true},
		{in: `[1]`, wantErr: true},
	}
	for _, tc := range tests {
		var got flexString
		err := json.Unmarshal([]byte(tc.in), &got)
		if (err != nil) != tc.wantErr {
			t.Errorf("flexString(%s): err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("flexString(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFlexIntUnmarshal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    flexInt
		wantErr bool
	}{
		{in: `15000`, want: 15000},
		{in: `"15000"`, want: 15000},
		{in: `" 7 "`, want: 7},
		{in: `-3`, want: -3},
		{in: `null`, want: 0},
		{in: `1.5`, wantErr: true},
		{in: `"N/A"`, wantErr: true},
		{in: `""`, wantErr: true},
		{in: `true`, wantErr: true},
	}
	for _, tc := range tests {
		var got flexInt
		err := json.Unmarshal([]byte(tc.in), &got)
		if (err != nil) != tc.wantErr {
			t.Errorf("flexInt(%s): err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("flexInt(%s) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestFlexIntValue(t *testing.T) {
	t.Parallel()

	var missing *flexInt
	if missing.value() != nil {
		t.Error("nil flexInt must map to a nil metric")
	}
	present := flexInt(9)
	if got := (&present).value(); got == nil || *got != 9 {
		t.Errorf("value() = %v, want 9", got)
	}
}
