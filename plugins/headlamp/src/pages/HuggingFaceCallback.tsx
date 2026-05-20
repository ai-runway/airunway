/**
 * HuggingFace OAuth Callback Page
 *
 * Handles the OAuth callback from HuggingFace, exchanges the authorization code
 * for an access token, and saves it to Kubernetes secrets.
 */

import { useEffect, useState, useRef, useCallback } from 'react';
import { useLocation, useHistory } from 'react-router-dom';
import {
  SectionBox,
  StatusLabel,
} from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import { useApiClient } from '../lib/api-client';
import { ROUTES } from '../routes';

type CallbackStatus = 'processing' | 'success' | 'error';

/**
 * Inline loading spinner
 */
function Spinner() {
  return (
    <span
      style={{
        display: 'inline-block',
        width: '20px',
        height: '20px',
        border: '2px solid currentColor',
        borderTopColor: 'transparent',
        borderRadius: '50%',
        animation: 'spin 1s linear infinite',
      }}
    />
  );
}

// Inject keyframes for spinner animation
const styleId = 'airunway-spinner-keyframes';
if (typeof document !== 'undefined' && !document.getElementById(styleId)) {
  const style = document.createElement('style');
  style.id = styleId;
  style.textContent = `
    @keyframes spin {
      to { transform: rotate(360deg); }
    }
  `;
  document.head.appendChild(style);
}

export function HuggingFaceCallback() {
  const api = useApiClient();
  const location = useLocation();
  const history = useHistory();

  const [status, setStatus] = useState<CallbackStatus>('processing');
  const [errorMessage, setErrorMessage] = useState<string>('');
  const [username, setUsername] = useState<string>('');

  // Prevent duplicate processing (React Strict Mode runs effects twice)
  const processedRef = useRef(false);

  const clearOAuthSession = useCallback(() => {
    sessionStorage.removeItem('hf_oauth_state');
    sessionStorage.removeItem('hf_oauth_from_headlamp');
  }, []);

  useEffect(() => {
    const processCallback = async () => {
      // Skip if already processed
      if (processedRef.current) {
        return;
      }
      processedRef.current = true;

      const searchParams = new URLSearchParams(location.search);

      // Check for error from HuggingFace
      const error = searchParams.get('error');
      const errorDescription = searchParams.get('error_description');
      if (error) {
        setStatus('error');
        setErrorMessage(errorDescription || error);
        clearOAuthSession();
        return;
      }

      // Get authorization code and state
      const code = searchParams.get('code');
      const state = searchParams.get('state');

      if (!code) {
        setStatus('error');
        setErrorMessage('No authorization code received from HuggingFace');
        clearOAuthSession();
        return;
      }

      // Validate state to prevent CSRF attacks
      const storedState = sessionStorage.getItem('hf_oauth_state');
      if (!state || state !== storedState) {
        setStatus('error');
        setErrorMessage('Invalid state parameter. This may be a security issue. Please try again.');
        clearOAuthSession();
        return;
      }

      try {
        // Exchange the authorization code for an access token. The PKCE
        // verifier is held server-side and looked up by `state`; it never
        // travels through the browser.
        console.log('[HF OAuth] Exchanging code for token (server-side verifier)...');
        const tokenResponse = await api.huggingFace.exchangeTokenWithState({
          code,
          state,
        });
        console.log('[HF OAuth] Token exchange successful, user:', tokenResponse.user.name);

        setUsername(tokenResponse.user.name);

        // Save access token to localStorage for frontend use (model searches, etc.)
        try {
          localStorage.setItem('hf_access_token', tokenResponse.accessToken);
          console.log('[HF OAuth] Saved token to localStorage');
        } catch (e) {
          console.warn('[HF OAuth] Failed to save token to localStorage:', e);
        }

        // Save token to K8s secrets
        console.log('[HF OAuth] Saving token to K8s secrets...');
        const saveResult = await api.huggingFace.saveSecret({
          accessToken: tokenResponse.accessToken,
        });
        console.log('[HF OAuth] Save result:', saveResult);

        setStatus('success');
        clearOAuthSession();

        // Redirect to integrations after a short delay
        setTimeout(() => {
          history.replace(ROUTES.INTEGRATIONS);
        }, 2000);
      } catch (err) {
        console.error('[HF OAuth] Error:', err);
        setStatus('error');
        setErrorMessage(err instanceof Error ? err.message : 'Failed to complete OAuth flow');
        clearOAuthSession();
      }
    };

    processCallback();
  }, [location.search, api, history, clearOAuthSession]);

  const handleRetry = () => {
    history.replace(ROUTES.INTEGRATIONS);
  };

  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        minHeight: '60vh',
        padding: '24px',
      }}
    >
      <SectionBox
        title={
          <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
            {status === 'processing' && (
              <>
                <Spinner />
                Connecting to HuggingFace
              </>
            )}
            {status === 'success' && (
              <>
                <span style={{ color: '#4caf50', fontSize: '24px' }}>✓</span>
                Connected Successfully
              </>
            )}
            {status === 'error' && (
              <>
                <span style={{ color: '#f44336', fontSize: '24px' }}>✕</span>
                Connection Failed
              </>
            )}
          </div>
        }
      >
        <div style={{ padding: '24px', minWidth: '400px', textAlign: 'center' }}>
          {status === 'processing' && (
            <div style={{ display: 'flex', flexDirection: 'column', gap: '16px' }}>
              <p style={{ margin: 0, opacity: 0.7 }}>
                Please wait while we complete the authentication...
              </p>
              <p style={{ margin: 0, fontSize: '14px', opacity: 0.5 }}>
                Exchanging authorization code and saving your HuggingFace token...
              </p>
            </div>
          )}

          {status === 'success' && (
            <div style={{ display: 'flex', flexDirection: 'column', gap: '16px' }}>
              <StatusLabel status="success">Connected as {username}</StatusLabel>
              <p
                style={{
                  margin: 0,
                  color: '#4caf50',
                  padding: '12px',
                  backgroundColor: 'rgba(76, 175, 80, 0.1)',
                  borderRadius: '6px',
                }}
              >
                Your HuggingFace token has been securely saved to your Kubernetes cluster.
                You can now deploy models that require HuggingFace authentication.
              </p>
              <p style={{ margin: 0, fontSize: '14px', opacity: 0.7 }}>
                Redirecting to Integrations...
              </p>
            </div>
          )}

          {status === 'error' && (
            <div style={{ display: 'flex', flexDirection: 'column', gap: '16px' }}>
              <div
                style={{
                  padding: '12px',
                  backgroundColor: 'rgba(244, 67, 54, 0.1)',
                  borderRadius: '6px',
                  color: '#f44336',
                }}
              >
                {errorMessage}
              </div>
              <button
                onClick={handleRetry}
                style={{
                  padding: '10px 20px',
                  backgroundColor: 'transparent',
                  border: '1px solid rgba(128, 128, 128, 0.3)',
                  borderRadius: '4px',
                  cursor: 'pointer',
                  color: 'inherit',
                }}
              >
                Back to Integrations
              </button>
            </div>
          )}
        </div>
      </SectionBox>
    </div>
  );
}
