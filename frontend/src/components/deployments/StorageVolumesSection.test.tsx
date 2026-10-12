import { useState } from 'react'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
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
  discoveredPVCs = availablePVCs,
}: {
  initialVolumes: StorageVolume[]
  onChange: (volumes: StorageVolume[]) => void
  discoveredPVCs?: typeof availablePVCs
}) {
  const [volumes, setVolumes] = useState(initialVolumes)

  return (
    <StorageVolumesSection
      volumes={volumes}
      onChange={(updatedVolumes) => {
        onChange(updatedVolumes)
        setVolumes(updatedVolumes)
      }}
      availablePVCs={discoveredPVCs}
    />
  )
}

function ReindexingStorageVolumesSection() {
  const [volumes, setVolumes] = useState<StorageVolume[]>([
    {
      name: 'first-cache',
      purpose: 'custom',
      mountPath: '/first',
      size: '100Gi',
      accessMode: 'ReadWriteMany',
    },
    {
      name: 'second-cache',
      purpose: 'custom',
      mountPath: '/second',
      size: '200Gi',
      accessMode: 'ReadWriteOnce',
    },
  ])

  return (
    <>
      <button type="button" onClick={() => setVolumes((current) => [current[1]])}>
        Remove first volume externally
      </button>
      <StorageVolumesSection volumes={volumes} onChange={setVolumes} />
    </>
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

  it('keeps an empty manually entered existing disk visible and correctable', () => {
    render(<ControlledStorageVolumesSection initialVolumes={[staleExistingVolume]} onChange={vi.fn()} discoveredPVCs={[]} />)
    const claim = screen.getByPlaceholderText('my-shared-storage')
    fireEvent.change(claim, { target: { value: '' } })
    fireEvent.blur(claim)
    expect(screen.getByRole('radio', { name: 'Use existing disk' })).toBeChecked()
    expect(screen.getByPlaceholderText('my-shared-storage')).toHaveValue('')
    expect(screen.getByText('A disk name is required when using existing storage')).toBeInTheDocument()
    fireEvent.change(screen.getByPlaceholderText('my-shared-storage'), { target: { value: 'replacement' } })
    expect(screen.getByPlaceholderText('my-shared-storage')).toHaveValue('replacement')
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

  it('derives the source mode from the retained volume after external filtering', () => {
    render(<ReindexingStorageVolumesSection />)

    fireEvent.click(screen.getAllByRole('radio', { name: 'Use existing disk' })[0])
    fireEvent.click(screen.getByRole('button', { name: 'Remove first volume externally' }))

    expect(screen.getByRole('radio', { name: 'Create new disk' })).toBeChecked()
    expect(screen.getByLabelText('Disk Size')).toHaveValue('200Gi')
  })
})
