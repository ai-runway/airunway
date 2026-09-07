/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// ValidateExternalAPIBaseURL validates the external endpoint contract shared
// by admission and reconciliation. Reconciliation repeats this check so
// objects created while the webhook was unavailable cannot publish unusable
// provider configuration.
func ValidateExternalAPIBaseURL(baseURL string) error {
	if baseURL != strings.TrimSpace(baseURL) {
		return fmt.Errorf("must not have leading or trailing whitespace")
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("must be a valid URL: %w", err)
	}
	if !parsed.IsAbs() || parsed.Host == "" || parsed.Hostname() == "" {
		return fmt.Errorf("must be an absolute URL with a host")
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return fmt.Errorf("scheme must be http or https")
	}
	if parsed.User != nil {
		return fmt.Errorf("must not include user information")
	}
	if strings.HasPrefix(parsed.Host, "[") {
		address, err := netip.ParseAddr(parsed.Hostname())
		if err != nil || !address.Is6() {
			return fmt.Errorf("bracketed host must be a valid IPv6 address")
		}
	} else if strings.Count(parsed.Host, ":") > 1 {
		return fmt.Errorf("IPv6 address must be enclosed in brackets")
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return fmt.Errorf("port must not be empty")
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return fmt.Errorf("port must be between 1 and 65535")
		}
	}
	return nil
}
