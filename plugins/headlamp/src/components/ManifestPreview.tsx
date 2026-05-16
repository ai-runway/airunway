/**
 * Manifest Preview Component
 *
 * Displays a read-only preview of the Kubernetes manifest (YAML/JSON)
 * that will be created when deploying a model. Includes copy-to-clipboard
 * and a refresh/retry action.
 */

import { useState, useCallback, useEffect, useRef } from 'react';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import { Icon } from '@iconify/react';
import { copyToClipboard } from '../lib/utils';

interface ManifestPreviewProps {
  manifest: string | null;
  loading: boolean;
  error: string | null;
  onRefresh: () => void;
}

export function ManifestPreview({ manifest, loading, error, onRefresh }: ManifestPreviewProps) {
  const [copied, setCopied] = useState(false);
  const [copyFailed, setCopyFailed] = useState(false);
  const copyTimerRef = useRef<ReturnType<typeof setTimeout>>();

  useEffect(() => {
    return () => clearTimeout(copyTimerRef.current);
  }, []);

  const handleCopy = useCallback(async () => {
    if (!manifest) return;
    const success = await copyToClipboard(manifest);
    if (success) {
      setCopied(true);
      setCopyFailed(false);
      clearTimeout(copyTimerRef.current);
      copyTimerRef.current = setTimeout(() => setCopied(false), 2000);
    } else {
      setCopyFailed(true);
      clearTimeout(copyTimerRef.current);
      copyTimerRef.current = setTimeout(() => setCopyFailed(false), 3000);
    }
  }, [manifest]);

  // Loading state
  if (loading) {
    return (
      <div style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        gap: '12px',
        padding: '32px',
        backgroundColor: 'rgba(128, 128, 128, 0.05)',
        borderRadius: '6px',
        border: '1px solid rgba(128, 128, 128, 0.2)',
      }}>
        <CircularProgress size={20} />
        <span style={{ opacity: 0.7 }}>Generating manifest preview...</span>
      </div>
    );
  }

  // Error state
  if (error) {
    return (
      <div style={{
        padding: '24px',
        backgroundColor: 'rgba(198, 40, 40, 0.08)',
        borderRadius: '6px',
        border: '1px solid rgba(198, 40, 40, 0.3)',
        textAlign: 'center',
      }}>
        <div style={{ color: '#f44336', marginBottom: '12px' }}>
          {error}
        </div>
        <Button
          size="small"
          onClick={onRefresh}
          startIcon={<Icon icon="mdi:refresh" />}
          sx={{ textTransform: 'none' }}
        >
          Retry
        </Button>
      </div>
    );
  }

  // Empty state
  if (!manifest) {
    return (
      <div style={{
        padding: '32px',
        textAlign: 'center',
        backgroundColor: 'rgba(128, 128, 128, 0.05)',
        borderRadius: '6px',
        border: '1px solid rgba(128, 128, 128, 0.2)',
        opacity: 0.7,
      }}>
        Configure your deployment to see a preview
      </div>
    );
  }

  // Manifest display
  return (
    <div style={{
      borderRadius: '6px',
      border: '1px solid rgba(128, 128, 128, 0.2)',
      overflow: 'hidden',
    }}>
      {/* Toolbar */}
      <div style={{
        display: 'flex',
        justifyContent: 'space-between',
        alignItems: 'center',
        padding: '8px 12px',
        backgroundColor: 'rgba(128, 128, 128, 0.08)',
        borderBottom: '1px solid rgba(128, 128, 128, 0.2)',
      }}>
        <span style={{ fontSize: '12px', opacity: 0.7, fontWeight: 500 }}>
          ModelDeployment Manifest
        </span>
        <div style={{ display: 'flex', gap: '8px' }}>
          <Button
            size="small"
            onClick={onRefresh}
            startIcon={<Icon icon="mdi:refresh" width={16} />}
            sx={{ textTransform: 'none', fontSize: '12px', minWidth: 'auto', py: 0.25 }}
          >
            Refresh
          </Button>
          <Button
            size="small"
            onClick={handleCopy}
            startIcon={<Icon icon={copied ? 'mdi:check' : 'mdi:content-copy'} width={16} />}
            sx={{ textTransform: 'none', fontSize: '12px', minWidth: 'auto', py: 0.25 }}
          >
            {copied ? 'Copied' : copyFailed ? 'Copy failed' : 'Copy'}
          </Button>
        </div>
      </div>

      {/* Code block */}
      <pre style={{
        margin: 0,
        padding: '16px',
        backgroundColor: 'rgba(128, 128, 128, 0.05)',
        fontFamily: '"Roboto Mono", "Consolas", "Monaco", monospace',
        fontSize: '13px',
        lineHeight: '1.5',
        overflowX: 'auto',
        whiteSpace: 'pre',
        maxHeight: '480px',
        overflowY: 'auto',
      }}>
        {manifest}
      </pre>
    </div>
  );
}
