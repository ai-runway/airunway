/**
 * Gateway Status Page
 *
 * Shows Gateway API Inference Extension status, endpoint, and model routing table.
 */

import { useState, useEffect, useCallback } from 'react';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import { Icon } from '@iconify/react';
import {
  SectionBox,
  SimpleTable,
  Loader,
  StatusLabel,
  StatusLabelProps,
} from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import type { GatewayInfo, GatewayModelInfo } from '@airunway/shared';
import { useApiClient } from '../lib/api-client';
import { ConnectionError } from '../components/ConnectionBanner';

function getModelStatusColor(ready: boolean): StatusLabelProps['status'] {
  return ready ? 'success' : 'error';
}

export function GatewayStatus() {
  const api = useApiClient();
  const [gatewayInfo, setGatewayInfo] = useState<GatewayInfo | null>(null);
  const [models, setModels] = useState<GatewayModelInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const fetchData = useCallback(async (isInitial = false) => {
    if (isInitial) {
      setLoading(true);
    } else {
      setRefreshing(true);
    }
    setError(null);

    try {
      const [status, gatewayModels] = await Promise.all([
        api.gateway.getInfo(),
        api.gateway.getModels(),
      ]);
      setGatewayInfo(status);
      setModels(gatewayModels);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch gateway status');
    } finally {
      setLoading(false);
      setRefreshing(false);
    }
  }, [api]);

  // Initial fetch
  useEffect(() => {
    fetchData(true);
  }, [fetchData]);

  // Auto-refresh every 30 seconds
  useEffect(() => {
    const interval = setInterval(() => fetchData(false), 30000);
    return () => clearInterval(interval);
  }, [fetchData]);

  const handleManualRefresh = useCallback(() => fetchData(false), [fetchData]);

  if (loading) {
    return <Loader title="Loading gateway status..." />;
  }

  if (error) {
    return (
      <SectionBox title="Gateway">
        <ConnectionError error={error} onRetry={handleManualRefresh} />
      </SectionBox>
    );
  }

  const available = gatewayInfo?.available ?? false;
  const endpoint = gatewayInfo?.endpoint;
  const modelCount = models.length;

  return (
    <>
      {/* Overview section */}
      <SectionBox
        title="Gateway Status"
        headerProps={{
          actions: [
            <Tooltip key="refresh" title={refreshing ? 'Refreshing…' : 'Refresh'}>
              <span>
                <IconButton onClick={handleManualRefresh} size="small" disabled={refreshing}>
                  <Icon icon="mdi:refresh" />
                </IconButton>
              </span>
            </Tooltip>,
          ],
        }}
      >
        <div
          style={{
            border: '1px solid rgba(128, 128, 128, 0.3)',
            borderRadius: '8px',
            padding: '20px',
            backgroundColor: 'transparent',
          }}
        >
          {/* Availability */}
          <div
            style={{
              display: 'flex',
              justifyContent: 'space-between',
              alignItems: 'center',
              marginBottom: '12px',
            }}
          >
            <span style={{ opacity: 0.7 }}>Availability</span>
            <StatusLabel status={available ? 'success' : 'error'}>
              {available ? 'Available' : 'Not Available'}
            </StatusLabel>
          </div>

          {/* Endpoint */}
          <div
            style={{
              display: 'flex',
              justifyContent: 'space-between',
              alignItems: 'center',
              marginBottom: '12px',
            }}
          >
            <span style={{ opacity: 0.7 }}>Endpoint</span>
            <span
              style={{
                fontFamily: 'monospace',
                fontSize: '13px',
              }}
            >
              {endpoint || '—'}
            </span>
          </div>

          {/* Routed models count */}
          <div
            style={{
              display: 'flex',
              justifyContent: 'space-between',
              alignItems: 'center',
            }}
          >
            <span style={{ opacity: 0.7 }}>Routed Models</span>
            <span style={{ fontWeight: 500 }}>{modelCount}</span>
          </div>
        </div>
      </SectionBox>

      {/* Model routing table */}
      <SectionBox title="Model Routing">
        {modelCount === 0 ? (
          <div style={{ padding: '24px', textAlign: 'center', opacity: 0.7 }}>
            No models are currently routed through the gateway.
          </div>
        ) : (
          <SimpleTable
            columns={[
              {
                label: 'Model Name',
                getter: (row: GatewayModelInfo) => row.name,
              },
              {
                label: 'Deployment',
                getter: (row: GatewayModelInfo) => row.deploymentName,
              },
              {
                label: 'Provider',
                getter: (row: GatewayModelInfo) => row.provider || '—',
              },
              {
                label: 'Status',
                getter: (row: GatewayModelInfo) => (
                  <StatusLabel status={getModelStatusColor(row.ready)}>
                    {row.ready ? 'Ready' : 'Not Ready'}
                  </StatusLabel>
                ),
              },
            ]}
            data={models}
          />
        )}
      </SectionBox>
    </>
  );
}
