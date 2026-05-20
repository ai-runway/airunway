/**
 * Storage Volumes Display Component
 *
 * Read-only display of storage volumes attached to a deployment.
 */

import { SimpleTable } from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import type { StorageVolume, VolumePurpose } from '@airunway/shared';
import { PURPOSE_LABELS } from '../lib/constants';

interface StorageVolumesDisplayProps {
  volumes: StorageVolume[];
}

function getPurposeLabel(purpose?: VolumePurpose): string {
  if (!purpose) {
    return 'Custom';
  }
  return PURPOSE_LABELS[purpose] || 'Custom';
}

export function StorageVolumesDisplay({ volumes }: StorageVolumesDisplayProps) {
  if (volumes.length === 0) {
    return (
      <div style={{ textAlign: 'center', padding: '24px', opacity: 0.6 }}>
        No storage volumes
      </div>
    );
  }

  return (
    <SimpleTable
      columns={[
        { label: 'Name', getter: (v: StorageVolume) => v.name },
        { label: 'Purpose', getter: (v: StorageVolume) => getPurposeLabel(v.purpose) },
        { label: 'Size', getter: (v: StorageVolume) => v.size || '-' },
        {
          label: 'PVC',
          getter: (v: StorageVolume) => v.claimName || '(auto-created)',
        },
        {
          label: 'Access Mode',
          getter: (v: StorageVolume) => v.accessMode || '-',
        },
        {
          label: 'Storage Class',
          getter: (v: StorageVolume) => v.storageClassName || '(default)',
        },
      ]}
      data={volumes}
    />
  );
}
