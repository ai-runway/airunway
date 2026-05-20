/**
 * Create Deployment Page
 *
 * Deploys a selected model with runtime and configuration options.
 * Matches the native UI flow - model is pre-selected from catalog.
 */

import { useState, useEffect, useCallback, useMemo } from 'react';
import { useHistory, useLocation } from 'react-router-dom';
import {
  SectionBox,
  Loader,
} from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import { Router } from '@kinvolk/headlamp-plugin/lib';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import CircularProgress from '@mui/material/CircularProgress';
import { Icon } from '@iconify/react';
import { useApiClient } from '../lib/api-client';
import type { DeploymentConfig, Engine, Model, RuntimeStatus, ModelTask, StorageVolume } from '@airunway/shared';
import { toModelDeploymentManifest } from '@airunway/shared';
import { getBadgeColors } from '../lib/theme';
import { StorageVolumesEditor } from '../components/StorageVolumesEditor';
import { ManifestPreview } from '../components/ManifestPreview';

type RuntimeId = 'kaito' | 'kuberay' | 'dynamo' | 'llmd';

// Runtime metadata matching native UI
const RUNTIME_INFO: Record<RuntimeId, { name: string; description: string; defaultNamespace: string }> = {
  dynamo: {
    name: 'NVIDIA Dynamo',
    description: 'High-performance inference with KV-cache routing',
    defaultNamespace: 'dynamo-system',
  },
  kuberay: {
    name: 'KubeRay',
    description: 'Ray-based serving with autoscaling',
    defaultNamespace: 'kuberay-system',
  },
  kaito: {
    name: 'KAITO',
    description: 'Flexible inference with GGUF and vLLM support',
    defaultNamespace: 'kaito-workspace',
  },
  llmd: {
    name: 'llm-d',
    description: 'Lightweight disaggregated inference with vLLM',
    defaultNamespace: 'default',
  },
};

// Engines supported by each runtime
const RUNTIME_ENGINES: Record<RuntimeId, Engine[]> = {
  dynamo: ['vllm', 'sglang', 'trtllm'],
  kuberay: ['vllm'],
  kaito: ['vllm', 'llamacpp'],
  llmd: ['vllm'],
};

// Check runtime compatibility with model
function isRuntimeCompatible(runtimeId: RuntimeId, modelEngines: Engine[]): boolean {
  const runtimeEngines = RUNTIME_ENGINES[runtimeId];
  return modelEngines.some((e) => runtimeEngines.includes(e));
}

// Generate deployment name from model ID
function generateDeploymentName(modelId: string): string {
  return modelId
    .replace(/[/:.]/g, '-')
    .toLowerCase()
    .replace(/--+/g, '-')
    .replace(/^-|-$/g, '')
    .slice(0, 53);
}

export function CreateDeployment() {
  const api = useApiClient();
  const history = useHistory();
  const location = useLocation();

  // Parse URL params
  const searchParams = useMemo(() => new URLSearchParams(location.search), [location.search]);
  const modelIdFromUrl = searchParams.get('modelId');
  const sourceFromUrl = searchParams.get('source');
  const isHfSource = sourceFromUrl === 'huggingface' || sourceFromUrl === 'hf';

  // State
  const [model, setModel] = useState<Model | null>(null);
  const [runtimes, setRuntimes] = useState<RuntimeStatus[]>([]);
  const [loading, setLoading] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Form state
  const [selectedRuntime, setSelectedRuntime] = useState<RuntimeId>('kaito');
  const [engine, setEngine] = useState<Engine>('vllm');
  const [name, setName] = useState('');
  const [namespace, setNamespace] = useState('kaito-workspace');
  const [replicas, setReplicas] = useState(1);
  const [gpuCount, setGpuCount] = useState(1);
  const [mode, setMode] = useState<'aggregated' | 'disaggregated'>('aggregated');
  const [prefillReplicas, setPrefillReplicas] = useState(1);
  const [prefillGpus, setPrefillGpus] = useState(1);
  const [decodeReplicas, setDecodeReplicas] = useState(1);
  const [decodeGpus, setDecodeGpus] = useState(1);
  const [showAdvanced, setShowAdvanced] = useState(false);
  const [servedModelName, setServedModelName] = useState('');
  const [engineArgs, setEngineArgs] = useState<{ key: string; value: string }[]>([]);
  const [contextLength, setContextLength] = useState<number | undefined>(undefined);
  const [enforceEager, setEnforceEager] = useState(true);
  const [trustRemoteCode, setTrustRemoteCode] = useState(false);
  const [hasSetInitialRuntime, setHasSetInitialRuntime] = useState(false);
  const [storageVolumes, setStorageVolumes] = useState<StorageVolume[]>([]);
  const [showManifest, setShowManifest] = useState(false);
  const [manifestYaml, setManifestYaml] = useState<string | null>(null);
  const [manifestError, setManifestError] = useState<string | null>(null);
  const [hfSecretConfigured, setHfSecretConfigured] = useState(false);

  // Check if HF token secret is configured
  useEffect(() => {
    api.huggingFace.getSecretStatus()
      .then((status) => setHfSecretConfigured(status.configured))
      .catch(() => setHfSecretConfigured(false));
  }, [api]);

  // Load model and runtime data
  useEffect(() => {
    async function loadData() {
      if (!modelIdFromUrl) {
        setLoading(false);
        return;
      }

      try {
        const [runtimesResult] = await Promise.all([
          api.runtimes.getStatus(),
        ]);
        setRuntimes(runtimesResult.runtimes);

        // Fetch model details
        if (isHfSource) {
          // For HuggingFace models, search to get full details
          const searchResult = await api.huggingFace.searchModels(modelIdFromUrl, { limit: 1 });
          if (searchResult.models.length > 0) {
            const hfModel = searchResult.models[0];
            // Convert HfModelSearchResult to Model format
            setModel({
              id: hfModel.id,
              name: hfModel.name || hfModel.id.split('/').pop() || hfModel.id,
              description: '',
              size: hfModel.estimatedGpuMemory || 'Unknown',
              minGpuMemory: hfModel.estimatedGpuMemory || 'N/A',
              estimatedGpuMemory: hfModel.estimatedGpuMemory,
              estimatedGpuMemoryGb: hfModel.estimatedGpuMemoryGb,
              task: (hfModel.pipelineTag || 'text-generation') as ModelTask,
              supportedEngines: hfModel.supportedEngines,
              gated: hfModel.gated,
              fromHfSearch: true,
            });
          } else {
            setError(`Model "${modelIdFromUrl}" was not found on HuggingFace`);
          }
        } else {
          // For curated models
          const modelsResult = await api.models.list();
          const foundModel = modelsResult.models.find((m) => m.id === modelIdFromUrl);
          if (foundModel) {
            setModel(foundModel);
          } else {
            setError(`Model "${modelIdFromUrl}" was not found in the catalog`);
          }
        }
      } catch (err) {
        setError(err instanceof Error ? err.message : 'Failed to load data');
      } finally {
        setLoading(false);
      }
    }
    loadData();
  }, [api, modelIdFromUrl, isHfSource]);

  // Set defaults when model loads (runs only once when model data arrives)
  useEffect(() => {
    if (model && runtimes.length > 0) {
      // Generate deployment name (only if not already set)
      if (!name) {
        setName(generateDeploymentName(model.id));
      }

      // Set GPU recommendation based on model
      if (model.estimatedGpuMemoryGb) {
        const recommendedGpus = Math.ceil(model.estimatedGpuMemoryGb / 80); // A100 80GB
        setGpuCount(Math.max(1, recommendedGpus));
      }
    }
  }, [model, runtimes.length, name]);

  // Set initial runtime selection only once when data first loads
  useEffect(() => {
    if (model && runtimes.length > 0 && !hasSetInitialRuntime) {
      // Select best runtime (prefer healthy/running ones)
      const compatibleRuntimes: RuntimeId[] = ['dynamo', 'kuberay', 'kaito', 'llmd'];
      for (const rtId of compatibleRuntimes) {
        const rt = runtimes.find((r) => r.id === rtId);
        if (rt?.healthy && isRuntimeCompatible(rtId, model.supportedEngines)) {
          setSelectedRuntime(rtId);
          setNamespace(RUNTIME_INFO[rtId].defaultNamespace);
          // Select best engine for this runtime
          const availableEngines = model.supportedEngines.filter(
            (e) => RUNTIME_ENGINES[rtId]?.includes(e)
          );
          if (availableEngines.length > 0) {
            setEngine(availableEngines[0]);
          }
          break;
        }
      }
      setHasSetInitialRuntime(true);
    }
  }, [model, runtimes, hasSetInitialRuntime]);

  // Handle runtime change
  const handleRuntimeChange = useCallback((runtime: RuntimeId) => {
    setSelectedRuntime(runtime);
    setNamespace(RUNTIME_INFO[runtime].defaultNamespace);
    if (runtime !== 'dynamo' && runtime !== 'llmd') {
      setMode('aggregated');
    }
    // Reset engine if not compatible
    if (model) {
      const availableEngines = model.supportedEngines.filter(
        (e) => RUNTIME_ENGINES[runtime]?.includes(e)
      );
      if (availableEngines.length > 0 && !availableEngines.includes(engine)) {
        setEngine(availableEngines[0]);
      }
    }
  }, [model, engine]);

  // Check if selected runtime supports disaggregated serving
  const supportsDisaggregated = selectedRuntime === 'dynamo' || selectedRuntime === 'llmd';



  // Build the current config object (shared between submit and preview)
  const buildCurrentConfig = useCallback((): DeploymentConfig | null => {
    if (!model) return null;

    const effectiveMode = supportsDisaggregated ? mode : 'aggregated';
    const config: DeploymentConfig = {
      name,
      namespace,
      modelId: model.id,
      engine,
      mode: effectiveMode,
      provider: selectedRuntime,
      servedModelName: servedModelName || undefined,
      engineArgs: engineArgs.length > 0
        ? Object.fromEntries(engineArgs.filter(a => a.key).map(a => [a.key, a.value]))
        : undefined,
      routerMode: 'default',
      replicas,
      hfTokenSecret: hfSecretConfigured ? 'hf-token-secret' : undefined,
      enforceEager,
      enablePrefixCaching: false,
      trustRemoteCode,
      contextLength,
      resources: {
        gpu: gpuCount,
      },
      ...(effectiveMode === 'disaggregated' && {
        prefillReplicas,
        prefillGpus,
        decodeReplicas,
        decodeGpus,
      }),
      storage: storageVolumes.length > 0 ? { volumes: storageVolumes } : undefined,
    };

    if (selectedRuntime === 'kaito') {
      if (engine === 'vllm') {
        config.modelSource = 'vllm';
        config.computeType = 'gpu';
      } else if (engine === 'llamacpp') {
        config.modelSource = 'huggingface';
        config.computeType = gpuCount > 0 ? 'gpu' : 'cpu';
        config.ggufRunMode = 'direct';
      }
    }

    return config;
  }, [model, name, namespace, engine, selectedRuntime, servedModelName, engineArgs, replicas, gpuCount, contextLength, enforceEager, trustRemoteCode, supportsDisaggregated, mode, prefillReplicas, prefillGpus, decodeReplicas, decodeGpus, storageVolumes, hfSecretConfigured]);

  // Submit deployment
  const handleSubmit = useCallback(async () => {
    const config = buildCurrentConfig();
    if (!config) return;

    setSubmitting(true);
    setError(null);

    try {
      await api.deployments.create(config);
      history.push(Router.createRouteURL('AI Runway Deployments'));
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to create deployment');
    } finally {
      setSubmitting(false);
    }
  }, [api, buildCurrentConfig, history]);

  // Generate manifest preview as YAML-like formatted JSON
  const generateManifestPreview = useCallback(() => {
    const config = buildCurrentConfig();
    if (!config) {
      setManifestYaml(null);
      setManifestError(null);
      return;
    }

    try {
      const manifest = toModelDeploymentManifest(config);
      setManifestYaml(JSON.stringify(manifest, null, 2));
      setManifestError(null);
    } catch (err) {
      setManifestYaml(null);
      setManifestError(err instanceof Error ? err.message : 'Failed to generate manifest');
    }
  }, [buildCurrentConfig]);

  useEffect(() => {
    if (showManifest) {
      generateManifestPreview();
    }
  }, [showManifest, generateManifestPreview]);

  // Get available engines for selected runtime
  const availableEngines = useMemo(() => {
    if (!model) return [];
    return model.supportedEngines.filter((e) => RUNTIME_ENGINES[selectedRuntime]?.includes(e));
  }, [model, selectedRuntime]);

  if (loading) {
    return <Loader title="Loading model..." />;
  }

  // No model selected - show message to go to catalog
  if (!model) {
    return (
      <SectionBox title="Create Deployment">
        <div style={{ textAlign: 'center', padding: '48px 24px' }}>
          <div style={{ fontSize: '48px', marginBottom: '16px' }}>🤖</div>
          <h2 style={{ marginBottom: '8px' }}>No Model Selected</h2>
          <p style={{ opacity: 0.7, marginBottom: '24px' }}>
            Please select a model from the catalog to deploy.
          </p>
          <button
            onClick={() => history.push(Router.createRouteURL('AI Runway Models'))}
            style={{
              padding: '12px 24px',
              backgroundColor: '#1976d2',
              color: 'white',
              border: 'none',
              borderRadius: '4px',
              cursor: 'pointer',
              fontSize: '14px',
            }}
          >
            Go to Model Catalog
          </button>
        </div>
      </SectionBox>
    );
  }

  const selectedRuntimeInfo = runtimes.find((r) => r.id === selectedRuntime);
  // Use 'healthy' to check if operator is running, not just CRDs installed
  const isRuntimeInstalled = selectedRuntimeInfo?.healthy ?? false;

  return (
    <SectionBox title="Deploy Model">
      {/* Back button */}
      <div style={{ marginBottom: '24px' }}>
        <button
          onClick={() => history.push(Router.createRouteURL('AI Runway Models'))}
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: '8px',
            padding: '8px 0',
            background: 'none',
            border: 'none',
            color: 'inherit',
            cursor: 'pointer',
            opacity: 0.7,
          }}
        >
          ← Back to Catalog
        </button>
      </div>

      {error && (
        <div style={{ padding: '12px', backgroundColor: 'rgba(198, 40, 40, 0.15)', color: '#f44336', borderRadius: '4px', marginBottom: '16px' }}>
          {error}
        </div>
      )}

      {/* Model Summary Card */}
      <div style={{
        border: '1px solid rgba(128, 128, 128, 0.3)',
        borderRadius: '8px',
        padding: '20px',
        marginBottom: '24px',
        backgroundColor: 'rgba(128, 128, 128, 0.05)',
      }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: '12px' }}>
          <div>
            <h2 style={{ margin: 0, marginBottom: '4px' }}>{model.name}</h2>
            <div style={{ fontSize: '13px', opacity: 0.7, fontFamily: 'monospace' }}>{model.id}</div>
            {model.gated && (
              <span style={{
                display: 'inline-block',
                marginTop: '8px',
                padding: '2px 8px',
                backgroundColor: getBadgeColors('warning').bg,
                color: getBadgeColors('warning').color,
                borderRadius: '4px',
                fontSize: '12px',
              }}>
                🔒 Gated Model
              </span>
            )}
          </div>
          <span style={{
            padding: '4px 12px',
            border: '1px solid rgba(128, 128, 128, 0.3)',
            borderRadius: '4px',
            fontSize: '14px',
            fontWeight: 500,
          }}>
            {model.size}
          </span>
        </div>

        {model.description && (
          <p style={{ margin: '12px 0', opacity: 0.8, fontSize: '14px' }}>{model.description}</p>
        )}

        <div style={{ display: 'flex', flexWrap: 'wrap', gap: '16px', fontSize: '14px', opacity: 0.7 }}>
          {model.estimatedGpuMemory && (
            <div>GPU: ~{model.estimatedGpuMemory}</div>
          )}
          {model.contextLength && (
            <div>Context: {model.contextLength.toLocaleString()}</div>
          )}
          <div style={{ textTransform: 'capitalize' }}>{model.task?.replace('-', ' ') || 'Text Generation'}</div>
        </div>

        <div style={{ display: 'flex', flexWrap: 'wrap', gap: '6px', marginTop: '12px' }}>
          {model.supportedEngines.map((eng) => (
            <span
              key={eng}
              style={{
                padding: '2px 8px',
                backgroundColor: getBadgeColors('info').bg,
                color: getBadgeColors('info').color,
                borderRadius: '4px',
                fontSize: '12px',
              }}
            >
              {eng.toUpperCase()}
            </span>
          ))}
        </div>
      </div>

      {/* Runtime Selection */}
      <div style={{ marginBottom: '24px' }}>
        <h3 style={{ marginBottom: '12px', display: 'flex', alignItems: 'center', gap: '8px' }}>
          🖥️ Runtime
        </h3>
        <div style={{ display: 'grid', gap: '12px' }}>
          {(['dynamo', 'kuberay', 'kaito', 'llmd'] as const).map((rtId) => {
            const info = RUNTIME_INFO[rtId];
            const rtStatus = runtimes.find((r) => r.id === rtId);
            // Use 'healthy' to check if operator is running, not just CRDs installed
            const isInstalled = rtStatus?.healthy ?? false;
            const isCompatible = isRuntimeCompatible(rtId, model.supportedEngines);
            const isSelected = selectedRuntime === rtId;
            // Only disable if not compatible - allow clicking uninstalled runtimes
            const isDisabled = !isCompatible;

            return (
              <div
                key={rtId}
                onClick={() => !isDisabled && handleRuntimeChange(rtId)}
                style={{
                  padding: '16px',
                  border: isSelected ? '2px solid #1976d2' : '1px solid rgba(128, 128, 128, 0.3)',
                  borderRadius: '8px',
                  backgroundColor: isSelected ? 'rgba(25, 118, 210, 0.1)' : 'transparent',
                  cursor: isDisabled ? 'not-allowed' : 'pointer',
                  opacity: isDisabled ? 0.5 : 1,
                }}
              >
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: '4px' }}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
                    <div style={{
                      width: '20px',
                      height: '20px',
                      borderRadius: '50%',
                      border: isSelected ? '2px solid #1976d2' : '2px solid rgba(128, 128, 128, 0.5)',
                      display: 'flex',
                      alignItems: 'center',
                      justifyContent: 'center',
                    }}>
                      {isSelected && <div style={{ width: '10px', height: '10px', borderRadius: '50%', backgroundColor: '#1976d2' }} />}
                    </div>
                    <span style={{ fontWeight: 500 }}>{info.name}</span>
                  </div>
                  <div style={{ display: 'flex', gap: '8px' }}>
                    {!isCompatible && (
                      <span style={{ padding: '2px 8px', backgroundColor: getBadgeColors('neutral').bg, borderRadius: '4px', fontSize: '12px' }}>
                        Not Compatible
                      </span>
                    )}
                    <span style={{
                      padding: '2px 8px',
                      backgroundColor: isInstalled ? getBadgeColors('success').bg : getBadgeColors('error').bg,
                      color: isInstalled ? getBadgeColors('success').color : getBadgeColors('error').color,
                      borderRadius: '4px',
                      fontSize: '12px',
                    }}>
                      {isInstalled ? '✓ Installed' : '⊘ Not Installed'}
                    </span>
                  </div>
                </div>
                <p style={{ margin: 0, marginLeft: '32px', fontSize: '13px', opacity: 0.7 }}>{info.description}</p>
                {/* Show install message with link when selected but not installed */}
                {isSelected && !isInstalled && (
                  <p style={{
                    margin: '8px 0 0 32px',
                    fontSize: '13px',
                    color: '#f57c00',
                  }}>
                    <a
                      href={Router.createRouteURL('AI Runway Runtimes')}
                      onClick={(e) => {
                        e.preventDefault();
                        e.stopPropagation();
                        history.push(Router.createRouteURL('AI Runway Runtimes'));
                      }}
                      style={{
                        color: '#f57c00',
                        textDecoration: 'underline',
                        cursor: 'pointer',
                      }}
                    >
                      Install {info.name}
                    </a>
                    {' '}before deploying.
                  </p>
                )}
              </div>
            );
          })}
        </div>
      </div>

      {/* Engine Selection */}
      {availableEngines.length > 0 && selectedRuntime !== 'kaito' && (
        <div style={{ marginBottom: '24px' }}>
          <h3 style={{ marginBottom: '12px' }}>⚙️ Inference Engine</h3>
          <div style={{ display: 'flex', gap: '12px' }}>
            {availableEngines.map((eng) => (
              <button
                key={eng}
                onClick={() => setEngine(eng)}
                style={{
                  padding: '10px 20px',
                  border: engine === eng ? '2px solid #1976d2' : '1px solid rgba(128, 128, 128, 0.3)',
                  borderRadius: '8px',
                  backgroundColor: engine === eng ? 'rgba(25, 118, 210, 0.1)' : 'transparent',
                  color: 'inherit',
                  cursor: 'pointer',
                  fontWeight: engine === eng ? 500 : 400,
                }}
              >
                {eng === 'vllm' && 'vLLM'}
                {eng === 'sglang' && 'SGLang'}
                {eng === 'trtllm' && 'TensorRT-LLM'}
                {eng === 'llamacpp' && 'llama.cpp'}
              </button>
            ))}
          </div>
        </div>
      )}

      {/* Serving Mode Selection - only for runtimes that support disaggregated */}
      {supportsDisaggregated && (
        <div style={{ marginBottom: '24px' }}>
          <h3 style={{ marginBottom: '12px' }}>🔀 Serving Mode</h3>
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '12px', maxWidth: '500px' }}>
            {([
              { id: 'aggregated' as const, label: 'Aggregated', description: 'All inference in a single deployment' },
              { id: 'disaggregated' as const, label: 'Disaggregated', description: 'Separate prefill and decode pipelines for lower latency' },
            ]).map((option) => (
              <div
                key={option.id}
                onClick={() => setMode(option.id)}
                style={{
                  padding: '16px',
                  border: mode === option.id ? '2px solid #1976d2' : '1px solid rgba(128, 128, 128, 0.3)',
                  borderRadius: '8px',
                  backgroundColor: mode === option.id ? 'rgba(25, 118, 210, 0.1)' : 'transparent',
                  cursor: 'pointer',
                }}
              >
                <div style={{ display: 'flex', alignItems: 'center', gap: '12px', marginBottom: '4px' }}>
                  <div style={{
                    width: '20px',
                    height: '20px',
                    borderRadius: '50%',
                    border: mode === option.id ? '2px solid #1976d2' : '2px solid rgba(128, 128, 128, 0.5)',
                    display: 'flex',
                    alignItems: 'center',
                    justifyContent: 'center',
                  }}>
                    {mode === option.id && <div style={{ width: '10px', height: '10px', borderRadius: '50%', backgroundColor: '#1976d2' }} />}
                  </div>
                  <span style={{ fontWeight: 500 }}>{option.label}</span>
                </div>
                <p style={{ margin: 0, marginLeft: '32px', fontSize: '13px', opacity: 0.7 }}>{option.description}</p>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* Basic Configuration */}
      <div style={{ marginBottom: '24px' }}>
        <h3 style={{ marginBottom: '12px' }}>📝 Basic Configuration</h3>
        <div style={{ display: 'grid', gap: '16px', maxWidth: '500px' }}>
          <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '16px' }}>
            <div>
              <label style={{ display: 'block', marginBottom: '6px', fontWeight: 500 }}>Deployment Name</label>
              <input
                type="text"
                value={name}
                onChange={(e) => setName(e.target.value)}
                style={{
                  width: '100%',
                  padding: '10px 12px',
                  border: '1px solid rgba(128, 128, 128, 0.3)',
                  borderRadius: '4px',
                  backgroundColor: 'transparent',
                  color: 'inherit',
                }}
              />
              <div style={{ fontSize: '12px', opacity: 0.6, marginTop: '4px' }}>
                Lowercase letters, numbers, and hyphens only
              </div>
            </div>
            <div>
              <label style={{ display: 'block', marginBottom: '6px', fontWeight: 500 }}>Namespace</label>
              <input
                type="text"
                value={namespace}
                onChange={(e) => setNamespace(e.target.value)}
                style={{
                  width: '100%',
                  padding: '10px 12px',
                  border: '1px solid rgba(128, 128, 128, 0.3)',
                  borderRadius: '4px',
                  backgroundColor: 'transparent',
                  color: 'inherit',
                }}
              />
            </div>
          </div>
        </div>
      </div>

      {/* Deployment Options */}
      <div style={{ marginBottom: '24px' }}>
        <h3 style={{ marginBottom: '12px' }}>🚀 Deployment Options</h3>
        {mode === 'disaggregated' ? (
          /* Disaggregated mode: separate prefill and decode scaling */
          <div style={{ display: 'grid', gap: '20px', maxWidth: '500px' }}>
            {/* Prefill Pipeline */}
            <div style={{
              border: '1px solid rgba(128, 128, 128, 0.3)',
              borderRadius: '8px',
              padding: '16px',
              backgroundColor: 'rgba(128, 128, 128, 0.05)',
            }}>
              <h4 style={{ margin: '0 0 12px 0', fontSize: '14px', fontWeight: 500 }}>
                Prefill Pipeline
              </h4>
              <div style={{ fontSize: '13px', opacity: 0.7, marginBottom: '12px' }}>
                Handles the initial prompt processing
              </div>
              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '16px' }}>
                <div>
                  <label style={{ display: 'block', marginBottom: '6px', fontWeight: 500 }}>
                    Replicas
                  </label>
                  <input
                    type="number"
                    min={1}
                    max={10}
                    value={prefillReplicas}
                    onChange={(e) => setPrefillReplicas(parseInt(e.target.value) || 1)}
                    style={{
                      width: '100%',
                      padding: '10px 12px',
                      border: '1px solid rgba(128, 128, 128, 0.3)',
                      borderRadius: '4px',
                      backgroundColor: 'transparent',
                      color: 'inherit',
                    }}
                  />
                </div>
                <div>
                  <label style={{ display: 'block', marginBottom: '6px', fontWeight: 500 }}>
                    GPUs per Replica
                  </label>
                  <input
                    type="number"
                    min={0}
                    max={8}
                    value={prefillGpus}
                    onChange={(e) => setPrefillGpus(parseInt(e.target.value) || 0)}
                    style={{
                      width: '100%',
                      padding: '10px 12px',
                      border: '1px solid rgba(128, 128, 128, 0.3)',
                      borderRadius: '4px',
                      backgroundColor: 'transparent',
                      color: 'inherit',
                    }}
                  />
                </div>
              </div>
            </div>

            {/* Decode Pipeline */}
            <div style={{
              border: '1px solid rgba(128, 128, 128, 0.3)',
              borderRadius: '8px',
              padding: '16px',
              backgroundColor: 'rgba(128, 128, 128, 0.05)',
            }}>
              <h4 style={{ margin: '0 0 12px 0', fontSize: '14px', fontWeight: 500 }}>
                Decode Pipeline
              </h4>
              <div style={{ fontSize: '13px', opacity: 0.7, marginBottom: '12px' }}>
                Generates output tokens
              </div>
              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '16px' }}>
                <div>
                  <label style={{ display: 'block', marginBottom: '6px', fontWeight: 500 }}>
                    Replicas
                  </label>
                  <input
                    type="number"
                    min={1}
                    max={10}
                    value={decodeReplicas}
                    onChange={(e) => setDecodeReplicas(parseInt(e.target.value) || 1)}
                    style={{
                      width: '100%',
                      padding: '10px 12px',
                      border: '1px solid rgba(128, 128, 128, 0.3)',
                      borderRadius: '4px',
                      backgroundColor: 'transparent',
                      color: 'inherit',
                    }}
                  />
                </div>
                <div>
                  <label style={{ display: 'block', marginBottom: '6px', fontWeight: 500 }}>
                    GPUs per Replica
                  </label>
                  <input
                    type="number"
                    min={0}
                    max={8}
                    value={decodeGpus}
                    onChange={(e) => setDecodeGpus(parseInt(e.target.value) || 0)}
                    style={{
                      width: '100%',
                      padding: '10px 12px',
                      border: '1px solid rgba(128, 128, 128, 0.3)',
                      borderRadius: '4px',
                      backgroundColor: 'transparent',
                      color: 'inherit',
                    }}
                  />
                </div>
              </div>
            </div>

            {model.estimatedGpuMemory && (
              <div style={{ fontSize: '12px', opacity: 0.6 }}>
                Model needs ~{model.estimatedGpuMemory}
              </div>
            )}
          </div>
        ) : (
          /* Aggregated mode: single replicas + GPU section */
          <div style={{ display: 'grid', gap: '16px', maxWidth: '500px' }}>
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '16px' }}>
              <div>
                <label style={{ display: 'block', marginBottom: '6px', fontWeight: 500 }}>
                  Replicas
                </label>
                <input
                  type="number"
                  min={1}
                  max={10}
                  value={replicas}
                  onChange={(e) => setReplicas(parseInt(e.target.value) || 1)}
                  style={{
                    width: '100%',
                    padding: '10px 12px',
                    border: '1px solid rgba(128, 128, 128, 0.3)',
                    borderRadius: '4px',
                    backgroundColor: 'transparent',
                    color: 'inherit',
                  }}
                />
              </div>
              <div>
                <label style={{ display: 'block', marginBottom: '6px', fontWeight: 500 }}>
                  GPUs per Replica
                  {model.estimatedGpuMemoryGb && gpuCount === Math.ceil(model.estimatedGpuMemoryGb / 80) && (
                    <span style={{
                      marginLeft: '8px',
                      padding: '2px 6px',
                      backgroundColor: getBadgeColors('info').bg,
                      color: getBadgeColors('info').color,
                      borderRadius: '4px',
                      fontSize: '11px',
                    }}>
                      Recommended
                    </span>
                  )}
                </label>
                <input
                  type="number"
                  min={0}
                  max={8}
                  value={gpuCount}
                  onChange={(e) => setGpuCount(parseInt(e.target.value) || 0)}
                  style={{
                    width: '100%',
                    padding: '10px 12px',
                    border: '1px solid rgba(128, 128, 128, 0.3)',
                    borderRadius: '4px',
                    backgroundColor: 'transparent',
                    color: 'inherit',
                  }}
                />
                {model.estimatedGpuMemory && (
                  <div style={{ fontSize: '12px', opacity: 0.6, marginTop: '4px' }}>
                    Model needs ~{model.estimatedGpuMemory}
                  </div>
                )}
              </div>
            </div>
          </div>
        )}
      </div>

      {/* Storage Volumes */}
      <div style={{ marginBottom: '24px' }}>
        <h3 style={{ marginBottom: '12px' }}>💾 Storage Volumes</h3>
        <div style={{ fontSize: '13px', opacity: 0.7, marginBottom: '12px' }}>
          Attach persistent storage for model caching or custom data
        </div>
        <StorageVolumesEditor
          volumes={storageVolumes}
          onChange={setStorageVolumes}
        />
      </div>

      {/* Advanced Options */}
      <div style={{ marginBottom: '24px' }}>
        <button
          onClick={() => setShowAdvanced(!showAdvanced)}
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: '8px',
            padding: '8px 0',
            background: 'none',
            border: 'none',
            color: 'inherit',
            cursor: 'pointer',
            fontWeight: 500,
          }}
        >
          <span style={{ transform: showAdvanced ? 'rotate(90deg)' : 'rotate(0deg)', transition: 'transform 0.2s' }}>▶</span>
          Advanced Options
        </button>

        {showAdvanced && (
          <div style={{ marginTop: '16px', paddingLeft: '16px', borderLeft: '2px solid rgba(128, 128, 128, 0.3)' }}>
            <div style={{ display: 'grid', gap: '16px', maxWidth: '500px' }}>
              <div>
                <label style={{ display: 'block', marginBottom: '6px', fontWeight: 500 }}>Context Length (optional)</label>
                <input
                  type="number"
                  placeholder={model.contextLength?.toString() || 'Default'}
                  value={contextLength || ''}
                  onChange={(e) => setContextLength(e.target.value ? parseInt(e.target.value) : undefined)}
                  style={{
                    width: '100%',
                    padding: '10px 12px',
                    border: '1px solid rgba(128, 128, 128, 0.3)',
                    borderRadius: '4px',
                    backgroundColor: 'transparent',
                    color: 'inherit',
                  }}
                />
              </div>

              <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
                <input
                  type="checkbox"
                  id="enforceEager"
                  checked={enforceEager}
                  onChange={(e) => setEnforceEager(e.target.checked)}
                  style={{ width: '18px', height: '18px' }}
                />
                <label htmlFor="enforceEager">
                  <div style={{ fontWeight: 500 }}>Enforce Eager Mode</div>
                  <div style={{ fontSize: '13px', opacity: 0.7 }}>Use eager mode for faster startup</div>
                </label>
              </div>

              <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
                <input
                  type="checkbox"
                  id="trustRemoteCode"
                  checked={trustRemoteCode}
                  onChange={(e) => setTrustRemoteCode(e.target.checked)}
                  style={{ width: '18px', height: '18px' }}
                />
                <label htmlFor="trustRemoteCode">
                  <div style={{ fontWeight: 500 }}>Trust Remote Code</div>
                  <div style={{ fontSize: '13px', opacity: 0.7 }}>Required for some models with custom code</div>
                </label>
              </div>

              {/* Served Model Name */}
              <div>
                <label style={{ display: 'block', marginBottom: '6px', fontWeight: 500 }}>Served Model Name (optional)</label>
                <input
                  type="text"
                  placeholder={model.id}
                  value={servedModelName}
                  onChange={(e) => setServedModelName(e.target.value)}
                  style={{
                    width: '100%',
                    padding: '10px 12px',
                    border: '1px solid rgba(128, 128, 128, 0.3)',
                    borderRadius: '4px',
                    backgroundColor: 'transparent',
                    color: 'inherit',
                  }}
                />
                <div style={{ fontSize: '12px', opacity: 0.6, marginTop: '4px' }}>
                  The name clients use when sending inference requests. Defaults to the model ID.
                </div>
              </div>

              {/* Engine Arguments */}
              <div>
                <label style={{ display: 'block', marginBottom: '6px', fontWeight: 500 }}>Engine Arguments</label>
                <div style={{ fontSize: '12px', opacity: 0.6, marginBottom: '8px' }}>
                  Key-value pairs passed to the inference engine.
                </div>
                {engineArgs.map((arg, index) => (
                  <div key={index} style={{ display: 'flex', gap: '8px', marginBottom: '8px', alignItems: 'center' }}>
                    <input
                      type="text"
                      placeholder="Key"
                      value={arg.key}
                      onChange={(e) => {
                        const newKey = e.target.value;
                        setEngineArgs((prev) =>
                          prev.map((a, i) => (i === index ? { ...a, key: newKey } : a))
                        );
                      }}
                      style={{
                        flex: 1,
                        padding: '10px 12px',
                        border: '1px solid rgba(128, 128, 128, 0.3)',
                        borderRadius: '4px',
                        backgroundColor: 'transparent',
                        color: 'inherit',
                      }}
                    />
                    <input
                      type="text"
                      placeholder="Value"
                      value={arg.value}
                      onChange={(e) => {
                        const newValue = e.target.value;
                        setEngineArgs((prev) =>
                          prev.map((a, i) => (i === index ? { ...a, value: newValue } : a))
                        );
                      }}
                      style={{
                        flex: 1,
                        padding: '10px 12px',
                        border: '1px solid rgba(128, 128, 128, 0.3)',
                        borderRadius: '4px',
                        backgroundColor: 'transparent',
                        color: 'inherit',
                      }}
                    />
                    <IconButton
                      size="small"
                      onClick={() => {
                        setEngineArgs((prev) => prev.filter((_, i) => i !== index));
                      }}
                      sx={{ color: 'inherit', opacity: 0.7 }}
                    >
                      <Icon icon="mdi:close" width={18} />
                    </IconButton>
                  </div>
                ))}
                <Button
                  size="small"
                  onClick={() => setEngineArgs((prev) => [...prev, { key: '', value: '' }])}
                  startIcon={<Icon icon="mdi:plus" />}
                  sx={{ textTransform: 'none', mt: 0.5 }}
                >
                  Add argument
                </Button>
              </div>
            </div>
          </div>
        )}
      </div>

      {/* Manifest Preview */}
      <div style={{ marginBottom: '24px' }}>
        <button
          onClick={() => setShowManifest(!showManifest)}
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: '8px',
            padding: '8px 0',
            background: 'none',
            border: 'none',
            color: 'inherit',
            cursor: 'pointer',
            fontWeight: 500,
          }}
        >
          <span style={{ transform: showManifest ? 'rotate(90deg)' : 'rotate(0deg)', transition: 'transform 0.2s' }}>▶</span>
          Preview Manifest
        </button>

        {showManifest && (
          <div style={{ marginTop: '16px' }}>
            <ManifestPreview
              manifest={manifestYaml}
              loading={false}
              error={manifestError}
              onRefresh={generateManifestPreview}
            />
          </div>
        )}
      </div>

      {/* Submit Section */}
      <div style={{
        display: 'flex',
        gap: '12px',
        paddingTop: '24px',
        paddingBottom: '32px',
        borderTop: '1px solid rgba(128, 128, 128, 0.3)',
      }}>
        <Button
          variant="contained"
          color="error"
          onClick={() => history.push(Router.createRouteURL('AI Runway Models'))}
          sx={{ fontWeight: 600, px: 3, py: 1.5 }}
        >
          Cancel
        </Button>
        <Button
          variant="contained"
          onClick={handleSubmit}
          color="primary"
          disabled={submitting || !isRuntimeInstalled || !name}
          startIcon={
            submitting ? (
              <CircularProgress size={18} color="inherit" />
            ) : !isRuntimeInstalled ? (
              <Icon icon="mdi:alert" />
            ) : (
              <Icon icon="mdi:rocket-launch" />
            )
          }
        >
          {submitting ? 'Creating...' : !isRuntimeInstalled ? 'Runtime Not Installed' : 'Deploy Model'}
        </Button>
      </div>
    </SectionBox>
  );
}
