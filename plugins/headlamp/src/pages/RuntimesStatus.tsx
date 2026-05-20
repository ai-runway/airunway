/**
 * Runtimes Status Page
 *
 * Shows installation status and health of KAITO, KubeRay, and Dynamo runtimes.
 */

import { useState, useEffect, useCallback } from 'react';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import { Icon } from '@iconify/react';
import {
  SectionBox,
  Loader,
  StatusLabel,
  StatusLabelProps,
} from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import { useApiClient } from '../lib/api-client';
import type { RuntimesStatusResponse, RuntimeStatus } from '@airunway/shared';
import { ConnectionError } from '../components/ConnectionBanner';

function getStatusColor(runtime: RuntimeStatus): StatusLabelProps['status'] {
  if (runtime.healthy) return 'success';
  if (runtime.installed) return 'warning'; // CRD exists but operator not running
  return 'error';
}

function getStatusText(runtime: RuntimeStatus): string {
  if (runtime.healthy) return 'Healthy';
  if (runtime.installed) return 'Unhealthy'; // CRD exists but operator not running
  return 'Not Installed';
}

export function RuntimesStatus() {
  const api = useApiClient();

  const [runtimesStatus, setRuntimesStatus] = useState<RuntimesStatusResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [installing, setInstalling] = useState<string | null>(null);
  const [uninstalling, setUninstalling] = useState<string | null>(null);

  // Fetch runtimes status
  const fetchStatus = useCallback(async () => {
    setLoading(true);
    setError(null);

    try {
      const result = await api.runtimes.getStatus();
      setRuntimesStatus(result);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch runtimes status');
    } finally {
      setLoading(false);
    }
  }, [api]);

  // Install runtime
  const handleInstall = useCallback(
    async (runtimeId: string) => {
      setInstalling(runtimeId);

      try {
        await api.installation.installProvider(runtimeId);
        // Refresh status after install
        await fetchStatus();
      } catch (err) {
        alert(`Failed to install ${runtimeId}: ${err instanceof Error ? err.message : 'Unknown error'}`);
      } finally {
        setInstalling(null);
      }
    },
    [api, fetchStatus]
  );

  // Uninstall runtime
  const handleUninstall = useCallback(
    async (runtimeId: string) => {
      setUninstalling(runtimeId);

      try {
        const result = await api.installation.uninstallProvider(runtimeId);
        if (result.success) {
          // Refresh status after uninstall
          await fetchStatus();
        } else {
          alert(`Uninstall failed: ${result.message}`);
        }
      } catch (err) {
        alert(`Failed to uninstall ${runtimeId}: ${err instanceof Error ? err.message : 'Unknown error'}`);
      } finally {
        setUninstalling(null);
      }
    },
    [api, fetchStatus]
  );

  // Initial fetch
  useEffect(() => {
    fetchStatus();
  }, [fetchStatus]);

  // Auto-refresh every 30 seconds
  useEffect(() => {
    const interval = setInterval(fetchStatus, 30000);
    return () => clearInterval(interval);
  }, [fetchStatus]);

  if (loading) {
    return <Loader title="Loading runtimes status..." />;
  }

  if (error) {
    return (
      <SectionBox title="Runtimes">
        <ConnectionError error={error} onRetry={fetchStatus} />
      </SectionBox>
    );
  }

  const runtimes = runtimesStatus?.runtimes || [];

  return (
    <SectionBox
      title="Runtime Status"
      headerProps={{
        actions: [
          <Tooltip key="refresh" title="Refresh">
            <IconButton onClick={fetchStatus} size="small">
              <Icon icon="mdi:refresh" />
            </IconButton>
          </Tooltip>,
        ],
      }}
    >
      <div style={{ display: 'grid', gap: '16px' }}>
        {runtimes.map((runtime) => (
          <div
            key={runtime.id}
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
                  {runtime.name}
                </h3>
              </div>
              <StatusLabel status={getStatusColor(runtime)}>
                {getStatusText(runtime)}
              </StatusLabel>
            </div>

            {/* Status details - CRD and Operator */}
            <div style={{ fontSize: '14px', marginBottom: '16px' }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '8px' }}>
                <span style={{ opacity: 0.7 }}>CRD</span>
                <StatusLabel status={runtime.installed ? 'success' : 'error'}>
                  {runtime.installed ? 'Installed' : 'Not Installed'}
                </StatusLabel>
              </div>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '8px' }}>
                <span style={{ opacity: 0.7 }}>Operator</span>
                <StatusLabel status={runtime.healthy ? 'success' : 'error'}>
                  {runtime.healthy ? 'Running' : 'Not Running'}
                </StatusLabel>
              </div>
              {runtime.version && (
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', opacity: 0.7 }}>
                  <span>Version</span>
                  <span style={{ fontFamily: 'monospace', fontSize: '12px' }}>{runtime.version}</span>
                </div>
              )}
            </div>

            {/* Actions */}
            <div style={{ display: 'flex', gap: '8px' }}>
              {/* Show Install button if not fully installed (CRD missing or operator not running) */}
              {!runtime.healthy && (
                <Button
                  variant="contained"
                  color="primary"
                  size="small"
                  startIcon={<Icon icon="mdi:download" />}
                  onClick={() => handleInstall(runtime.id)}
                  disabled={installing === runtime.id}
                >
                  {installing === runtime.id ? 'Deploying...' : 'Deploy'}
                </Button>
              )}
              {/* Show Upgrade and Uninstall only when fully healthy */}
              {runtime.healthy && (
                <>
                  <Button
                    variant="contained"
                    color="success"
                    size="small"
                    startIcon={<Icon icon="mdi:arrow-up-circle" />}
                    sx={{ fontWeight: 600, boxShadow: 3, alignSelf: 'flex-start', mt: 'auto', display: 'flex', alignItems: 'center' }}
                    onClick={() => handleInstall(runtime.id)}
                    disabled={installing === runtime.id || uninstalling === runtime.id}
                  >
                    {installing === runtime.id ? 'Upgrading...' : 'Upgrade'}
                  </Button>
                  <Button
                    variant="contained"
                    color="error"
                    size="small"
                    startIcon={<Icon icon="mdi:trash" />}
                    onClick={() => handleUninstall(runtime.id)}
                    disabled={uninstalling === runtime.id || installing === runtime.id}
                  >
                    {uninstalling === runtime.id ? 'Uninstalling...' : 'Uninstall'}
                  </Button>
                </>
              )}
            </div>
          </div>
        ))}
      </div>

      {runtimes.length === 0 && (
        <div style={{ padding: '24px', textAlign: 'center', opacity: 0.7 }}>
          No runtimes found. Check your connection to the AI Runway backend.
        </div>
      )}
    </SectionBox>
  );
}
