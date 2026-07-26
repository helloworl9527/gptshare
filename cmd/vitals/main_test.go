package main

import (
	"testing"
	"time"
)

func TestReplacementTestIntervalRequiresPositiveDuration(t *testing.T) {
	for _, test := range []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "valid", value: "25ms", want: 25 * time.Millisecond},
		{name: "invalid", value: "not-a-duration", wantErr: true},
		{name: "non-positive", value: "0s", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("VITALS_TEST_REPLACEMENT_INTERVAL", test.value)
			got, err := testReplacementInterval()
			if (err != nil) != test.wantErr || (!test.wantErr && got != test.want) {
				t.Fatalf("interval=%s err=%v, want %s err=%v", got, err, test.want, test.wantErr)
			}
		})
	}
}
