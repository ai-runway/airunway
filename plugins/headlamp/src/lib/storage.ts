import { storageAfterNamespaceChange } from '@airunway/shared';
import type { StorageVolume } from '@airunway/shared';

export function storageVolumeWithSize(volume: StorageVolume, size: string): StorageVolume {
  const normalizedSize = size.trim() || undefined;

  return {
    ...volume,
    size: normalizedSize,
    claimName: normalizedSize ? undefined : volume.claimName,
    accessMode: normalizedSize ? volume.accessMode ?? 'ReadWriteMany' : volume.accessMode,
  };
}

export function storageVolumeWithExistingClaim(
  volume: StorageVolume,
  claimName: string
): StorageVolume {
  const normalizedClaimName = claimName.trim();
  if (!normalizedClaimName) {
    return {
      ...volume,
      claimName: undefined,
    };
  }

  return {
    ...volume,
    claimName: normalizedClaimName,
    size: undefined,
    storageClassName: undefined,
    accessMode: undefined,
  };
}

export function storageVolumesAfterNamespaceChange(
  volumes: StorageVolume[],
  currentNamespace: string,
  nextNamespace: string
): StorageVolume[] {
  return storageAfterNamespaceChange(
    { volumes },
    currentNamespace,
    nextNamespace
  )?.volumes ?? [];
}
