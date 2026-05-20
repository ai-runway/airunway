import { useState } from 'react'
import { render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import type { StorageVolume } from '@airunway/shared'
import { StorageVolumesSection } from './StorageVolumesSection'

const staleExistingVolume = {
  name: 'data',
  purpose: 'custom' as const,
  mountPath: '/data',
  claimName: 'old-pvc',
}

const availablePVCs = [
  { name: 'current-pvc', status: 'Bound', storageClass: 'standard', capacity: '10Gi' },
]

function ControlledStorageVolumesSection({
  initialVolumes,
  onChange,
}: {
  initialVolumes: StorageVolume[]
  onChange: (volumes: StorageVolume[]) => void
}) {
  const [volumes, setVolumes] = useState(initialVolumes)

  return (
    <StorageVolumesSection
      volumes={volumes}
      onChange={(updatedVolumes) => {
        onChange(updatedVolumes)
        setVolumes(updatedVolumes)
      }}
      availablePVCs={availablePVCs}
    />
  )
}

describe('StorageVolumesSection', () => {
  it('clears stale existing PVC selections when the available PVC list changes', async () => {
    const onChange = vi.fn()

    render(
      <ControlledStorageVolumesSection
        initialVolumes={[staleExistingVolume]}
        onChange={onChange}
      />
    )

    await waitFor(() => {
      expect(onChange).toHaveBeenCalledWith([
        { ...staleExistingVolume, claimName: '' },
      ])
    })
    expect(screen.getByText('A disk name is required when using existing storage')).toBeInTheDocument()
  })

  it('keeps a selected PVC that is still available', async () => {
    const onChange = vi.fn()

    render(
      <StorageVolumesSection
        volumes={[{ ...staleExistingVolume, claimName: 'current-pvc' }]}
        onChange={onChange}
        availablePVCs={availablePVCs}
      />
    )

    await waitFor(() => expect(onChange).not.toHaveBeenCalled())
  })
})
