package kaito

import (
	"os"
	"testing"
)

// TestShimVersionInjection closes the gap left by the test.yml `go version -m`
// check: that grep proves the -ldflags string was *passed* to `go build`, not
// that the linker actually *resolved and patched* the shimVersion symbol. A
// future refactor (making shimVersion a const, renaming it, or retargeting
// ProviderVersion) would keep the flag present but silently no-op the
// injection, reverting status.version to "kaito-provider:dev" with CI still green.
//
// This test asserts the *runtime* value of ProviderVersion after injection. It
// is gated on EXPECT_PROVIDER_VERSION so a plain `go test` (which never sets
// ldflags) skips instead of failing. CI runs it built with
//
//	-ldflags "-X $(go list -m).shimVersion=<tag>"
//	EXPECT_PROVIDER_VERSION="kaito-provider:<tag>"
//
// so a silent no-op fails the build.
func TestShimVersionInjection(t *testing.T) {
	want := os.Getenv("EXPECT_PROVIDER_VERSION")
	if want == "" {
		t.Skip("EXPECT_PROVIDER_VERSION not set; skipping injection assertion (plain go test has no ldflags)")
	}
	if ProviderVersion != want {
		t.Fatalf("ProviderVersion = %q, want %q — the -ldflags -X shimVersion injection did not take effect (silent no-op)", ProviderVersion, want)
	}
}
