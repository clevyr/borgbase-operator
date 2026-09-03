package main

import "testing"

func TestParseNamespacedName(t *testing.T) {
	tests := []struct {
		name, in, defaultNS, wantNS, wantName string
		wantErr                               bool
	}{
		{
			name:      "bare name resolves in the operator namespace",
			in:        "borgbase-api",
			defaultNS: "borgbase-operator-system",
			wantNS:    "borgbase-operator-system",
			wantName:  "borgbase-api",
		},
		{
			// The default namespace is deliberately something the expectation
			// cannot match, so this fails if precedence is ever inverted.
			name:      "explicit namespace wins over the default",
			in:        "other-ns/other-token",
			defaultNS: "should-be-ignored",
			wantNS:    "other-ns",
			wantName:  "other-token",
		},
		{
			// Without POD_NAMESPACE a bare name has nowhere to resolve.
			// Guessing one would surface later as a confusing "secret not
			// found" at reconcile time instead of failing at startup.
			name:      "bare name with no default is an error",
			in:        "some-secret",
			defaultNS: "",
			wantErr:   true,
		},
		{name: "empty namespace is an error", in: "/name", defaultNS: "ns", wantErr: true},
		{name: "empty name is an error", in: "ns/", defaultNS: "ns", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNamespacedName(tt.in, tt.defaultNS)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseNamespacedName(%q, %q) = %v, want an error", tt.in, tt.defaultNS, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseNamespacedName(%q, %q) error = %v", tt.in, tt.defaultNS, err)
			}
			if got.Namespace != tt.wantNS || got.Name != tt.wantName {
				t.Errorf("parseNamespacedName(%q, %q) = %s/%s, want %s/%s",
					tt.in, tt.defaultNS, got.Namespace, got.Name, tt.wantNS, tt.wantName)
			}
		})
	}
}
