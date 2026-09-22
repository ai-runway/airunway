import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { StorageVolumesEditor } from './StorageVolumesEditor';

describe('managed storage defaults', () => {
  it('adds a volume with the API multi-node access default', () => {
    const onChange = vi.fn();
    render(<StorageVolumesEditor volumes={[]} onChange={onChange} />);

    fireEvent.click(screen.getByRole('button', { name: 'Add storage volume' }));

    expect(onChange).toHaveBeenCalledWith([
      expect.objectContaining({ size: '100Gi', accessMode: 'ReadWriteMany' }),
    ]);
  });

  it('displays the API default for a managed volume with no explicit access mode', () => {
    render(<StorageVolumesEditor
      volumes={[{ name: 'cache', purpose: 'custom', size: '100Gi' }]}
      onChange={vi.fn()}
    />);

    const accessMode = screen.getByRole('option', { name: 'ReadWriteMany' }).closest('select');
    expect(accessMode).toHaveValue('ReadWriteMany');
  });
});
