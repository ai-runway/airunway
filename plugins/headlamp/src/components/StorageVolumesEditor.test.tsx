import type { StorageVolume } from '@airunway/shared';
import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { StorageVolumesEditor } from './StorageVolumesEditor';

describe('StorageVolumesEditor', () => {
  it('adds a usable default volume', () => {
    const onChange = vi.fn();
    render(<StorageVolumesEditor volumes={[]} onChange={onChange} />);

    fireEvent.click(screen.getByRole('button', { name: 'Add storage volume' }));

    expect(onChange).toHaveBeenCalledOnce();
    expect(onChange.mock.calls[0]?.[0]).toEqual([
      expect.objectContaining({
        name: expect.stringMatching(/^volume-\d+$/),
        purpose: 'custom',
        size: '100Gi',
        accessMode: 'ReadWriteOnce',
      }),
    ]);
  });

  it('emits edited values without changing the other volumes', () => {
    const onChange = vi.fn();
    const volumes: StorageVolume[] = [
      { name: 'model-cache', purpose: 'modelCache', size: '100Gi' },
      { name: 'scratch', purpose: 'custom', size: '20Gi' },
    ];
    render(<StorageVolumesEditor volumes={volumes} onChange={onChange} />);

    fireEvent.change(screen.getByDisplayValue('model-cache'), {
      target: { value: 'shared-model-cache' },
    });

    expect(onChange).toHaveBeenCalledWith([
      { name: 'shared-model-cache', purpose: 'modelCache', size: '100Gi' },
      volumes[1],
    ]);

    const purposeSelect = screen.getAllByRole('combobox')[0];
    const compilationOption = purposeSelect?.querySelector('option[value="compilationCache"]');
    expect(compilationOption).not.toBeDisabled();
  });

  it('prevents duplicate singleton purposes and enforces the volume limit', () => {
    const onChange = vi.fn();
    const volumes: StorageVolume[] = Array.from({ length: 8 }, (_, index) => ({
      name: `volume-${index}`,
      purpose: index === 1 ? 'modelCache' : 'custom',
      size: '10Gi',
    }));
    render(<StorageVolumesEditor volumes={volumes} onChange={onChange} />);

    const purposeSelect = screen.getAllByRole('combobox')[0];
    expect(purposeSelect?.querySelector('option[value="modelCache"]')).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Add storage volume' })).toBeDisabled();
    expect(screen.getByText('Maximum of 8 volumes reached')).toBeInTheDocument();
  });

  it('removes a volume from the keyboard without toggling its details', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const volumes: StorageVolume[] = [
      { name: 'model-cache', purpose: 'modelCache', size: '100Gi' },
      { name: 'scratch', purpose: 'custom', size: '20Gi' },
    ];
    render(<StorageVolumesEditor volumes={volumes} onChange={onChange} />);

    const removeButton = screen.getByRole('button', { name: 'Remove scratch' });
    const expandButton = screen.getByRole('button', { name: 'Hide model-cache details' });
    const expandedBefore = expandButton.getAttribute('aria-expanded');

    removeButton.focus();
    await user.keyboard('{Enter}');

    expect(expandButton.getAttribute('aria-expanded')).toBe(expandedBefore);
    expect(onChange).toHaveBeenCalledWith([volumes[0]]);
  });
});
