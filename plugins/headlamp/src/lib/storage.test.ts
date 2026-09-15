import { describe, expect, it } from 'vitest';
import type { StorageVolume } from '@airunway/shared';
import {
  storageVolumesAfterNamespaceChange,
  storageVolumeWithExistingClaim,
  storageVolumeWithSize,
} from './storage';

const managedVolume: StorageVolume = {
  name: 'managed-cache',
  purpose: 'modelCache',
  size: '100Gi',
};

const existingClaimVolume: StorageVolume = {
  name: 'existing-cache',
  purpose: 'custom',
  claimName: 'existing-cache',
  mountPath: '/existing-cache',
};

describe('storageVolumesAfterNamespaceChange', () => {
  it('keeps managed volumes and removes existing claims when the namespace changes', () => {
    expect(storageVolumesAfterNamespaceChange(
      [managedVolume, existingClaimVolume, { ...existingClaimVolume, size: '' }],
      'dynamo-system',
      'kuberay-system'
    )).toEqual([managedVolume]);
  });

  it('preserves all volumes when the namespace stays the same', () => {
    expect(storageVolumesAfterNamespaceChange(
      [managedVolume, existingClaimVolume],
      'default',
      'default'
    )).toEqual([managedVolume, existingClaimVolume]);
  });
});

describe('storage volume source normalization', () => {
  it('clears managed-only fields when an existing claim is entered', () => {
    expect(storageVolumeWithExistingClaim(
      {
        ...managedVolume,
        storageClassName: 'fast-storage',
        accessMode: 'ReadWriteMany',
      },
      'existing-cache'
    )).toEqual({
      name: 'managed-cache',
      purpose: 'modelCache',
      claimName: 'existing-cache',
      size: undefined,
      storageClassName: undefined,
      accessMode: undefined,
    });
  });

  it('clears the existing claim when a managed size is entered', () => {
    expect(storageVolumeWithSize(existingClaimVolume, '200Gi')).toEqual({
      ...existingClaimVolume,
      claimName: undefined,
      size: '200Gi',
      accessMode: 'ReadWriteMany',
    });
  });

  it('restores the displayed access mode after a managed-existing-managed round trip', () => {
    const existing = storageVolumeWithExistingClaim(
      {
        ...managedVolume,
        accessMode: 'ReadWriteMany',
      },
      'existing-cache'
    );

    expect(storageVolumeWithSize(existing, '200Gi')).toEqual({
      name: 'managed-cache',
      purpose: 'modelCache',
      claimName: undefined,
      size: '200Gi',
      storageClassName: undefined,
      accessMode: 'ReadWriteMany',
    });
  });

  it('preserves an explicitly selected access mode when changing managed size', () => {
    expect(storageVolumeWithSize({
      ...managedVolume,
      accessMode: 'ReadWriteOnce',
    }, '200Gi').accessMode).toBe('ReadWriteOnce');
  });
});
