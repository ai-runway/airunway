package e2eutil

import (
	"strings"
	"testing"
)

func TestInjectStorageClass(t *testing.T) {
	const dynamoWithStorage = `
spec:
  storage:
    persistentVolumeClaim:
      storageClassName: azurefile-premium
`
	const dynamoWrongStorage = `
spec:
  storage:
    persistentVolumeClaim:
      storageClassName: managed-csi
`
	const noStorage = `
spec:
  model:
    id: qwen
`

	cases := []struct {
		name        string
		provider    string
		raw         string
		sc          string
		wantOK      bool
		wantChanged bool // whether the output should differ from the input
		mustContain string
	}{
		{
			name:        "dynamo with pinned storage is rewritten",
			provider:    "dynamo",
			raw:         dynamoWithStorage,
			sc:          "managed-csi-premium",
			wantOK:      true,
			wantChanged: true,
			mustContain: "storageClassName: managed-csi-premium",
		},
		{
			name:        "dynamo same storage class is a no-op replace but still ok",
			provider:    "dynamo",
			raw:         dynamoWithStorage,
			sc:          "azurefile-premium",
			wantOK:      true,
			wantChanged: false,
			mustContain: "storageClassName: azurefile-premium",
		},
		{
			name:     "dynamo declares storage but not the pinned literal fails loudly",
			provider: "dynamo",
			raw:      dynamoWrongStorage,
			sc:       "managed-csi-premium",
			wantOK:   false,
		},
		{
			name:        "dynamo without any storage block passes through",
			provider:    "dynamo",
			raw:         noStorage,
			sc:          "managed-csi-premium",
			wantOK:      true,
			wantChanged: false,
		},
		{
			name:        "non-dynamo provider with storage is left untouched",
			provider:    "vllm",
			raw:         dynamoWithStorage,
			sc:          "managed-csi-premium",
			wantOK:      true,
			wantChanged: false,
			mustContain: "storageClassName: azurefile-premium",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, ok := InjectStorageClass(tc.provider, []byte(tc.raw), tc.sc)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			changed := string(out) != tc.raw
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v (out=%q)", changed, tc.wantChanged, out)
			}
			if tc.mustContain != "" && !strings.Contains(string(out), tc.mustContain) {
				t.Errorf("output missing %q:\n%s", tc.mustContain, out)
			}
		})
	}
}
