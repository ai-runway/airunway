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

package vllm

import "github.com/ai-runway/airunway/providers/pkg/shim"

const (
	conditionAvailable   = "Available"
	conditionProgressing = "Progressing"
)

type ProviderStatusResult = shim.DeploymentStatusResult

type StatusTranslator = shim.DeploymentStatusTranslator

func NewStatusTranslator() *StatusTranslator {
	return shim.NewDeploymentStatusTranslator(int32(DefaultVLLMPort))
}
