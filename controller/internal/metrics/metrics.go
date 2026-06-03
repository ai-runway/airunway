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

package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// ReconciliationDurationSeconds tracks how long reconciliations take by provider.
	ReconciliationDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:                            "airunway_reconciliation_duration_seconds",
			Help:                            "Duration of ModelDeployment reconciliation in seconds.",
			Buckets:                         prometheus.DefBuckets,
			NativeHistogramBucketFactor:     1.1,
			NativeHistogramMaxBucketNumber:  160,
			NativeHistogramMinResetDuration: 1 * time.Hour,
		},
		[]string{"provider"},
	)

	// ReconciliationErrorsTotal counts reconciliation errors by provider and error type.
	ReconciliationErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "airunway_reconciliation_errors_total",
			Help: "Total number of reconciliation errors by provider and error type.",
		},
		[]string{"provider", "error_type"},
	)

	// ProviderSelection counts provider selection events by provider and reason.
	ProviderSelection = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "airunway_provider_selection_total",
			Help: "Total number of provider selection events.",
		},
		[]string{"provider", "reason"},
	)

	// DeploymentReplicas tracks aggregate replica counts by provider and state.
	DeploymentReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "airunway_deployment_replicas",
			Help: "Aggregate replica count across all ModelDeployments by provider and state (desired, ready, available).",
		},
		[]string{"provider", "state"},
	)

	// DeploymentStatus tracks ModelDeployment counts by provider and phase.
	DeploymentStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "airunway_deployment_status",
			Help: "Number of ModelDeployments by provider and phase.",
		},
		[]string{"provider", "phase"},
	)

	// PhaseTransitionsTotal counts phase transitions for tracking deployment frequency and change failure rate.
	PhaseTransitionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "airunway_deployment_phase_transitions_total",
			Help: "Total number of ModelDeployment phase transitions.",
		},
		[]string{"provider", "from_phase", "to_phase"},
	)

	// ReadyDurationSeconds measures lead time from CR creation to Running phase.
	ReadyDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:                            "airunway_deployment_ready_duration_seconds",
			Help:                            "Duration from ModelDeployment creation to Running phase in seconds.",
			Buckets:                         []float64{10, 30, 60, 120, 300, 600, 1200, 1800, 3600},
			NativeHistogramBucketFactor:     1.1,
			NativeHistogramMaxBucketNumber:  160,
			NativeHistogramMinResetDuration: 1 * time.Hour,
		},
		[]string{"provider"},
	)

	// ProvisionDurationSeconds measures infrastructure provisioning time (Deploying to Running phase).
	ProvisionDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:                            "airunway_deployment_provision_duration_seconds",
			Help:                            "Time from Deploying phase to Running phase in seconds (provider infrastructure provisioning).",
			Buckets:                         []float64{10, 30, 60, 120, 300, 600, 1200, 1800, 3600},
			NativeHistogramBucketFactor:     1.1,
			NativeHistogramMaxBucketNumber:  160,
			NativeHistogramMinResetDuration: 1 * time.Hour,
		},
		[]string{"provider"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		ReconciliationDurationSeconds,
		ReconciliationErrorsTotal,
		ProviderSelection,
		DeploymentReplicas,
		DeploymentStatus,
		PhaseTransitionsTotal,
		ReadyDurationSeconds,
		ProvisionDurationSeconds,
	)
}
