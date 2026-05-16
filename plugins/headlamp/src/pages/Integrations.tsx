/**
 * Integrations Page
 *
 * Configure external integrations like NVIDIA GPU Operator, Gateway API, and HuggingFace.
 */

import { useState, useEffect, useCallback } from 'react';
import {
  SectionBox,
  Loader,
  StatusLabel,
} from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import { Icon } from '@iconify/react';
import { useApiClient } from '../lib/api-client';
import type { GPUOperatorStatus, HfSecretStatus, GatewayCRDStatus } from '@airunway/shared';
import { ConnectionError } from '../components/ConnectionBanner';
import { copyToClipboard } from '../lib/utils';

/**
 * Inline loading spinner for use inside sections (simpler than the full-page Loader)
 */
function InlineLoader() {
  return (
    <span
      style={{
        display: 'inline-block',
        width: '16px',
        height: '16px',
        border: '2px solid currentColor',
        borderTopColor: 'transparent',
        borderRadius: '50%',
        animation: 'spin 1s linear infinite',
      }}
    />
  );
}

export function Integrations() {
  const api = useApiClient();

  // GPU Operator state
  const [gpuStatus, setGpuStatus] = useState<GPUOperatorStatus | null>(null);
  const [gpuLoading, setGpuLoading] = useState(true);
  const [gpuError, setGpuError] = useState<string | null>(null);
  const [installing, setInstalling] = useState(false);

  // HuggingFace state
  const [hfStatus, setHfStatus] = useState<HfSecretStatus | null>(null);
  const [hfLoading, setHfLoading] = useState(true);
  const [hfError, setHfError] = useState<string | null>(null);
  const [disconnectingHf, setDisconnectingHf] = useState(false);

  // Gateway API state
  const [gatewayStatus, setGatewayStatus] = useState<GatewayCRDStatus | null>(null);
  const [gatewayLoading, setGatewayLoading] = useState(true);
  const [gatewayError, setGatewayError] = useState<string | null>(null);
  const [installingGateway, setInstallingGateway] = useState(false);

  // Fetch GPU Operator status
  const fetchGpuStatus = useCallback(async () => {
    setGpuLoading(true);
    setGpuError(null);

    try {
      const result = await api.gpuOperator.getStatus();
      setGpuStatus(result);
    } catch (err) {
      setGpuError(err instanceof Error ? err.message : 'Failed to fetch GPU status');
    } finally {
      setGpuLoading(false);
    }
  }, [api]);

  // Fetch HuggingFace status
  const fetchHfStatus = useCallback(async () => {
    setHfLoading(true);
    setHfError(null);

    try {
      const result = await api.huggingFace.getSecretStatus();
      setHfStatus(result);
    } catch (err) {
      setHfError(err instanceof Error ? err.message : 'Failed to fetch HuggingFace status');
    } finally {
      setHfLoading(false);
    }
  }, [api]);

  // Fetch Gateway CRD status
  const fetchGatewayStatus = useCallback(async () => {
    setGatewayLoading(true);
    setGatewayError(null);

    try {
      const result = await api.gateway.getStatus();
      setGatewayStatus(result);
    } catch (err) {
      setGatewayError(err instanceof Error ? err.message : 'Failed to fetch Gateway status');
    } finally {
      setGatewayLoading(false);
    }
  }, [api]);

  // Install GPU Operator
  const handleInstallGpu = useCallback(async () => {
    setInstalling(true);

    try {
      const result = await api.gpuOperator.install();
      if (result.success) {
        await fetchGpuStatus();
      } else {
        alert(`Installation failed: ${result.message}`);
      }
    } catch (err) {
      alert(`Failed to install GPU Operator: ${err instanceof Error ? err.message : 'Unknown error'}`);
    } finally {
      setInstalling(false);
    }
  }, [api, fetchGpuStatus]);

  // Install Gateway CRDs
  const handleInstallGatewayCRDs = useCallback(async () => {
    setInstallingGateway(true);

    try {
      const result = await api.gateway.installCrds();
      if (result.success) {
        await fetchGatewayStatus();
      } else {
        alert(`Installation failed: ${result.message}`);
      }
    } catch (err) {
      alert(`Failed to install Gateway CRDs: ${err instanceof Error ? err.message : 'Unknown error'}`);
    } finally {
      setInstallingGateway(false);
    }
  }, [api, fetchGatewayStatus]);

  // TODO: Re-enable OAuth sign-in once backend is ready
  // Start HuggingFace OAuth flow
  // Uses backend-driven PKCE flow to work across different frontend origins
  // const handleConnectHf = useCallback(async () => {
  //   setConnectingHf(true);
  //
  //   try {
  //     // Get the callback URL for this environment (uses current origin)
  //     const redirectUri = getHuggingFaceCallbackUrl();
  //
  //     // Start OAuth via backend - it generates PKCE and stores the verifier
  //     const { authorizationUrl, state } = await api.huggingFace.startOAuth({ redirectUri });
  //
  //     // Store state so the callback can validate it
  //     sessionStorage.setItem('hf_oauth_state', state);
  //     sessionStorage.setItem('hf_oauth_from_headlamp', 'true');
  //
  //     // Redirect to HuggingFace authorization
  //     window.location.href = authorizationUrl;
  //   } catch (err) {
  //     alert(`Failed to start OAuth: ${err instanceof Error ? err.message : 'Unknown error'}`);
  //     setConnectingHf(false);
  //   }
  // }, [api]);

  // Disconnect HuggingFace
  const handleDisconnectHf = useCallback(async () => {
    setDisconnectingHf(true);

    try {
      await api.huggingFace.deleteSecret();
      await fetchHfStatus();
    } catch (err) {
      alert(`Failed to disconnect: ${err instanceof Error ? err.message : 'Unknown error'}`);
    } finally {
      setDisconnectingHf(false);
    }
  }, [api, fetchHfStatus]);

  const [copiedText, setCopiedText] = useState<string | null>(null);

  const handleCopy = useCallback(async (text: string) => {
    const success = await copyToClipboard(text);
    if (success) {
      setCopiedText(text);
      setTimeout(() => setCopiedText(null), 2000);
    }
  }, []);

  // Initial fetch
  useEffect(() => {
    fetchGpuStatus();
    fetchHfStatus();
    fetchGatewayStatus();
  }, [fetchGpuStatus, fetchHfStatus, fetchGatewayStatus]);

  const loading = gpuLoading || hfLoading || gatewayLoading;

  if (loading) {
    return <Loader title="Loading integrations..." />;
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: '24px', paddingBottom: '24px' }}>
      {/* NVIDIA GPU Operator */}
      <SectionBox title="NVIDIA GPU Operator">
        <div style={{ padding: '16px 0' }}>
          <p style={{ margin: '0 0 16px', opacity: 0.7 }}>
            Install the NVIDIA GPU Operator to enable GPU support in your cluster
          </p>

          {gpuError ? (
            <ConnectionError error={gpuError} onRetry={fetchGpuStatus} />
          ) : gpuLoading ? (
            <div style={{ display: 'flex', alignItems: 'center', gap: '8px', opacity: 0.7 }}>
              <InlineLoader />
              <span>Checking GPU Operator status...</span>
            </div>
          ) : gpuStatus?.installed ? (
            /* Installed state - show runtime-style status card */
            <div
              style={{
                border: '1px solid rgba(128, 128, 128, 0.3)',
                borderRadius: '8px',
                padding: '20px',
                backgroundColor: 'transparent',
              }}
            >
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: '12px' }}>
                <div>
                  <h3 style={{ margin: '0 0 4px 0', fontSize: '18px', textTransform: 'uppercase' }}>
                    GPU Status
                  </h3>
                  <p style={{ margin: 0, fontSize: '14px', opacity: 0.7 }}>
                    NVIDIA GPU Operator manages GPU drivers and device plugins
                  </p>
                </div>
                <StatusLabel status={gpuStatus.operatorRunning && gpuStatus.gpusAvailable ? 'success' : gpuStatus.operatorRunning ? 'warning' : 'error'}>
                  {gpuStatus.operatorRunning && gpuStatus.gpusAvailable ? 'GPUs Enabled' : gpuStatus.operatorRunning ? 'No GPUs' : 'Unhealthy'}
                </StatusLabel>
              </div>

              {/* Status details */}
              <div style={{ fontSize: '14px', marginBottom: '16px' }}>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '8px' }}>
                  <span style={{ opacity: 0.7 }}>Operator</span>
                  <StatusLabel status={gpuStatus.operatorRunning ? 'success' : 'error'}>
                    {gpuStatus.operatorRunning ? 'Running' : 'Not Running'}
                  </StatusLabel>
                </div>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '8px' }}>
                  <span style={{ opacity: 0.7 }}>GPUs Available</span>
                  <StatusLabel status={gpuStatus.gpusAvailable ? 'success' : 'warning'}>
                    {gpuStatus.gpusAvailable ? `${gpuStatus.totalGPUs} GPU(s)` : 'None'}
                  </StatusLabel>
                </div>
                {gpuStatus.gpuNodes && gpuStatus.gpuNodes.length > 0 && (
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', opacity: 0.7 }}>
                    <span>GPU Nodes</span>
                    <span style={{ fontFamily: 'monospace', fontSize: '12px' }}>{gpuStatus.gpuNodes.length} node(s)</span>
                  </div>
                )}
              </div>

              {/* GPU Node details if available */}
              {gpuStatus.gpusAvailable && gpuStatus.gpuNodes && gpuStatus.gpuNodes.length > 0 && (
                <div
                  style={{
                    padding: '12px',
                    backgroundColor: 'rgba(76, 175, 80, 0.1)',
                    borderRadius: '6px',
                    marginBottom: '16px',
                  }}
                >
                  <div style={{ display: 'flex', alignItems: 'center', gap: '8px', color: '#4caf50', marginBottom: '8px' }}>
                    <Icon icon="mdi:check-circle" />
                    <span style={{ fontWeight: 500 }}>
                      GPUs enabled: {gpuStatus.totalGPUs} GPU(s) on {gpuStatus.gpuNodes.length} node(s)
                    </span>
                  </div>
                  <div style={{ fontSize: '12px', fontFamily: 'monospace', opacity: 0.8 }}>
                    Nodes: {gpuStatus.gpuNodes.join(', ')}
                  </div>
                </div>
              )}
            </div>
          ) : (
            /* Not installed state - show install option */
            <div style={{ display: 'flex', flexDirection: 'column', gap: '16px' }}>
              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
                <div>
                  <div style={{ fontWeight: 500 }}>Enable GPU Operator</div>
                  <div style={{ fontSize: '13px', opacity: 0.7 }}>
                    Automatically installs the NVIDIA GPU Operator via Helm
                  </div>
                </div>

                <Button
                  variant="contained"
                  color="primary"
                  size="small"
                  startIcon={installing ? <InlineLoader /> : <Icon icon="mdi:download" />}
                  onClick={handleInstallGpu}
                  disabled={installing}
                >
                  {installing ? 'Installing...' : 'Install'}
                </Button>
              </div>

              {installing && (
                <div style={{ display: 'flex', alignItems: 'center', gap: '8px', opacity: 0.7 }}>
                  <InlineLoader />
                  <span>Installing GPU Operator... This may take several minutes.</span>
                </div>
              )}

              {/* Manual installation commands */}
              {gpuStatus?.helmCommands && gpuStatus.helmCommands.length > 0 && (
                <div style={{ marginTop: '8px' }}>
                  <div style={{ fontWeight: 500, marginBottom: '12px' }}>Manual Installation</div>
                  <div style={{ display: 'flex', flexDirection: 'column', gap: '8px' }}>
                    {gpuStatus.helmCommands.map((cmd, index) => (
                      <div
                        key={index}
                        style={{
                          display: 'flex',
                          alignItems: 'center',
                          gap: '8px',
                        }}
                      >
                        <code
                          style={{
                            flex: 1,
                            padding: '8px 12px',
                            backgroundColor: 'rgba(128, 128, 128, 0.1)',
                            borderRadius: '4px',
                            fontSize: '12px',
                            fontFamily: 'monospace',
                            overflowX: 'auto',
                          }}
                        >
                          {cmd}
                        </code>
                        <IconButton
                          color={copiedText === cmd ? 'success' : 'primary'}
                          onClick={() => handleCopy(cmd)}
                        >
                          <Icon icon={copiedText === cmd ? 'mdi:check' : 'mdi:content-copy'}/>
                        </IconButton>
                      </div>
                    ))}
                  </div>
                </div>
              )}
            </div>
          )}
        </div>
      </SectionBox>

      {/* Gateway API */}
      <SectionBox title="Gateway API">
        <div style={{ padding: '16px 0' }}>
          <p style={{ margin: '0 0 16px', opacity: 0.7 }}>
            Install Gateway API and Inference Extension CRDs to enable smart traffic routing for your models
          </p>

          {gatewayError ? (
            <ConnectionError error={gatewayError} onRetry={fetchGatewayStatus} />
          ) : gatewayLoading ? (
            <div style={{ display: 'flex', alignItems: 'center', gap: '8px', opacity: 0.7 }}>
              <InlineLoader />
              <span>Checking Gateway API status...</span>
            </div>
          ) : gatewayStatus?.gatewayAvailable ? (
            /* Available state - show status card */
            <div
              style={{
                border: '1px solid rgba(128, 128, 128, 0.3)',
                borderRadius: '8px',
                padding: '20px',
                backgroundColor: 'transparent',
              }}
            >
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: '12px' }}>
                <div>
                  <h3 style={{ margin: '0 0 4px 0', fontSize: '18px', textTransform: 'uppercase' }}>
                    Gateway Status
                  </h3>
                  <p style={{ margin: 0, fontSize: '14px', opacity: 0.7 }}>
                    Gateway API enables intelligent request routing to inference endpoints
                  </p>
                </div>
                <StatusLabel status="success">
                  Available
                </StatusLabel>
              </div>

              {/* Status details */}
              <div style={{ fontSize: '14px', marginBottom: '16px' }}>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '8px' }}>
                  <span style={{ opacity: 0.7 }}>Gateway API CRDs</span>
                  <StatusLabel status={gatewayStatus.gatewayApiInstalled ? 'success' : 'error'}>
                    {gatewayStatus.gatewayApiInstalled ? 'Installed' : 'Not Installed'}
                  </StatusLabel>
                </div>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '8px' }}>
                  <span style={{ opacity: 0.7 }}>Inference Extension CRDs</span>
                  <StatusLabel status={gatewayStatus.inferenceExtInstalled ? 'success' : 'error'}>
                    {gatewayStatus.inferenceExtInstalled ? 'Installed' : 'Not Installed'}
                  </StatusLabel>
                </div>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '8px' }}>
                  <span style={{ opacity: 0.7 }}>Pinned Version</span>
                  <span style={{ fontFamily: 'monospace', fontSize: '12px' }}>{gatewayStatus.pinnedVersion}</span>
                </div>
                {gatewayStatus.gatewayEndpoint && (
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', opacity: 0.7 }}>
                    <span>Gateway Endpoint</span>
                    <span style={{ fontFamily: 'monospace', fontSize: '12px' }}>{gatewayStatus.gatewayEndpoint}</span>
                  </div>
                )}
              </div>

              {/* Success banner */}
              <div
                style={{
                  padding: '12px',
                  backgroundColor: 'rgba(76, 175, 80, 0.1)',
                  borderRadius: '6px',
                }}
              >
                <div style={{ display: 'flex', alignItems: 'center', gap: '8px', color: '#4caf50' }}>
                  <Icon icon="mdi:check-circle" />
                  <span style={{ fontWeight: 500 }}>
                    Gateway API is ready for traffic routing
                  </span>
                </div>
              </div>
            </div>
          ) : (
            /* Not available state - show install option */
            <div style={{ display: 'flex', flexDirection: 'column', gap: '16px' }}>
              {/* CRD status badges */}
              <div style={{ fontSize: '14px' }}>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '8px' }}>
                  <span style={{ opacity: 0.7 }}>Gateway API CRDs</span>
                  <StatusLabel status={gatewayStatus?.gatewayApiInstalled ? 'success' : 'error'}>
                    {gatewayStatus?.gatewayApiInstalled ? 'Installed' : 'Not Installed'}
                  </StatusLabel>
                </div>
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '8px' }}>
                  <span style={{ opacity: 0.7 }}>Inference Extension CRDs</span>
                  <StatusLabel status={gatewayStatus?.inferenceExtInstalled ? 'success' : 'error'}>
                    {gatewayStatus?.inferenceExtInstalled ? 'Installed' : 'Not Installed'}
                  </StatusLabel>
                </div>
                {gatewayStatus?.pinnedVersion && (
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', opacity: 0.7 }}>
                    <span>Pinned Version</span>
                    <span style={{ fontFamily: 'monospace', fontSize: '12px' }}>{gatewayStatus.pinnedVersion}</span>
                  </div>
                )}
              </div>

              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between' }}>
                <div>
                  <div style={{ fontWeight: 500 }}>Install Gateway CRDs</div>
                  <div style={{ fontSize: '13px', opacity: 0.7 }}>
                    Installs Gateway API and Inference Extension custom resource definitions
                  </div>
                </div>

                <Button
                  variant="contained"
                  color="primary"
                  size="small"
                  startIcon={installingGateway ? <InlineLoader /> : <Icon icon="mdi:download" />}
                  onClick={handleInstallGatewayCRDs}
                  disabled={installingGateway}
                >
                  {installingGateway ? 'Installing...' : 'Install'}
                </Button>
              </div>

              {installingGateway && (
                <div style={{ display: 'flex', alignItems: 'center', gap: '8px', opacity: 0.7 }}>
                  <InlineLoader />
                  <span>Installing Gateway CRDs... This may take a moment.</span>
                </div>
              )}

              {/* Manual installation commands */}
              {gatewayStatus?.installCommands && gatewayStatus.installCommands.length > 0 && (
                <div style={{ marginTop: '8px' }}>
                  <div style={{ fontWeight: 500, marginBottom: '12px' }}>Manual Installation</div>
                  <div style={{ display: 'flex', flexDirection: 'column', gap: '8px' }}>
                    {gatewayStatus.installCommands.map((cmd, index) => (
                      <div
                        key={index}
                        style={{
                          display: 'flex',
                          alignItems: 'center',
                          gap: '8px',
                        }}
                      >
                        <code
                          style={{
                            flex: 1,
                            padding: '8px 12px',
                            backgroundColor: 'rgba(128, 128, 128, 0.1)',
                            borderRadius: '4px',
                            fontSize: '12px',
                            fontFamily: 'monospace',
                            overflowX: 'auto',
                          }}
                        >
                          {cmd}
                        </code>
                        <IconButton
                          color={copiedText === cmd ? 'success' : 'primary'}
                          onClick={() => handleCopy(cmd)}
                        >
                          <Icon icon={copiedText === cmd ? 'mdi:check' : 'mdi:content-copy'}/>
                        </IconButton>
                      </div>
                    ))}
                  </div>
                </div>
              )}
            </div>
          )}
        </div>
      </SectionBox>

      {/* HuggingFace Token */}
      <SectionBox title="HuggingFace Token">
        <div style={{ padding: '16px 0' }}>
          <p style={{ margin: '0 0 16px', opacity: 0.7 }}>
            Connect your HuggingFace account to access gated models like Llama
          </p>

          {hfError ? (
            <ConnectionError error={hfError} onRetry={fetchHfStatus} />
          ) : hfLoading ? (
            <div style={{ display: 'flex', alignItems: 'center', gap: '8px', opacity: 0.7 }}>
              <InlineLoader />
              <span>Checking HuggingFace connection...</span>
            </div>
          ) : hfStatus?.configured ? (
            // Connected state
            <div style={{ display: 'flex', flexDirection: 'column', gap: '16px' }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
                  {hfStatus.user?.avatarUrl ? (
                    <img
                      src={hfStatus.user.avatarUrl}
                      alt={hfStatus.user.name}
                      style={{
                        width: '40px',
                        height: '40px',
                        borderRadius: '50%',
                      }}
                    />
                  ) : (
                    <div
                      style={{
                        width: '40px',
                        height: '40px',
                        borderRadius: '50%',
                        backgroundColor: 'rgba(128, 128, 128, 0.2)',
                        display: 'flex',
                        alignItems: 'center',
                        justifyContent: 'center',
                      }}
                    >
                      🔑
                    </div>
                  )}
                  <div>
                    {hfStatus.user ? (
                      <>
                        <div style={{ fontWeight: 500 }}>
                          {hfStatus.user.fullname || hfStatus.user.name}
                        </div>
                        <div style={{ fontSize: '13px', opacity: 0.7 }}>
                          @{hfStatus.user.name}
                        </div>
                      </>
                    ) : (
                      <>
                        <div style={{ fontWeight: 500 }}>HuggingFace Token</div>
                        <div style={{ fontSize: '13px', opacity: 0.7 }}>Token configured</div>
                      </>
                    )}
                  </div>
                </div>
                <StatusLabel status="success">Connected</StatusLabel>
              </div>

              <div
                style={{
                  padding: '12px',
                  backgroundColor: 'rgba(76, 175, 80, 0.1)',
                  borderRadius: '6px',
                  display: 'flex',
                  alignItems: 'center',
                  gap: '8px',
                  color: '#4caf50',
                  alignSelf: 'flex-start',
                }}
              >
                <Icon icon="mdi:check-circle" />
                <span>
                  Token saved in {hfStatus.namespaces.filter((n) => n.exists).length} namespace(s)
                </span>
              </div>

              <Button
                onClick={handleDisconnectHf}
                variant="outlined"
                color="error"
                startIcon={<Icon icon="mdi:link-off" />}
                disabled={disconnectingHf}
                style={{ alignSelf: 'flex-start' }}
              >
                {disconnectingHf ? 'Disconnecting...' : 'Disconnect HuggingFace'}
              </Button>
            </div>
          ) : (
            // Not connected state - show manual setup instructions
            <div style={{ display: 'flex', flexDirection: 'column', gap: '16px' }}>
              <p style={{ margin: 0, fontSize: '14px', opacity: 0.8 }}>
                Create a HuggingFace token at{' '}
                <a
                  href="https://huggingface.co/settings/tokens"
                  target="_blank"
                  rel="noopener noreferrer"
                  style={{ color: '#1976d2' }}
                >
                  huggingface.co/settings/tokens
                </a>{' '}
                and run the following commands to configure access to gated models:
              </p>

              {/* Manual setup commands */}
              <div style={{ display: 'flex', flexDirection: 'column', gap: '8px' }}>
                {[
                  'export HF_TOKEN=hf_your_token_here',
                  'kubectl create secret generic hf-secret --from-literal=hf_token=$HF_TOKEN -n kaito-workspace',
                ].map((cmd, index) => (
                  <div
                    key={index}
                    style={{
                      display: 'flex',
                      alignItems: 'center',
                      gap: '8px',
                    }}
                  >
                    <code
                      style={{
                        flex: 1,
                        padding: '8px 12px',
                        backgroundColor: 'rgba(128, 128, 128, 0.1)',
                        borderRadius: '4px',
                        fontSize: '12px',
                        fontFamily: 'monospace',
                        overflowX: 'auto',
                      }}
                    >
                      {cmd}
                    </code>
                    <IconButton
                      color={copiedText === cmd ? 'success' : 'primary'}
                      onClick={() => handleCopy(cmd)}
                    >
                      <Icon icon={copiedText === cmd ? 'mdi:check' : 'mdi:content-copy'} />
                    </IconButton>
                  </div>
                ))}
              </div>

              {/* TODO: Re-enable OAuth sign-in once backend is ready
              <Button
                onClick={handleConnectHf}
                disabled={connectingHf}
                variant="contained"
                style={{
                  backgroundColor: '#FFD21E',
                  color: 'black',
                  alignSelf: 'flex-start',
                }}
              >
                {connectingHf ? 'Connecting...' : <><span>🤗</span> Sign in with Hugging Face</>}
              </Button>
              */}
            </div>
          )}
        </div>
      </SectionBox>
    </div>
  );
}
