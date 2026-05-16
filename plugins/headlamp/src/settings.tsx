/**
 * Plugin Settings Component
 *
 * Allows users to configure the AI Runway backend URL and other settings.
 */

import { useState, useEffect, useCallback } from 'react';
import Button from '@mui/material/Button';
import { Icon } from '@iconify/react';
import {
  getBackendUrlFromSettings,
  setBackendUrl,
  getBackendNamespace,
  setBackendNamespace,
  clearBackendCache,
  getBackendConfig,
  type BackendConfig,
} from './lib/backend-discovery';
import { resetApiClient, api } from './lib/api-client';

export function PluginSettings() {
  const [backendUrl, setBackendUrlState] = useState('');
  const [namespace, setNamespaceState] = useState('');
  const [backendConfig, setBackendConfigState] = useState<BackendConfig | null>(null);
  const [connectionStatus, setConnectionStatus] = useState<'unknown' | 'checking' | 'connected' | 'error'>('unknown');
  const [errorMessage, setErrorMessage] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  // Load current settings
  useEffect(() => {
    setBackendUrlState(getBackendUrlFromSettings() || '');
    setNamespaceState(getBackendNamespace());

    // Get current backend config
    getBackendConfig().then(setBackendConfigState);
  }, []);

  // Check connection
  const checkConnection = useCallback(async () => {
    setConnectionStatus('checking');
    setErrorMessage(null);

    try {
      const result = await api.health.check();
      if (result.status === 'healthy' || result.status === 'ok') {
        setConnectionStatus('connected');
      } else {
        setConnectionStatus('error');
        setErrorMessage(`Backend returned unexpected status: ${result.status}`);
      }
    } catch (err) {
      setConnectionStatus('error');
      setErrorMessage(err instanceof Error ? err.message : 'Connection failed');
    }
  }, []);

  // Save settings
  const handleSave = useCallback(() => {
    if (backendUrl.trim()) {
      setBackendUrl(backendUrl.trim());
    }
    setBackendNamespace(namespace || 'airunway-system');

    // Reset cached values
    clearBackendCache();
    resetApiClient();

    // Update config display
    getBackendConfig().then(setBackendConfigState);

    setSaved(true);
    setTimeout(() => setSaved(false), 3000);

    // Check connection with new settings
    setTimeout(checkConnection, 100);
  }, [backendUrl, namespace, checkConnection]);

  return (
    <div style={{ padding: '24px', maxWidth: '600px' }}>
      <h1 style={{ marginBottom: '24px' }}>AI Runway Settings</h1>

      {/* Backend URL Section */}
      <section style={{ marginBottom: '32px' }}>
        <h2 style={{ fontSize: '18px', marginBottom: '16px' }}>Backend Configuration</h2>

        <div style={{ marginBottom: '16px' }}>
          <label style={{ display: 'block', marginBottom: '8px', fontWeight: 500 }}>
            Backend URL
          </label>
          <input
            type="text"
            value={backendUrl}
            onChange={(e) => setBackendUrlState(e.target.value)}
            placeholder="http://localhost:3001 (auto-detected if empty)"
            style={{
              width: '100%',
              padding: '8px 12px',
              border: '1px solid rgba(128, 128, 128, 0.3)',
              borderRadius: '4px',
              fontSize: '14px',
              backgroundColor: 'transparent',
              color: 'inherit',
            }}
          />
          <p style={{ fontSize: '12px', opacity: 0.6, marginTop: '4px' }}>
            Leave empty for automatic discovery. The plugin will try in-cluster service discovery first.
          </p>
        </div>

        <div style={{ marginBottom: '16px' }}>
          <label style={{ display: 'block', marginBottom: '8px', fontWeight: 500 }}>
            Backend Namespace
          </label>
          <input
            type="text"
            value={namespace}
            onChange={(e) => setNamespaceState(e.target.value)}
            placeholder="airunway-system"
            style={{
              width: '100%',
              padding: '8px 12px',
              border: '1px solid rgba(128, 128, 128, 0.3)',
              borderRadius: '4px',
              fontSize: '14px',
              backgroundColor: 'transparent',
              color: 'inherit',
            }}
          />
          <p style={{ fontSize: '12px', opacity: 0.6, marginTop: '4px' }}>
            Namespace where AI Runway backend is deployed (for service discovery).
          </p>
        </div>

        <div style={{ display: 'flex', gap: '8px', marginBottom: '16px' }}>
          <Button
            variant="contained"
            color="primary"
            size="small"
            startIcon={<Icon icon="mdi:content-save" />}
            onClick={handleSave}
          >
            Save Settings
          </Button>
          <Button
            variant="contained"
            color="primary"
            size="small"
            startIcon={<Icon icon="mdi:access-point-network" />}
            onClick={checkConnection}
            disabled={connectionStatus === 'checking'}
          >
            Test Connection
          </Button>
        </div>

        {saved && (
          <div style={{ padding: '8px 12px', backgroundColor: 'rgba(46, 125, 50, 0.15)', color: '#4caf50', borderRadius: '4px', marginBottom: '16px' }}>
            Settings saved!
          </div>
        )}
      </section>

      {/* Connection Status Section */}
      <section style={{ marginBottom: '32px' }}>
        <h2 style={{ fontSize: '18px', marginBottom: '16px' }}>Connection Status</h2>

        <div style={{ padding: '16px', backgroundColor: 'rgba(128, 128, 128, 0.1)', borderRadius: '8px' }}>
          {backendConfig && (
            <div style={{ marginBottom: '12px' }}>
              <strong>Current URL:</strong> {backendConfig.url}
              <span
                style={{
                  marginLeft: '8px',
                  padding: '2px 8px',
                  backgroundColor: backendConfig.source === 'settings' ? 'rgba(25, 118, 210, 0.15)' : 'rgba(156, 39, 176, 0.15)',
                  color: backendConfig.source === 'settings' ? '#2196f3' : '#ab47bc',
                  borderRadius: '4px',
                  fontSize: '12px',
                }}
              >
                {backendConfig.source === 'settings' && 'From Settings'}
                {backendConfig.source === 'service-discovery' && 'Auto-discovered'}
                {backendConfig.source === 'default' && 'Default'}
              </span>
            </div>
          )}

          <div>
            <strong>Status:</strong>{' '}
            <span
              style={{
                padding: '2px 8px',
                borderRadius: '4px',
                backgroundColor:
                  connectionStatus === 'connected'
                    ? 'rgba(46, 125, 50, 0.15)'
                    : connectionStatus === 'error'
                      ? 'rgba(198, 40, 40, 0.15)'
                      : connectionStatus === 'checking'
                        ? 'rgba(255, 152, 0, 0.15)'
                        : 'rgba(128, 128, 128, 0.15)',
                color:
                  connectionStatus === 'connected'
                    ? '#4caf50'
                    : connectionStatus === 'error'
                      ? '#f44336'
                      : connectionStatus === 'checking'
                        ? '#ff9800'
                        : 'inherit',
              }}
            >
              {connectionStatus === 'unknown' && 'Not checked'}
              {connectionStatus === 'checking' && 'Checking...'}
              {connectionStatus === 'connected' && '✓ Connected'}
              {connectionStatus === 'error' && '✗ Error'}
            </span>
          </div>

          {errorMessage && (
            <div style={{ marginTop: '12px', color: '#f44336', fontSize: '14px' }}>
              {errorMessage}
            </div>
          )}
        </div>
      </section>

      {/* Help Section */}
      <section>
        <h2 style={{ fontSize: '18px', marginBottom: '16px' }}>Help</h2>
        <div style={{ fontSize: '14px', opacity: 0.7, lineHeight: '1.6' }}>
          <p>The AI Runway plugin connects to the AI Runway backend to manage deployments.</p>
          <p style={{ marginTop: '8px' }}>
            <strong>Discovery order:</strong>
          </p>
          <ol style={{ margin: '8px 0', paddingLeft: '20px' }}>
            <li>Custom URL from settings (if configured)</li>
            <li>In-cluster service: <code style={{ backgroundColor: 'rgba(128, 128, 128, 0.2)', padding: '2px 6px', borderRadius: '4px' }}>airunway.{'{namespace}'}.svc:3001</code></li>
            <li>Default: <code style={{ backgroundColor: 'rgba(128, 128, 128, 0.2)', padding: '2px 6px', borderRadius: '4px' }}>http://localhost:3001</code></li>
          </ol>
        </div>
      </section>
    </div>
  );
}
