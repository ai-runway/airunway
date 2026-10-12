/**
 * Delete Confirmation Dialog Component
 *
 * Displays a confirmation dialog before deleting a resource.
 * Uses MUI Dialog to properly support Headlamp's dark/light theme.
 */

import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';

interface DeleteDialogProps {
  open: boolean;
  resourceName: string;
  resourceType?: string;
  onConfirm: () => void;
  onCancel: () => void;
  loading?: boolean;
}

export function DeleteDialog({ open, resourceName, resourceType = 'deployment', onConfirm, onCancel, loading = false }: DeleteDialogProps) {
  return (
    <Dialog
      open={open}
      onClose={loading ? undefined : onCancel}
      maxWidth="xs"
      fullWidth
    >
      <DialogTitle>Delete {resourceType}?</DialogTitle>
      <DialogContent>
        <DialogContentText>
          Are you sure you want to delete <strong>{resourceName}</strong>? This action cannot be undone.
        </DialogContentText>
      </DialogContent>
      <DialogActions>
        <Button
          onClick={onCancel}
          disabled={loading}
        >
          Cancel
        </Button>
        <Button
          onClick={onConfirm}
          disabled={loading}
          color="error"
          variant="contained"
        >
          {loading ? 'Deleting...' : 'Delete'}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
