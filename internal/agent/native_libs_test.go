package agent

import (
	"slices"
	"testing"
)

func TestMissingLibs(t *testing.T) {
	cache := `	libz.so.1 (libc6,AArch64) => /lib/aarch64-linux-gnu/libz.so.1
	libstdc++.so.6 (libc6,AArch64) => /lib/aarch64-linux-gnu/libstdc++.so.6
	libssl.so.3 (libc6,AArch64) => /lib/aarch64-linux-gnu/libssl.so.3
	libgssapi_krb5.so.2 (libc6,AArch64) => /lib/aarch64-linux-gnu/libgssapi_krb5.so.2
`
	if got := missingLibs(cache); !slices.Equal(got, []string{"libicu"}) {
		t.Fatalf("missing = %v, want libicu only", got)
	}
	if got := missingLibs(cache + "\tlibicuuc.so.74 (libc6,AArch64) => /lib/aarch64-linux-gnu/libicuuc.so.74\n"); len(got) != 0 {
		t.Fatalf("missing = %v, want none", got)
	}
}
