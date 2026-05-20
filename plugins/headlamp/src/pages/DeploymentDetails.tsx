/**
 * Deployment Details Page
 *
 * Shows detailed information about a specific deployment including
 * status, pods, conditions, metrics, and logs.
 * Matches the native UI layout with status cards and access info.
 */

import { useState, useEffect, useCallback, useRef } from 'react';
import { useParams, useHistory } from 'react-router-dom';
import {
  SectionBox,
  SimpleTable,
  Loader,
  StatusLabel,
  Tabs,
} from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import { Router } from '@kinvolk/headlamp-plugin/lib';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import { Icon } from '@iconify/react';
import { buildPortForwardCommand } from '@airunway/shared';
import { useApiClient } from '../lib/api-client';
import type { DeploymentStatus, PodStatus, MetricsResponse, PodLogsResponse } from '@airunway/shared';
import { MetricsPanel } from '../components/MetricsPanel';
import { LogsViewer } from '../components/LogsViewer';
import { ConditionsTable } from '../components/ConditionsTable';
import { StorageVolumesDisplay } from '../components/StorageVolumesDisplay';
import { ConnectionError } from '../components/ConnectionBanner';
import { DeleteDialog } from '../components/DeleteDialog';
import { generateAynaUrl, getDeploymentPhaseColor, copyToClipboard, formatRelativeTime } from '../lib/utils';

export function DeploymentDetails() {
  const { name, namespace } = useParams<{ name: string; namespace: string }>();
  const api = useApiClient();
  const history = useHistory();

  const [deployment, setDeployment] = useState<DeploymentStatus | null>(null);
  const [pods, setPods] = useState<PodStatus[]>([]);
  const [metrics, setMetrics] = useState<MetricsResponse | null>(null);
  const [logs, setLogs] = useState<PodLogsResponse | null>(null);
  const [selectedPod, setSelectedPod] = useState<string | null>(null);

  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [activeTab, setActiveTab] = useState<'overview' | 'pods' | 'metrics' | 'logs' | 'conditions' | 'storage'>('overview');
  const [showDeleteDialog, setShowDeleteDialog] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [copied, setCopied] = useState(false);
  const copyTimerRef = useRef<ReturnType<typeof setTimeout>>();

  // Fetch deployment details
  const fetchDetails = useCallback(async (isInitialLoad = false) => {
    if (!name || !namespace) return;

    if (isInitialLoad) {
      setLoading(true);
    }
    setError(null);

    try {
      const [deploymentData, podsData] = await Promise.all([
        api.deployments.get(name, namespace),
        api.deployments.getPods(name, namespace),
      ]);

      setDeployment(deploymentData);
      setPods(podsData.pods);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch deployment');
    } finally {
      if (isInitialLoad) {
        setLoading(false);
      }
    }
  }, [api, name, namespace]);

  // Select first pod by default when pods first load
  useEffect(() => {
    if (pods.length > 0 && !selectedPod) {
      setSelectedPod(pods[0].name);
    }
  }, [pods, selectedPod]);

  // Fetch metrics
  const fetchMetrics = useCallback(async () => {
    if (!name || !namespace) return;

    try {
      const metricsData = await api.deployments.getMetrics(name, namespace);
      setMetrics(metricsData);
    } catch (err) {
      console.warn('Failed to fetch metrics:', err);
    }
  }, [api, name, namespace]);

  // Fetch logs
  const fetchLogs = useCallback(async () => {
    if (!name || !namespace) return;

    try {
      const logsData = await api.deployments.getLogs(name, namespace, {
        podName: selectedPod || undefined,
        tailLines: 200,
      });
      setLogs(logsData);
    } catch (err) {
      console.warn('Failed to fetch logs:', err);
    }
  }, [api, name, namespace, selectedPod]);

  // Delete deployment
  const handleDelete = useCallback(async () => {
    if (!deployment) return;

    setDeleting(true);
    try {
      await api.deployments.delete(deployment.name, deployment.namespace);
      history.push(Router.createRouteURL('AI Runway Deployments'));
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to delete deployment');
    } finally {
      setDeleting(false);
      setShowDeleteDialog(false);
    }
  }, [api, deployment, history]);

  // Copy port-forward command
  const copyPortForwardCommand = useCallback(async () => {
    if (!deployment) return;
    const command = buildPortForwardCommand(deployment);
    const success = await copyToClipboard(command);
    if (success) {
      setCopied(true);
      clearTimeout(copyTimerRef.current);
      copyTimerRef.current = setTimeout(() => setCopied(false), 2000);
    }
  }, [deployment]);

  useEffect(() => {
    return () => clearTimeout(copyTimerRef.current);
  }, []);

  // Initial fetch
  useEffect(() => {
    fetchDetails(true);
  }, [fetchDetails]);

  // Fetch metrics when tab is active
  useEffect(() => {
    if (activeTab === 'metrics') {
      fetchMetrics();
    }
  }, [activeTab, fetchMetrics]);

  // Fetch logs when tab is active
  useEffect(() => {
    if (activeTab === 'logs') {
      fetchLogs();
    }
  }, [activeTab, fetchLogs]);

  // Auto-refresh
  useEffect(() => {
    const interval = setInterval(() => {
      fetchDetails();
      if (activeTab === 'metrics') fetchMetrics();
    }, 15000);

    return () => clearInterval(interval);
  }, [fetchDetails, fetchMetrics, activeTab]);

  if (loading) {
    return <Loader title="Loading deployment details..." />;
  }

  if (error || !deployment) {
    return (
      <SectionBox title="Error">
        <ConnectionError error={error || 'Deployment not found'} onRetry={fetchDetails} />
      </SectionBox>
    );
  }

  // Generate port-forward command
  const portForwardCommand = buildPortForwardCommand(deployment);

  // Tab content components
  const OverviewContent = (
    <div style={{ display: 'grid', gap: '12px', maxWidth: '600px' }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', padding: '8px 0', borderBottom: '1px solid rgba(128, 128, 128, 0.2)' }}>
        <span style={{ opacity: 0.7 }}>Model</span>
        <span style={{ fontFamily: 'monospace' }}>{deployment.modelId || '-'}</span>
      </div>
      {deployment.servedModelName && deployment.servedModelName !== deployment.modelId && (
        <div style={{ display: 'flex', justifyContent: 'space-between', padding: '8px 0', borderBottom: '1px solid rgba(128, 128, 128, 0.2)' }}>
          <span style={{ opacity: 0.7 }}>Served Model Name</span>
          <span style={{ fontFamily: 'monospace' }}>{deployment.servedModelName}</span>
        </div>
      )}
      <div style={{ display: 'flex', justifyContent: 'space-between', padding: '8px 0', borderBottom: '1px solid rgba(128, 128, 128, 0.2)' }}>
        <span style={{ opacity: 0.7 }}>Provider</span>
        <span>{deployment.provider}</span>
      </div>
      <div style={{ display: 'flex', justifyContent: 'space-between', padding: '8px 0', borderBottom: '1px solid rgba(128, 128, 128, 0.2)' }}>
        <span style={{ opacity: 0.7 }}>Engine</span>
        <span>{deployment.engine || '-'}</span>
      </div>
      <div style={{ display: 'flex', justifyContent: 'space-between', padding: '8px 0', borderBottom: '1px solid rgba(128, 128, 128, 0.2)' }}>
        <span style={{ opacity: 0.7 }}>Mode</span>
        <span style={{ textTransform: 'capitalize' }}>{deployment.mode || '-'}</span>
      </div>
      <div style={{ display: 'flex', justifyContent: 'space-between', padding: '8px 0', borderBottom: '1px solid rgba(128, 128, 128, 0.2)' }}>
        <span style={{ opacity: 0.7 }}>Replicas</span>
        <span>{deployment.replicas?.ready || 0}/{deployment.replicas?.desired || 1}</span>
      </div>
      {deployment.prefillReplicas && (
        <div style={{ display: 'flex', justifyContent: 'space-between', padding: '8px 0', borderBottom: '1px solid rgba(128, 128, 128, 0.2)' }}>
          <span style={{ opacity: 0.7 }}>Prefill Replicas</span>
          <span>{deployment.prefillReplicas.ready}/{deployment.prefillReplicas.desired}</span>
        </div>
      )}
      {deployment.decodeReplicas && (
        <div style={{ display: 'flex', justifyContent: 'space-between', padding: '8px 0', borderBottom: '1px solid rgba(128, 128, 128, 0.2)' }}>
          <span style={{ opacity: 0.7 }}>Decode Replicas</span>
          <span>{deployment.decodeReplicas.ready}/{deployment.decodeReplicas.desired}</span>
        </div>
      )}
      <div style={{ display: 'flex', justifyContent: 'space-between', padding: '8px 0', borderBottom: '1px solid rgba(128, 128, 128, 0.2)' }}>
        <span style={{ opacity: 0.7 }}>Created</span>
        <span>{deployment.createdAt ? new Date(deployment.createdAt).toLocaleString() : '-'}</span>
      </div>
      <div style={{ display: 'flex', justifyContent: 'space-between', padding: '8px 0', borderBottom: '1px solid rgba(128, 128, 128, 0.2)' }}>
        <span style={{ opacity: 0.7 }}>Frontend Service</span>
        <span style={{ fontFamily: 'monospace' }}>{deployment.frontendService || '-'}</span>
      </div>
    </div>
  );

  const PodsContent = (
    <SimpleTable
      columns={[
        { label: 'Name', getter: (pod: PodStatus) => pod.name },
        { label: 'Status', getter: (pod: PodStatus) => (
          <StatusLabel status={getDeploymentPhaseColor(pod.phase)}>
            {pod.phase}
          </StatusLabel>
        )},
        { label: 'Node', getter: (pod: PodStatus) => pod.node || '-' },
        { label: 'Ready', getter: (pod: PodStatus) => pod.ready ? 'Yes' : 'No' },
        { label: 'Restarts', getter: (pod: PodStatus) => String(pod.restarts || 0) },
      ]}
      data={pods}
    />
  );

  const MetricsContent = (
    <MetricsPanel metrics={metrics} onRefresh={fetchMetrics} />
  );

  const LogsContent = (
    <LogsViewer
      logs={logs}
      pods={pods}
      selectedPod={selectedPod}
      onSelectPod={setSelectedPod}
      onRefresh={fetchLogs}
    />
  );

  const ConditionsContent = deployment.conditions ? (
    <ConditionsTable conditions={deployment.conditions} />
  ) : (
    <div style={{ padding: '16px', opacity: 0.7, textAlign: 'center' }}>No conditions available</div>
  );

  const StorageContent = deployment.storage?.volumes?.length ? (
    <StorageVolumesDisplay volumes={deployment.storage.volumes} />
  ) : null;

  const tabIds: Array<typeof activeTab> = ['overview', 'pods', 'metrics', 'logs', 'conditions'];
  if (StorageContent) tabIds.push('storage');

  const tabs = [
    { label: 'Overview', component: OverviewContent },
    { label: `Pods (${pods.length})`, component: PodsContent },
    { label: 'Metrics', component: MetricsContent },
    { label: 'Logs', component: LogsContent },
    { label: 'Conditions', component: ConditionsContent },
    ...(StorageContent ? [{ label: 'Storage', component: StorageContent }] : []),
  ];

  return (
    <div style={{ paddingTop: '16px', paddingBottom: '24px' }}>
      {/* Header with back button and actions */}
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: '24px' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '16px' }}>
          <Tooltip title="Back to Deployments">
            <IconButton
              onClick={() => history.push(Router.createRouteURL('AI Runway Deployments'))}
              size="small"
              sx={{
                border: '1px solid rgba(128, 128, 128, 0.3)',
                borderRadius: '8px',
              }}
            >
              <Icon icon="mdi:arrow-left" />
            </IconButton>
          </Tooltip>
          <div>
            <h1 style={{ margin: 0, fontSize: '24px', fontWeight: 600 }}>{deployment.name}</h1>
            <div style={{ fontSize: '14px', opacity: 0.7, marginTop: '4px' }}>
              {deployment.namespace} • Created {formatRelativeTime(deployment.createdAt)}
            </div>
          </div>
        </div>

        <Button
          onClick={() => setShowDeleteDialog(true)}
          variant="contained"
          color="error"
          startIcon={<Icon icon="mdi:trash-can" />}
        >
          Delete
        </Button>
      </div>

      {/* Status Card */}
      <div style={{
        border: '1px solid rgba(128, 128, 128, 0.3)',
        borderRadius: '8px',
        padding: '20px',
        marginBottom: '24px',
      }}>
        <h3 style={{ margin: 0, marginBottom: '16px' }}>Status</h3>
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(120px, 1fr))', gap: '24px' }}>
          <div>
            <div style={{ fontSize: '12px', opacity: 0.7, marginBottom: '4px' }}>Phase</div>
            <StatusLabel status={getDeploymentPhaseColor(deployment.phase)}>
              {deployment.phase}
            </StatusLabel>
          </div>
          <div>
            <div style={{ fontSize: '12px', opacity: 0.7, marginBottom: '4px' }}>Runtime</div>
            <span style={{
              padding: '2px 8px',
              backgroundColor: 'rgba(128, 128, 128, 0.15)',
              borderRadius: '4px',
              fontSize: '12px',
            }}>
              {deployment.provider}
            </span>
          </div>
          <div>
            <div style={{ fontSize: '12px', opacity: 0.7, marginBottom: '4px' }}>Replicas</div>
            <div style={{ fontWeight: 500 }}>
              {deployment.replicas?.ready || 0}/{deployment.replicas?.desired || 1} Ready
            </div>
          </div>
          <div>
            <div style={{ fontSize: '12px', opacity: 0.7, marginBottom: '4px' }}>Engine</div>
            <span style={{
              padding: '2px 8px',
              border: '1px solid rgba(128, 128, 128, 0.3)',
              borderRadius: '4px',
              fontSize: '12px',
            }}>
              {(deployment.engine || 'vllm').toUpperCase()}
            </span>
          </div>
          <div>
            <div style={{ fontSize: '12px', opacity: 0.7, marginBottom: '4px' }}>Mode</div>
            <div style={{ fontWeight: 500, textTransform: 'capitalize' }}>{deployment.mode || 'aggregated'}</div>
          </div>
          {deployment.gateway?.endpoint && (
            <div>
              <div style={{ fontSize: '12px', opacity: 0.7, marginBottom: '4px' }}>Gateway Endpoint</div>
              <code style={{ fontSize: '12px' }}>{deployment.gateway.endpoint}</code>
            </div>
          )}
          {deployment.gateway?.modelName && (
            <div>
              <div style={{ fontSize: '12px', opacity: 0.7, marginBottom: '4px' }}>Gateway Model Name</div>
              <span style={{ fontSize: '13px' }}>{deployment.gateway.modelName}</span>
            </div>
          )}
        </div>
      </div>

      {/* Model Card */}
      <div style={{
        border: '1px solid rgba(128, 128, 128, 0.3)',
        borderRadius: '8px',
        padding: '20px',
        marginBottom: '24px',
      }}>
        <h3 style={{ margin: 0, marginBottom: '4px' }}>Model</h3>
        <div style={{ fontSize: '14px', opacity: 0.7, fontFamily: 'monospace' }}>
          {deployment.modelId || '-'}
        </div>
      </div>

      {/* Access Model Card */}
      <div style={{
        border: '1px solid rgba(128, 128, 128, 0.3)',
        borderRadius: '8px',
        padding: '20px',
        marginBottom: '24px',
      }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginBottom: '8px' }}>
          <Icon icon="mdi:monitor" style={{ fontSize: '20px' }} />
          <h3 style={{ margin: 0 }}>Access Model</h3>
        </div>
        <div style={{ fontSize: '14px', opacity: 0.7, marginBottom: '16px' }}>
          Run this command to access the deployed model locally
        </div>

        <div style={{ display: 'flex', gap: '8px', alignItems: 'center' }}>
          <code style={{
            flex: 1,
            padding: '12px',
            backgroundColor: 'rgba(128, 128, 128, 0.1)',
            borderRadius: '4px',
            fontSize: '13px',
            fontFamily: 'monospace',
            overflowX: 'auto',
          }}>
            {portForwardCommand}
          </code>
          <Tooltip title={copied ? 'Copied!' : 'Copy to clipboard'}>
            <IconButton
              onClick={copyPortForwardCommand}
              color={copied ? 'success' : 'default'}
            >
              <Icon icon={copied ? 'mdi:check' : 'mdi:content-copy'} />
            </IconButton>
          </Tooltip>
        </div>

        <div style={{ fontSize: '12px', opacity: 0.6, marginTop: '8px' }}>
          After running the command, access the model at http://localhost:8000
        </div>

        {/* Ayna Integration */}
        <div style={{ marginTop: '16px', paddingTop: '16px', borderTop: '1px solid rgba(128, 128, 128, 0.2)' }}>
          <Button
            variant="contained"
            color="primary"
            startIcon={<Icon icon="mdi:chat" />}
            href={generateAynaUrl({
              model: deployment.modelId,
              provider: 'openai',
              endpoint: 'http://localhost:8000',
              type: 'chat',
            })}
          >
            Open in Ayna
          </Button>
        </div>
      </div>

      {/* Gateway Access Card */}
      {deployment.gateway?.endpoint && (
        <div style={{
          border: '1px solid rgba(128, 128, 128, 0.3)',
          borderRadius: '8px',
          padding: '20px',
          marginBottom: '24px',
        }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: '8px', marginBottom: '8px' }}>
            <Icon icon="mdi:router-network" style={{ fontSize: '20px' }} />
            <h3 style={{ margin: 0 }}>Gateway Access</h3>
          </div>
          <div style={{ fontSize: '14px', opacity: 0.7, marginBottom: '12px' }}>
            This model is accessible through the gateway at:
          </div>
          <code style={{
            display: 'block',
            padding: '12px',
            backgroundColor: 'rgba(128, 128, 128, 0.1)',
            borderRadius: '4px',
            fontSize: '13px',
          }}>
            {deployment.gateway.endpoint}
          </code>
          {deployment.gateway.modelName && (
            <div style={{ fontSize: '12px', opacity: 0.6, marginTop: '8px' }}>
              Use model name <code>{deployment.gateway.modelName}</code> in your inference requests
            </div>
          )}
        </div>
      )}

      {/* Tabbed Content */}
      <SectionBox title="">
        <Tabs
          tabs={tabs}
          ariaLabel="Deployment details tabs"
          onTabChanged={(index) => {
            setActiveTab(tabIds[index] || 'overview');
          }}
          sx={{ borderBottom: 1, borderColor: 'divider', marginBottom: 2 }}
        />
      </SectionBox>

      {/* Delete Confirmation Dialog */}
      <DeleteDialog
        open={showDeleteDialog}
        onCancel={() => setShowDeleteDialog(false)}
        onConfirm={handleDelete}
        resourceName={deployment.name}
        resourceType="deployment"
        loading={deleting}
      />
    </div>
  );
}
