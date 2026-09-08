package agentorka

import (
	"os"
	"testing"
)

// TestShimVersionInjection asserts the *runtime* value of ProviderVersion after
// injection, which the test.yml `go version -m` grep cannot: that grep only
// proves the -ldflags string was passed to `go build`, not that the linker
// resolved and patched the symbol. Making shimVersion a const, renaming it, or
// retargeting ProviderVersion would keep the flag present while silently
// no-oping the injection.
//
// Gated on EXPECT_PROVIDER_VERSION so a plain `go test` (no ldflags) skips.
func TestShimVersionInjection(t *testing.T) {
	want := os.Getenv("EXPECT_PROVIDER_VERSION")
	if want == "" {
		t.Skip("EXPECT_PROVIDER_VERSION not set; skipping injection assertion (plain go test has no ldflags)")
	}
	if ProviderVersion != want {
		t.Fatalf("ProviderVersion = %q, want %q — the -ldflags -X shimVersion injection did not take effect (silent no-op)", ProviderVersion, want)
	}
}
