package monitorfacade

import (
	"errors"
	"testing"
)

func TestTypedFaultsRemainUnavailableToLegacyCallers(t *testing.T) {
	for _, kind := range []FaultKind{FaultUnavailable, FaultTimeout, FaultNotFound, FaultContractChanged} {
		err := NewFault(kind)
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("%s must preserve fail-closed unavailable classification", kind)
		}
		got, ok := FaultKindOf(err)
		if !ok || got != kind {
			t.Fatalf("FaultKindOf(%s) = %s, %v", kind, got, ok)
		}
	}
}
