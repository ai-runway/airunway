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

// Package agentcontainer carries the reported build version for the agent-container shim.
package agentcontainer

// ProviderConfigName names this shim for version reporting.
const ProviderConfigName = "agent-container"

// shimVersion is this shim's reported version tag, injected at build time via:
//
//	-ldflags "-X $(go list -m).shimVersion=$(SHIM_VERSION)"
//
// The "dev" fallback only applies when the Makefile is bypassed (e.g. `go run`,
// plain `go test`).
var shimVersion = "dev"

// ProviderVersion is the reported version of this shim (e.g.
// "agent-container-provider:v0.8.0"), published to AgentProviderConfig.status.version.
var ProviderVersion = ProviderConfigName + "-provider:" + shimVersion
