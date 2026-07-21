package e2eutil

import (
	"net/http"
	"strings"
	"testing"
)

func TestParseChatResponse(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantText  string
		wantErr   bool
		errSubstr string
	}{
		{
			name:     "content present",
			status:   http.StatusOK,
			body:     `{"choices":[{"message":{"content":"hello","reasoning":""}}]}`,
			wantText: "hello",
		},
		{
			name:     "reasoning fallback when content empty",
			status:   http.StatusOK,
			body:     `{"choices":[{"message":{"content":"","reasoning":"thinking out loud"}}]}`,
			wantText: "thinking out loud",
		},
		{
			name:     "content wins over reasoning",
			status:   http.StatusOK,
			body:     `{"choices":[{"message":{"content":"answer","reasoning":"think"}}]}`,
			wantText: "answer",
		},
		{
			name:      "non-200 status",
			status:    http.StatusInternalServerError,
			body:      `{"message":"boom"}`,
			wantErr:   true,
			errSubstr: "status 500",
		},
		{
			name:      "invalid JSON",
			status:    http.StatusOK,
			body:      `not json`,
			wantErr:   true,
			errSubstr: "not valid JSON",
		},
		{
			name:      "error envelope",
			status:    http.StatusOK,
			body:      `{"error":{"message":"model not found"}}`,
			wantErr:   true,
			errSubstr: "model not found",
		},
		{
			name:      "empty choices",
			status:    http.StatusOK,
			body:      `{"choices":[]}`,
			wantErr:   true,
			errSubstr: "no choices",
		},
		{
			name:     "both fields empty returns empty string (caller treats as failure)",
			status:   http.StatusOK,
			body:     `{"choices":[{"message":{"content":"","reasoning":""}}]}`,
			wantText: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseChatResponse(tc.status, []byte(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (text=%q)", got)
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantText {
				t.Errorf("got %q, want %q", got, tc.wantText)
			}
		})
	}
}
