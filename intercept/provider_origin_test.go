package intercept

import (
	"errors"
	"testing"
)

func TestProviderOriginErrorPreservesConcreteSnapshot(t *testing.T) {
	ctx := WithProviderOrigin(t.Context())
	failure := errors.New("late failure")
	SetProviderOrigin(ctx, ProviderOrigin{ProfileID: 12, Native: true, Data: "native-config"})
	SetProviderError(ctx, failure)
	got := providerOrigin(ctx)
	if got.ProfileID != 12 || !got.Native || got.Data != "native-config" || got.Err != failure {
		t.Fatalf("native failure erased concrete selection: %+v", got)
	}
	// A selected cloud member replaces the entire previous native snapshot.
	SetProviderOrigin(ctx, ProviderOrigin{ProfileID: 34, Data: "cloud-config"})
	SetProviderError(ctx, failure)
	got = providerOrigin(ctx)
	if got.ProfileID != 34 || got.Native || got.Data != "cloud-config" || got.Err != failure {
		t.Fatalf("cloud selection retained stale native state: %+v", got)
	}
	fresh := WithProviderOrigin(ctx)
	SetProviderError(fresh, failure)
	if got := providerOrigin(fresh); got.Native || got.ProfileID != 0 || got.Data != nil || got.Err != failure {
		t.Fatalf("fresh route error inherited origin: %+v", got)
	}
	if got := providerOrigin(ctx); got.ProfileID != 34 {
		t.Fatal("fresh run changed parent state")
	}
}
