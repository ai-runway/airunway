/**
 * Storage Volumes Editor Component
 *
 * Allows users to add and configure persistent storage volumes
 * (model cache, compilation cache, custom) for deployments.
 */

import type { PersistentVolumeAccessMode, StorageVolume, VolumePurpose } from '@airunway/shared';
import { Icon } from '@iconify/react';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import { useRef, useState } from 'react';
import { PURPOSE_LABELS } from '../lib/constants';

interface StorageVolumesEditorProps {
  volumes: StorageVolume[];
  onChange: (volumes: StorageVolume[]) => void;
}

const MAX_VOLUMES = 8;

const PURPOSE_BADGE_COLORS: Record<VolumePurpose, { bg: string; color: string }> = {
  modelCache: { bg: '#e3f2fd', color: '#1565c0' },
  compilationCache: { bg: '#fff3e0', color: '#e65100' },
  custom: { bg: '#f3e5f5', color: '#7b1fa2' },
};

const ACCESS_MODE_LABELS: Record<PersistentVolumeAccessMode, string> = {
  ReadWriteOnce: 'ReadWriteOnce',
  ReadWriteMany: 'ReadWriteMany',
  ReadOnlyMany: 'ReadOnlyMany',
  ReadWriteOncePod: 'ReadWriteOncePod',
};

let volumeCounter = 0;

function createDefaultVolume(): StorageVolume {
  volumeCounter++;
  return {
    name: `volume-${volumeCounter}`,
    purpose: 'custom',
    size: '100Gi',
    accessMode: 'ReadWriteOnce',
  };
}

const inputStyle: React.CSSProperties = {
  width: '100%',
  padding: '10px 12px',
  border: '1px solid rgba(128, 128, 128, 0.3)',
  borderRadius: '4px',
  backgroundColor: 'transparent',
  color: 'inherit',
  boxSizing: 'border-box',
};

const selectStyle: React.CSSProperties = {
  ...inputStyle,
  appearance: 'auto',
};

const labelStyle: React.CSSProperties = {
  display: 'block',
  marginBottom: '6px',
  fontWeight: 500,
  fontSize: '13px',
};

export function StorageVolumesEditor({ volumes, onChange }: StorageVolumesEditorProps) {
  const [expandedIndex, setExpandedIndex] = useState<number | null>(
    volumes.length > 0 ? 0 : null
  );

  // Stable React keys keyed by volume object identity. Survives reorders and
  // removals so input focus / expanded state don't jump to the wrong row.
  const keyMap = useRef(new WeakMap<StorageVolume, string>());
  const nextKey = useRef(0);
  function getVolumeKey(vol: StorageVolume): string {
    let key = keyMap.current.get(vol);
    if (!key) {
      nextKey.current += 1;
      key = `vol-${nextKey.current}`;
      keyMap.current.set(vol, key);
    }
    return key;
  }

  // Determine which singleton purposes are already taken
  const usedSingletonPurposes = new Set<VolumePurpose>();
  for (const vol of volumes) {
    if (vol.purpose === 'modelCache' || vol.purpose === 'compilationCache') {
      usedSingletonPurposes.add(vol.purpose);
    }
  }

  function handleAdd() {
    if (volumes.length >= MAX_VOLUMES) return;
    const newVolumes = [...volumes, createDefaultVolume()];
    onChange(newVolumes);
    setExpandedIndex(newVolumes.length - 1);
  }

  function handleRemove(index: number) {
    const newVolumes = volumes.filter((_, i) => i !== index);
    onChange(newVolumes);
    if (expandedIndex === index) {
      setExpandedIndex(null);
    } else if (expandedIndex !== null && expandedIndex > index) {
      setExpandedIndex(expandedIndex - 1);
    }
  }

  function handleUpdate(index: number, updates: Partial<StorageVolume>) {
    const newVolumes = volumes.map((vol, i) => {
      if (i !== index) return vol;
      const updated = { ...vol, ...updates };
      // Preserve the stable React key across the object replacement.
      const existing = keyMap.current.get(vol);
      if (existing) keyMap.current.set(updated, existing);
      return updated;
    });
    onChange(newVolumes);
  }

  function toggleExpanded(index: number) {
    setExpandedIndex(expandedIndex === index ? null : index);
  }

  return (
    <div>
      {volumes.map((volume, index) => {
        const isExpanded = expandedIndex === index;
        const purpose = volume.purpose || 'custom';
        const badgeColors = PURPOSE_BADGE_COLORS[purpose];

        return (
          <div
            key={getVolumeKey(volume)}
            style={{
              border: '1px solid rgba(128, 128, 128, 0.3)',
              borderRadius: '8px',
              marginBottom: '12px',
              overflow: 'hidden',
            }}
          >
            {/* Card Header */}
            <div
              style={{
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'space-between',
                backgroundColor: 'rgba(128, 128, 128, 0.05)',
              }}
            >
              <button
                type="button"
                aria-expanded={isExpanded}
                aria-label={`${isExpanded ? 'Hide' : 'Show'} ${volume.name || 'unnamed volume'} details`}
                onClick={() => toggleExpanded(index)}
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  flex: 1,
                  gap: '12px',
                  padding: '12px 8px 12px 16px',
                  border: 0,
                  cursor: 'pointer',
                  background: 'transparent',
                  color: 'inherit',
                  font: 'inherit',
                  textAlign: 'left',
                }}
              >
                <Icon
                  icon={isExpanded ? 'mdi:chevron-down' : 'mdi:chevron-right'}
                  width={20}
                  style={{ opacity: 0.7 }}
                />
                <span style={{ fontWeight: 500 }}>{volume.name || 'Unnamed volume'}</span>
                <span
                  style={{
                    padding: '2px 8px',
                    backgroundColor: badgeColors.bg,
                    color: badgeColors.color,
                    borderRadius: '4px',
                    fontSize: '11px',
                    fontWeight: 500,
                  }}
                >
                  {PURPOSE_LABELS[purpose]}
                </span>
              </button>
              <IconButton
                size="small"
                aria-label={`Remove ${volume.name || 'unnamed volume'}`}
                onClick={() => handleRemove(index)}
                sx={{ color: 'inherit', opacity: 0.7, marginRight: '8px' }}
              >
                <Icon icon="mdi:close" width={18} />
              </IconButton>
            </div>

            {/* Card Body */}
            {isExpanded && (
              <div style={{ padding: '16px', borderTop: '1px solid rgba(128, 128, 128, 0.2)' }}>
                <div style={{ display: 'grid', gap: '16px', maxWidth: '500px' }}>
                  {/* Name */}
                  <div>
                    <label htmlFor={`storage-volume-${index}-name`} style={labelStyle}>Name</label>
                    <input
                      id={`storage-volume-${index}-name`}
                      type="text"
                      value={volume.name}
                      onChange={(e) => handleUpdate(index, { name: e.target.value })}
                      placeholder="e.g. model-cache"
                      style={inputStyle}
                    />
                  </div>

                  {/* Purpose + Size row */}
                  <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '16px' }}>
                    <div>
                      <label htmlFor={`storage-volume-${index}-purpose`} style={labelStyle}>Purpose</label>
                      <select
                        id={`storage-volume-${index}-purpose`}
                        value={purpose}
                        onChange={(e) =>
                          handleUpdate(index, { purpose: e.target.value as VolumePurpose })
                        }
                        style={selectStyle}
                      >
                        {(Object.keys(PURPOSE_LABELS) as VolumePurpose[]).map((p) => {
                          const isSingleton = p === 'modelCache' || p === 'compilationCache';
                          const isUsedByOther = usedSingletonPurposes.has(p) && volume.purpose !== p;
                          const disabled = isSingleton && isUsedByOther;
                          return (
                            <option key={p} value={p} disabled={disabled}>
                              {PURPOSE_LABELS[p]}{disabled ? ' (already added)' : ''}
                            </option>
                          );
                        })}
                      </select>
                    </div>
                    <div>
                      <label htmlFor={`storage-volume-${index}-size`} style={labelStyle}>Size</label>
                      <input
                        id={`storage-volume-${index}-size`}
                        type="text"
                        value={volume.size || ''}
                        onChange={(e) => handleUpdate(index, { size: e.target.value })}
                        placeholder="e.g. 100Gi"
                        style={inputStyle}
                      />
                    </div>
                  </div>

                  {/* Access Mode */}
                  <div>
                    <label htmlFor={`storage-volume-${index}-access-mode`} style={labelStyle}>Access Mode</label>
                    <select
                      id={`storage-volume-${index}-access-mode`}
                      value={volume.accessMode || 'ReadWriteOnce'}
                      onChange={(e) =>
                        handleUpdate(index, {
                          accessMode: e.target.value as PersistentVolumeAccessMode,
                        })
                      }
                      style={selectStyle}
                    >
                      {(Object.keys(ACCESS_MODE_LABELS) as PersistentVolumeAccessMode[]).map(
                        (mode) => (
                          <option key={mode} value={mode}>
                            {ACCESS_MODE_LABELS[mode]}
                          </option>
                        )
                      )}
                    </select>
                  </div>

                  {/* Existing PVC name */}
                  <div>
                    <label htmlFor={`storage-volume-${index}-claim-name`} style={labelStyle}>Existing PVC Name (optional)</label>
                    <input
                      id={`storage-volume-${index}-claim-name`}
                      type="text"
                      value={volume.claimName || ''}
                      onChange={(e) =>
                        handleUpdate(index, { claimName: e.target.value || undefined })
                      }
                      placeholder="Leave blank to create a new volume"
                      style={inputStyle}
                    />
                    <div style={{ fontSize: '12px', opacity: 0.6, marginTop: '4px' }}>
                      Use an existing persistent volume claim instead of creating a new one
                    </div>
                  </div>

                  {/* Storage Class */}
                  <div>
                    <label htmlFor={`storage-volume-${index}-storage-class`} style={labelStyle}>Storage Class (optional)</label>
                    <input
                      id={`storage-volume-${index}-storage-class`}
                      type="text"
                      value={volume.storageClassName || ''}
                      onChange={(e) =>
                        handleUpdate(index, { storageClassName: e.target.value || undefined })
                      }
                      placeholder="Leave blank for cluster default"
                      style={inputStyle}
                    />
                  </div>
                </div>
              </div>
            )}
          </div>
        );
      })}

      <Button
        size="small"
        onClick={handleAdd}
        disabled={volumes.length >= MAX_VOLUMES}
        startIcon={<Icon icon="mdi:plus" />}
        sx={{ textTransform: 'none', mt: volumes.length > 0 ? 1 : 0 }}
      >
        Add storage volume
      </Button>

      {volumes.length >= MAX_VOLUMES && (
        <div style={{ fontSize: '12px', opacity: 0.6, marginTop: '4px' }}>
          Maximum of {MAX_VOLUMES} volumes reached
        </div>
      )}
    </div>
  );
}
