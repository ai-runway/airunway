/**
 * Models Catalog Page
 *
 * Browse curated models and search HuggingFace models.
 */

import type { HfModelSearchResult, Model } from '@airunway/shared';
import { Icon } from '@iconify/react';
import { Router } from '@kinvolk/headlamp-plugin/lib';
import {
  Loader,
  SectionBox,
  Tabs,
} from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import Button from '@mui/material/Button';
import { useCallback, useEffect, useState } from 'react';
import { useHistory } from 'react-router-dom';
import { ConnectionError } from '../components/ConnectionBanner';
import { useApiClient } from '../lib/api-client';
import { getBadgeColors } from '../lib/theme';

type TabType = 'curated' | 'huggingface';

/**
 * Helper to determine compute type from model data
 * CPU models have minGpus === 0 or undefined with GGUF-style IDs
 */
function getComputeType(model: Model): 'cpu' | 'gpu' {
  if (model.minGpus !== undefined && model.minGpus > 0) {
    return 'gpu';
  }
  // KAITO GGUF models are CPU-only (they use llama.cpp)
  if (model.id.startsWith('kaito/') && model.id.includes('-gguf')) {
    return 'cpu';
  }
  // Default to GPU for models that require hardware acceleration
  return model.minGpus === 0 ? 'cpu' : 'gpu';
}

export function ModelsCatalog() {
  const api = useApiClient();
  const history = useHistory();

  const [activeTab, setActiveTab] = useState<TabType>('curated');
  const [curatedModels, setCuratedModels] = useState<Model[]>([]);
  const [searchResults, setSearchResults] = useState<HfModelSearchResult[]>([]);
  const [searchQuery, setSearchQuery] = useState('');
  const [loading, setLoading] = useState(true);
  const [searching, setSearching] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Fetch curated models from the models API (same as main frontend)
  const fetchCuratedModels = useCallback(async () => {
    setLoading(true);
    setError(null);

    try {
      const result = await api.models.list();
      setCuratedModels(result.models);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch models');
    } finally {
      setLoading(false);
    }
  }, [api]);

  // Search HuggingFace
  const searchHuggingFace = useCallback(async () => {
    if (!searchQuery.trim()) {
      setSearchResults([]);
      return;
    }

    setSearching(true);
    setError(null);

    try {
      const result = await api.huggingFace.searchModels(searchQuery, { limit: 20 });
      setSearchResults(result.models);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Search failed');
      setSearchResults([]);
    } finally {
      setSearching(false);
    }
  }, [api, searchQuery]);

  // Initial fetch
  useEffect(() => {
    fetchCuratedModels();
  }, [fetchCuratedModels]);

  // Search on query change (debounced)
  useEffect(() => {
    if (activeTab !== 'huggingface') return;

    const timeout = setTimeout(() => {
      searchHuggingFace();
    }, 500);

    return () => clearTimeout(timeout);
  }, [searchQuery, activeTab, searchHuggingFace]);

  // Curated Models Tab Content
  const CuratedModelsContent = (
    <>
      {loading ? (
        <Loader title="Loading models..." />
      ) : error ? (
        <ConnectionError error={error} onRetry={fetchCuratedModels} />
      ) : (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(300px, 1fr))', gap: '16px', paddingBottom: '32px' }}>
          {curatedModels.map((model) => {
            const computeType = getComputeType(model);
            return (
              <div
                key={model.id}
                style={{
                  border: '1px solid rgba(128, 128, 128, 0.3)',
                  borderRadius: '8px',
                  padding: '16px',
                  backgroundColor: 'transparent',
                  display: 'flex',
                  flexDirection: 'column',
                }}
              >
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: '8px' }}>
                  <h3 style={{ margin: 0, fontSize: '16px' }}>{model.name}</h3>
                  <span
                    style={{
                      padding: '2px 8px',
                      borderRadius: '4px',
                      backgroundColor: getBadgeColors(computeType).bg,
                      color: getBadgeColors(computeType).color,
                      fontSize: '12px',
                    }}
                  >
                    {computeType.toUpperCase()}
                  </span>
                </div>

                <div style={{ fontSize: '14px', opacity: 0.7, marginBottom: '12px' }}>
                  <div>Size: {model.size}</div>
                  {model.license && <div>License: {model.license}</div>}
                </div>

                {model.description && (
                  <p style={{ fontSize: '13px', opacity: 0.6, marginBottom: '12px', flexGrow: 1 }}>
                    {model.description}
                  </p>
                )}
                {!model.description && <div style={{ flexGrow: 1 }} />}

                <Button
                  variant="contained"
                  color="primary"
                  size="medium"
                  startIcon={<Icon icon="mdi:plus"/>}
                  sx={{ fontWeight: 600, boxShadow: 3, alignSelf: 'flex-start', mt: 'auto', display: 'flex', alignItems: 'center' }}
                  onClick={() => {
                    const url = Router.createRouteURL('Create Deployment');
                    history.push(`${url}?modelId=${encodeURIComponent(model.id)}`);
                  }}
                >
                  Deploy
                </Button>
              </div>
            );
          })}
        </div>
      )}
    </>
  );

  // HuggingFace Search Tab Content
  const HuggingFaceContent = (
    <>
      <div style={{ marginBottom: '24px' }}>
        <input
          type="text"
          placeholder="Search HuggingFace models..."
          value={searchQuery}
          onChange={(e) => setSearchQuery(e.target.value)}
          style={{
            width: '100%',
            maxWidth: '400px',
            padding: '10px 16px',
            border: '1px solid rgba(128, 128, 128, 0.3)',
            borderRadius: '4px',
            fontSize: '14px',
            backgroundColor: 'transparent',
            color: 'inherit',
          }}
        />
      </div>

      {searching ? (
        <Loader title="Searching..." />
      ) : error ? (
        <ConnectionError error={error} onRetry={searchHuggingFace} />
      ) : searchResults.length === 0 ? (
        <div style={{ padding: '24px', textAlign: 'center', opacity: 0.7 }}>
          {searchQuery ? 'No models found.' : 'Enter a search query to find models.'}
        </div>
      ) : (
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(300px, 1fr))', gap: '16px', paddingBottom: '32px' }}>
          {searchResults.map((model) => (
            <div
              key={model.id}
              style={{
                border: '1px solid rgba(128, 128, 128, 0.3)',
                borderRadius: '8px',
                padding: '16px',
                backgroundColor: 'transparent',
                display: 'flex',
                flexDirection: 'column',
              }}
            >
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', marginBottom: '8px' }}>
                <h3 style={{ margin: 0, fontSize: '14px', wordBreak: 'break-word', flex: 1 }}>
                  {model.id}
                </h3>
                {!model.compatible && (
                  <span style={{ padding: '2px 6px', backgroundColor: getBadgeColors('error').bg, color: getBadgeColors('error').color, fontSize: '11px', borderRadius: '4px', marginLeft: '8px' }}>
                    Incompatible
                  </span>
                )}
              </div>

              <div style={{ fontSize: '13px', opacity: 0.7, marginBottom: '8px' }}>
                <div>Downloads: {model.downloads?.toLocaleString() || 'N/A'}</div>
                <div>Likes: {model.likes?.toLocaleString() || 'N/A'}</div>
                {model.pipelineTag && <div>Task: {model.pipelineTag}</div>}
                {model.estimatedGpuMemory && <div>GPU Memory: {model.estimatedGpuMemory}</div>}
              </div>

              {model.supportedEngines && model.supportedEngines.length > 0 && (
                <div style={{ display: 'flex', flexWrap: 'wrap', gap: '4px', marginBottom: '12px' }}>
                  {model.supportedEngines.map((engine) => (
                    <span
                      key={engine}
                      style={{
                        padding: '2px 6px',
                        backgroundColor: getBadgeColors('info').bg,
                        borderRadius: '4px',
                        fontSize: '11px',
                        color: getBadgeColors('info').color,
                      }}
                    >
                      {engine}
                    </span>
                  ))}
                </div>
              )}

              <div style={{ flexGrow: 1 }} />
              <Button
                variant="contained"
                color={model.compatible ? 'primary' : 'inherit'}
                size="small"
                startIcon={<Icon icon="mdi:plus" />}
                sx={{ fontWeight: 600, boxShadow: 3, alignSelf: 'flex-start', mt: 'auto', display: 'flex', alignItems: 'center' }}
                disabled={!model.compatible}
                onClick={() => {
                  const url = Router.createRouteURL('Create Deployment');
                  history.push(`${url}?modelId=${encodeURIComponent(model.id)}&source=huggingface`);
                }}
              >
                Deploy
              </Button>
            </div>
          ))}
        </div>
      )}
    </>
  );

  const tabs = [
    { label: 'Curated Models', component: CuratedModelsContent },
    { label: 'HuggingFace Search', component: HuggingFaceContent },
  ];

  return (
    <SectionBox title="Model Catalog">
      <Tabs
        tabs={tabs}
        ariaLabel="Model catalog tabs"
        onTabChanged={(index) => setActiveTab(index === 0 ? 'curated' : 'huggingface')}
        sx={{ borderBottom: 1, borderColor: 'divider', marginBottom: 2 }}
      />
    </SectionBox>
  );
}
