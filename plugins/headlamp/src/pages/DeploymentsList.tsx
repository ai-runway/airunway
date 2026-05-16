/**
 * Deployments List Page
 *
 * Displays all deployments across namespaces with filtering and status.
 */

import { useState, useEffect, useCallback, useMemo } from 'react';
import {
  SectionBox,
  SectionFilterHeader,
  SimpleTable,
  Link as HeadlampLink,
  Loader,
  StatusLabel,
} from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import { Utils } from '@kinvolk/headlamp-plugin/lib';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import { Icon } from '@iconify/react';
import { useApiClient } from '../lib/api-client';
import type { DeploymentStatus } from '@airunway/shared';
import { ConnectionError } from '../components/ConnectionBanner';
import { getDeploymentPhaseColor, formatAge } from '../lib/utils';

export function DeploymentsList() {
  const api = useApiClient();
  const [deployments, setDeployments] = useState<DeploymentStatus[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [refreshKey, setRefreshKey] = useState(0);

  // Use Headlamp's filter function for namespace/search filtering
  const filterFunc = Utils.useFilterFunc<DeploymentStatus>(['$.name', '$.namespace', '$.modelId', '$.provider', '$.engine']);

  // Fetch all deployments (filtering happens on frontend using Headlamp's filter)
  const fetchDeployments = useCallback(async () => {
    setLoading(true);
    setError(null);

    try {
      const result = await api.deployments.list();
      setDeployments(result.deployments);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch deployments');
      setDeployments([]);
    } finally {
      setLoading(false);
    }
  }, [api]);

  // Filter deployments based on Headlamp's global filter state
  const filteredDeployments = useMemo(() => {
    return deployments.filter(filterFunc);
  }, [deployments, filterFunc]);

  // Initial fetch and refresh
  useEffect(() => {
    fetchDeployments();
  }, [fetchDeployments, refreshKey]);

  // Auto-refresh every 30 seconds
  useEffect(() => {
    const interval = setInterval(() => {
      setRefreshKey((k) => k + 1);
    }, 30000);

    return () => clearInterval(interval);
  }, []);

  // Handle delete
  const handleDelete = useCallback(
    async (name: string, ns: string) => {
      if (!confirm(`Are you sure you want to delete deployment "${name}"?`)) {
        return;
      }

      try {
        await api.deployments.delete(name, ns);
        setRefreshKey((k) => k + 1);
      } catch (err) {
        alert(`Failed to delete deployment: ${err instanceof Error ? err.message : 'Unknown error'}`);
      }
    },
    [api]
  );

  // Table columns
  const columns = [
    {
      label: 'Name',
      getter: (item: DeploymentStatus) => (
        <div style={{ display: 'flex', alignItems: 'center', height: '100%' }}>
          <HeadlampLink routeName="Deployment Details" params={{ name: item.name, namespace: item.namespace }}>
            {item.name}
          </HeadlampLink>
        </div>
      ),
    },
    {
      label: 'Namespace',
      getter: (item: DeploymentStatus) => (
        <div style={{ display: 'flex', alignItems: 'center', height: '100%' }}>
          {item.namespace}
        </div>
      ),
    },
    {
      label: 'Model',
      getter: (item: DeploymentStatus) => (
        <div style={{ display: 'flex', alignItems: 'center', height: '100%' }}>
          {item.modelId ? <StatusLabel status="">{item.modelId}</StatusLabel> : '-'}
        </div>
      ),
    },
    {
      label: 'Provider',
      getter: (item: DeploymentStatus) => (
        <div style={{ display: 'flex', alignItems: 'center', height: '100%' }}>
          <StatusLabel status="">{item.provider}</StatusLabel>
        </div>
      ),
    },
    {
      label: 'Status',
      getter: (item: DeploymentStatus) => (
        <div style={{ display: 'flex', alignItems: 'center', height: '100%' }}>
          <StatusLabel status={getDeploymentPhaseColor(item.phase)}>
            {item.phase}
          </StatusLabel>
        </div>
      ),
    },
    {
      label: 'Replicas',
      getter: (item: DeploymentStatus) => (
        <div style={{ display: 'flex', alignItems: 'center', height: '100%' }}>
          {`${item.replicas?.ready || 0}/${item.replicas?.desired || 1}`}
        </div>
      ),
    },
    {
      label: 'Engine',
      getter: (item: DeploymentStatus) => (
        <div style={{ display: 'flex', alignItems: 'center', height: '100%' }}>
          {item.engine ? <StatusLabel status="">{item.engine}</StatusLabel> : '-'}
        </div>
      ),
    },
    {
      label: 'Gateway',
      getter: (item: DeploymentStatus) => (
        <div style={{ display: 'flex', alignItems: 'center', height: '100%' }}>
          {item.gateway?.endpoint
            ? <code style={{ fontSize: '12px' }}>{item.gateway.modelName || '-'}</code>
            : <span style={{ opacity: 0.5 }}>-</span>}
        </div>
      ),
    },
    {
      label: 'Age',
      getter: (item: DeploymentStatus) => {
        if (!item.createdAt) return <div style={{ display: 'flex', alignItems: 'center', height: '100%' }}>-</div>;
        return <div style={{ display: 'flex', alignItems: 'center', height: '100%' }}>{formatAge(item.createdAt)}</div>;
      },
    },
    {
      label: 'Actions',
      getter: (item: DeploymentStatus) => (
        <div style={{ display: 'flex', gap: '4px', alignItems: 'center', height: '100%' }}>
          <Tooltip title="View">
            <IconButton
              component={HeadlampLink}
              routeName="Deployment Details"
              params={{ name: item.name, namespace: item.namespace }}
              color="primary"
              size="small"
            >
              <Icon icon="mdi:eye" />
            </IconButton>
          </Tooltip>
          <Tooltip title="Delete">
            <IconButton
              color="error"
              size="small"
              onClick={() => handleDelete(item.name, item.namespace)}
            >
              <Icon icon="mdi:trash-can" />
            </IconButton>
          </Tooltip>
        </div>
      ),
    },
  ];

  return (
    <SectionBox
      title={
        <SectionFilterHeader
          title={
            <span style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
              Deployments
              <Tooltip title="Create Deployment">
                <IconButton
                  component={HeadlampLink}
                  routeName="Create Deployment"
                  size="small"
                  color="primary"
                >
                  <Icon icon="mdi:plus-circle" />
                </IconButton>
              </Tooltip>
            </span>
          }
          actions={[
            <Tooltip key="refresh" title="Refresh">
              <IconButton
                onClick={() => setRefreshKey((k) => k + 1)}
                size="small"
              >
                <Icon icon="mdi:refresh" />
              </IconButton>
            </Tooltip>,
          ]}
        />
      }
    >
      {loading ? (
        <Loader title="Loading deployments..." />
      ) : error ? (
        <ConnectionError error={error} onRetry={fetchDeployments} />
      ) : filteredDeployments.length === 0 ? (
        <div style={{ padding: '24px', textAlign: 'center', opacity: 0.7 }}>
          <p>No deployments found.</p>
          <HeadlampLink routeName="Create Deployment" style={{ color: '#1976d2' }}>
            Create your first deployment
          </HeadlampLink>
        </div>
      ) : (
        <SimpleTable
          columns={columns}
          data={filteredDeployments}
          rowsPerPage={[10, 25, 50]}
        />
      )}
    </SectionBox>
  );
}
