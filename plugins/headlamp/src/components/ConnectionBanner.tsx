/**
 * Connection Banner Component
 *
 * Shows a banner when the AI Runway backend is not reachable.
 * Provides helpful information about how to fix the connection issue.
 */

import { useEffect, useState } from 'react';
import { useHistory } from 'react-router-dom';
import { api } from '../lib/api-client';
import { getBackendUrlSync } from '../lib/backend-discovery';

interface ConnectionBannerProps {
  /** Only show the banner on connection error, not during loading */
  showOnError?: boolean;
}

type ConnectionStatus = 'checking' | 'connected' | 'error';

export function ConnectionBanner({ showOnError = true }: ConnectionBannerProps) {
  const history = useHistory();
  const [status, setStatus] = useState<ConnectionStatus>('checking');
  const [errorMessage, setErrorMessage] = useState<string>('');
  const [backendUrl, setBackendUrl] = useState<string>('');
  const [dismissed, setDismissed] = useState(false);

  useEffect(() => {
    checkConnection();
    // Re-check every 30 seconds
    const interval = setInterval(checkConnection, 30000);
    return () => clearInterval(interval);
  }, []);

  async function checkConnection() {
    setBackendUrl(getBackendUrlSync());
    try {
      await api.health.check();
      setStatus('connected');
      setDismissed(false); // Reset dismissed state when connection recovers
    } catch (err) {
      setStatus('error');
      setErrorMessage(err instanceof Error ? err.message : 'Connection failed');
    }
  }

  // Don't show if connected or dismissed
  if (status === 'connected' || dismissed) {
    return null;
  }

  // Don't show during initial check if showOnError is true
  if (status === 'checking' && showOnError) {
    return null;
  }

  return (
    <div
      style={{
        padding: '12px 16px',
        backgroundColor: status === 'error' ? 'rgba(198, 40, 40, 0.15)' : 'rgba(255, 152, 0, 0.15)',
        borderBottom: `1px solid ${status === 'error' ? 'rgba(198, 40, 40, 0.3)' : 'rgba(255, 152, 0, 0.3)'}`,
        display: 'flex',
        alignItems: 'center',
        gap: '16px',
      }}
    >
      <div style={{ flex: 1 }}>
        <div
          style={{
            fontWeight: 500,
            color: status === 'error' ? '#f44336' : '#ff9800',
            marginBottom: '4px',
          }}
        >
          {status === 'checking' && '⏳ Connecting to AI Runway backend...'}
          {status === 'error' && '⚠️ Cannot connect to AI Runway backend'}
        </div>
        {status === 'error' && (
          <div style={{ fontSize: '13px', opacity: 0.8 }}>
            <span>Backend URL: {backendUrl}</span>
            {errorMessage && <span style={{ marginLeft: '8px' }}>({errorMessage})</span>}
          </div>
        )}
      </div>

      <div style={{ display: 'flex', gap: '8px' }}>
        <button
          onClick={checkConnection}
          style={{
            padding: '6px 12px',
            backgroundColor: 'transparent',
            border: '1px solid rgba(128, 128, 128, 0.3)',
            borderRadius: '4px',
            cursor: 'pointer',
            fontSize: '13px',
            color: 'inherit',
          }}
        >
          Retry
        </button>
        <button
          onClick={() => history.push('/c/airunway/settings')}
          style={{
            padding: '6px 12px',
            backgroundColor: '#1976d2',
            color: 'white',
            border: 'none',
            borderRadius: '4px',
            cursor: 'pointer',
            fontSize: '13px',
          }}
        >
          Settings
        </button>
        <button
          onClick={() => setDismissed(true)}
          style={{
            padding: '6px 12px',
            backgroundColor: 'transparent',
            border: 'none',
            cursor: 'pointer',
            fontSize: '16px',
            color: 'inherit',
            opacity: 0.7,
          }}
          title="Dismiss"
        >
          ✕
        </button>
      </div>
    </div>
  );
}

/**
 * Inline connection error component for page-level error handling
 */
interface ConnectionErrorProps {
  error: string;
  onRetry?: () => void;
}

export function ConnectionError({ error, onRetry }: ConnectionErrorProps) {
  const history = useHistory();
  const backendUrl = getBackendUrlSync();

  const isConnectionError =
    error.includes('Failed to fetch') ||
    error.includes('Network') ||
    error.includes('ECONNREFUSED') ||
    error.includes('Connection');

  return (
    <div
      style={{
        padding: '32px',
        textAlign: 'center',
        backgroundColor: 'rgba(128, 128, 128, 0.1)',
        borderRadius: '8px',
        margin: '16px',
      }}
    >
      <div style={{ fontSize: '48px', marginBottom: '16px' }}>
        {isConnectionError ? '🔌' : '⚠️'}
      </div>
      <h2 style={{ color: '#f44336', marginBottom: '8px' }}>
        {isConnectionError ? 'Cannot Connect to Backend' : 'Error'}
      </h2>
      <p style={{ opacity: 0.7, marginBottom: '16px' }}>{error}</p>

      {isConnectionError && (
        <div
          style={{
            backgroundColor: 'rgba(128, 128, 128, 0.1)',
            padding: '16px',
            borderRadius: '8px',
            border: '1px solid rgba(128, 128, 128, 0.3)',
            marginBottom: '24px',
            textAlign: 'left',
            maxWidth: '500px',
            marginLeft: 'auto',
            marginRight: 'auto',
          }}
        >
          <div style={{ fontWeight: 500, marginBottom: '12px' }}>
            Troubleshooting Steps:
          </div>
          <ol style={{ margin: 0, paddingLeft: '20px', lineHeight: 1.8, opacity: 0.8 }}>
            <li>
              Ensure the AI Runway backend is running:
              <code
                style={{
                  display: 'block',
                  backgroundColor: 'rgba(128, 128, 128, 0.2)',
                  padding: '8px',
                  borderRadius: '4px',
                  marginTop: '4px',
                  fontSize: '13px',
                }}
              >
                cd airunway && bun run dev
              </code>
            </li>
            <li>
              Check the backend URL in Settings
              <div style={{ fontSize: '13px', opacity: 0.7, marginTop: '2px' }}>
                Current: {backendUrl}
              </div>
            </li>
            <li>
              For in-cluster deployments, ensure AI Runway is deployed to your cluster
            </li>
          </ol>
        </div>
      )}

      <div style={{ display: 'flex', gap: '8px', justifyContent: 'center' }}>
        {onRetry && (
          <button
            onClick={onRetry}
            style={{
              padding: '10px 20px',
              backgroundColor: '#1976d2',
              color: 'white',
              border: 'none',
              borderRadius: '4px',
              cursor: 'pointer',
            }}
          >
            Retry
          </button>
        )}
        <button
          onClick={() => history.push('/c/airunway/settings')}
          style={{
            padding: '10px 20px',
            backgroundColor: 'transparent',
            color: 'inherit',
            border: '1px solid rgba(128, 128, 128, 0.3)',
            borderRadius: '4px',
            cursor: 'pointer',
          }}
        >
          Open Settings
        </button>
      </div>
    </div>
  );
}
