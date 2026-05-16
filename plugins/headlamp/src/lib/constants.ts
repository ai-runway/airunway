/**
 * Shared constants for the Headlamp plugin
 */

import type { VolumePurpose } from '@airunway/shared';

/** Human-readable labels for storage volume purposes */
export const PURPOSE_LABELS: Record<VolumePurpose, string> = {
  modelCache: 'Model Cache',
  compilationCache: 'Compilation Cache',
  custom: 'Custom',
};
