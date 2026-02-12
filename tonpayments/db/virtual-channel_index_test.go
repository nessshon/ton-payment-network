package db

import "testing"

func TestHasSpecialDetails(t *testing.T) {
	t.Run("filled map", func(t *testing.T) {
		v := map[string]any{
			"foo": "bar",
		}
		if !hasSpecialDetails(v) {
			t.Fatalf("expected filled special details to be detected")
		}
	})

	t.Run("nested map", func(t *testing.T) {
		v := map[string]any{
			"details": map[string]any{"x": 1},
		}
		if !hasSpecialDetails(v) {
			t.Fatalf("expected nested special details to be detected")
		}
	})

	t.Run("empty map", func(t *testing.T) {
		v := map[string]any{}
		if hasSpecialDetails(v) {
			t.Fatalf("empty details must not be indexed")
		}
	})

	t.Run("nil", func(t *testing.T) {
		if hasSpecialDetails(nil) {
			t.Fatalf("nil details must not be indexed")
		}
	})

	t.Run("non object", func(t *testing.T) {
		v := map[string]any{
			"foo": "bar",
		}
		raw := v["foo"]
		if hasSpecialDetails(raw) {
			t.Fatalf("non-object special details must not be indexed")
		}
	})
}

func TestShouldIndexActiveSpecialMeta(t *testing.T) {
	meta := &ConditionalMeta{
		Status: ConditionalStateActive,
		Incoming: &ConditionalMetaSide{
			ChannelAddress: "ch",
		},
		SpecialDetails: map[string]any{
			"foo": "bar",
		},
	}

	if !shouldIndexActiveSpecialMeta(meta) {
		t.Fatalf("expected active incoming special meta to be indexed")
	}

	meta.Status = ConditionalStateClosed
	if shouldIndexActiveSpecialMeta(meta) {
		t.Fatalf("closed meta should not be indexed")
	}

	meta.Status = ConditionalStateActive
	meta.Incoming = nil
	if shouldIndexActiveSpecialMeta(meta) {
		t.Fatalf("meta without incoming side should not be indexed")
	}

	meta.Incoming = &ConditionalMetaSide{ChannelAddress: "ch"}
	meta.SpecialDetails = map[string]any{}
	if shouldIndexActiveSpecialMeta(meta) {
		t.Fatalf("meta with empty special details should not be indexed")
	}
}
