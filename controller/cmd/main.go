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

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	airunwayv1alpha1 "github.com/kaito-project/airunway/controller/api/v1alpha1"
	"github.com/kaito-project/airunway/controller/internal/controller"
	"github.com/kaito-project/airunway/controller/internal/gateway"
	webhookv1alpha1 "github.com/kaito-project/airunway/controller/internal/webhook/v1alpha1"
	inferencev1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	// +kubebuilder:scaffold:imports
)

const (
	secretName     = "airunway-webhook-server-cert"
	caName         = "airunway-ca"
	caOrganization = "airunway"
	certDir        = "/tmp/k8s-webhook-server/serving-certs"
	vwhName        = "airunway-validating-webhook-configuration"
	mwhName        = "airunway-mutating-webhook-configuration"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(airunwayv1alpha1.AddToScheme(scheme))
	utilruntime.Must(gatewayv1.Install(scheme))
	utilruntime.Must(gatewayv1beta1.Install(scheme))
	utilruntime.Must(inferencev1.Install(scheme))
	// +kubebuilder:scaffold:scheme
}

// ensureBootstrapCerts creates temporary self-signed TLS certificates in certDir
// so the webhook server can start. The cert-rotator will overwrite these with
// properly signed certificates once it runs.
func ensureBootstrapCerts(dir string) error {
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	// Skip if certs already exist
	if _, err := os.Stat(certPath); err == nil {
		return nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating cert dir: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generating key: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("creating certificate: %w", err)
	}

	certFile, err := os.Create(certPath)
	if err != nil {
		return fmt.Errorf("creating cert file: %w", err)
	}
	defer certFile.Close()
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		return fmt.Errorf("encoding cert: %w", err)
	}

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshaling key: %w", err)
	}

	keyFile, err := os.Create(keyPath)
	if err != nil {
		return fmt.Errorf("creating key file: %w", err)
	}
	defer keyFile.Close()
	if err := pem.Encode(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		return fmt.Errorf("encoding key: %w", err)
	}

	return nil
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var enableProviderSelector bool
	var disableCertRotation bool
	var certServiceName string
	var gatewayName string
	var gatewayNamespace string
	var eppServicePort int
	var eppImage string
	var patchGateway bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.BoolVar(&enableProviderSelector, "enable-provider-selector", true,
		"If set, the controller will run provider selection for ModelDeployments without explicit provider.name")
	flag.BoolVar(&disableCertRotation, "disable-cert-rotation", false,
		"Disable automatic generation and rotation of webhook TLS certificates/keys")
	flag.StringVar(&certServiceName, "cert-service-name", "airunway-webhook-service",
		"The service name used to generate the TLS cert's hostname. Defaults to airunway-webhook-service")
	flag.StringVar(&gatewayName, "gateway-name", "",
		"Explicit Gateway resource name for HTTPRoute parent. If empty, auto-detects from cluster.")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "",
		"Namespace of the Gateway resource. Required when --gateway-name is set.")
	flag.IntVar(&eppServicePort, "epp-service-port", 9002,
		"Port of the Endpoint Picker Proxy (EPP) Service.")
	flag.StringVar(&eppImage, "epp-image",
		"registry.k8s.io/gateway-api-inference-extension/epp:"+gateway.DefaultGAIEVersion,
		"Container image for the Endpoint Picker Proxy (EPP).")
	flag.BoolVar(&patchGateway, "patch-gateway-allowed-routes", true,
		"Patch the Gateway's allowedRoutes to accept HTTPRoutes from ModelDeployment namespaces. "+
			"Set to false when a Gateway admin manages allowedRoutes independently.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Validate gateway flags: both must be set or both empty
	if (gatewayName == "") != (gatewayNamespace == "") {
		setupLog.Error(fmt.Errorf("--gateway-name and --gateway-namespace must both be set or both be empty"), "invalid gateway flags")
		os.Exit(1)
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Ensure bootstrap certs exist so the webhook server can start.
	// The cert-rotator will overwrite these with properly signed certificates.
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := ensureBootstrapCerts(certDir); err != nil {
			setupLog.Error(err, "unable to create bootstrap certificates")
			os.Exit(1)
		}
	}

	// Webhook server options - cert-controller will write certs to certDir
	webhookServerOptions := webhook.Options{
		TLSOpts: tlsOpts,
		CertDir: certDir,
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.23.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "2038fe6a.airunway.ai",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Set up cert rotation for webhook TLS certificates.
	setupFinished := make(chan struct{})
	if !disableCertRotation && os.Getenv("ENABLE_WEBHOOKS") != "false" {
		setupLog.Info("setting up cert rotation")

		podNamespace := os.Getenv("POD_NAMESPACE")
		if podNamespace == "" {
			setupLog.Error(fmt.Errorf("POD_NAMESPACE must be set"), "unable to determine namespace")
			os.Exit(1)
		}

		dnsName := fmt.Sprintf("%s.%s.svc", certServiceName, podNamespace)

		if err := rotator.AddRotator(mgr, &rotator.CertRotator{
			SecretKey: types.NamespacedName{
				Namespace: podNamespace,
				Name:      secretName,
			},
			CertDir:        certDir,
			CAName:         caName,
			CAOrganization: caOrganization,
			DNSName:        dnsName,
			IsReady:        setupFinished,
			Webhooks: []rotator.WebhookInfo{
				{
					Name: vwhName,
					Type: rotator.Validating,
				},
				{
					Name: mwhName,
					Type: rotator.Mutating,
				},
			},
		}); err != nil {
			setupLog.Error(err, "unable to set up cert rotation")
			os.Exit(1)
		}

		// Sync certs from the Secret to the filesystem after cert rotation is ready.
		// The cert-rotator writes to the K8s Secret; this copies the data to certDir
		// so the webhook server can serve the proper certificates.
		go func() {
			<-setupFinished
			setupLog.Info("syncing certs from secret to filesystem")
			secret := &corev1.Secret{}
			if err := mgr.GetAPIReader().Get(context.Background(), types.NamespacedName{
				Namespace: podNamespace,
				Name:      secretName,
			}, secret); err != nil {
				setupLog.Error(err, "unable to read cert secret")
				return
			}
			for key, data := range secret.Data {
				if err := os.WriteFile(filepath.Join(certDir, key), data, 0o644); err != nil {
					setupLog.Error(err, "unable to write cert file", "file", key)
				}
			}
			setupLog.Info("certs synced to filesystem")
		}()
	} else {
		close(setupFinished)
	}

	// Create gateway detector
	dc, err := discovery.NewDiscoveryClientForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to create discovery client")
		os.Exit(1)
	}
	gatewayDetector := gateway.NewDetector(dc)
	gatewayDetector.ExplicitGatewayName = gatewayName
	gatewayDetector.ExplicitGatewayNamespace = gatewayNamespace
	gatewayDetector.EPPServicePort = int32(eppServicePort)
	gatewayDetector.EPPImage = eppImage
	gatewayDetector.PatchGateway = patchGateway

	if err := (&controller.ModelDeploymentReconciler{
		Client:                 mgr.GetClient(),
		Scheme:                 mgr.GetScheme(),
		EnableProviderSelector: enableProviderSelector,
		GatewayDetector:        gatewayDetector,
		ProviderResolver:       gateway.NewInferenceProviderConfigResolver(mgr.GetClient()),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ModelDeployment")
		os.Exit(1)
	}
	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1alpha1.SetupModelDeploymentWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create webhook", "webhook", "ModelDeployment")
			os.Exit(1)
		}
	}

	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
