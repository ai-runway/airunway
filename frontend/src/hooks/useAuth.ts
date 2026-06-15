import { useState, useEffect, useCallback } from 'react';
import { useSettings } from './useSettings';

const AUTH_TOKEN_KEY = 'airunway_auth_token';
const AUTH_USERNAME_KEY = 'airunway_auth_username';

export interface AuthState {
  isAuthenticated: boolean;
  isLoading: boolean;
  username: string | null;
  authEnabled: boolean;
  error: string | null;
}

export interface UseAuthReturn extends AuthState {
  login: (token: string, username?: string) => void;
  logout: () => void;
  getToken: () => string | null;
  checkTokenFromUrl: () => boolean;
}

/**
 * Hook for managing authentication state
 * Handles token storage, URL-based login (magic link), and auth status
 */
export function useAuth(): UseAuthReturn {
  const { data: settings, isLoading: settingsLoading } = useSettings();
  const [state, setState] = useState<AuthState>({
    isAuthenticated: false,
    isLoading: true,
    username: null,
    authEnabled: false,
    error: null,
  });

  /**
   * Get stored token from localStorage
   */
  const getToken = useCallback((): string | null => {
    try {
      return localStorage.getItem(AUTH_TOKEN_KEY);
    } catch {
      return null;
    }
  }, []);

  /**
   * Login with a token
   */
  const login = useCallback((token: string, username?: string) => {
    try {
      localStorage.setItem(AUTH_TOKEN_KEY, token);
      if (username) {
        localStorage.setItem(AUTH_USERNAME_KEY, username);
      }
      
      // Extract username from token if not provided
      const extractedUsername = username || extractUsernameFromToken(token);
      
      setState(prev => ({
        ...prev,
        isAuthenticated: true,
        username: extractedUsername,
        error: null,
      }));
    } catch {
      setState(prev => ({
        ...prev,
        error: 'Failed to save authentication token',
      }));
    }
  }, []);

  /**
   * Logout - clear stored credentials
   */
  const logout = useCallback(() => {
    try {
      localStorage.removeItem(AUTH_TOKEN_KEY);
      localStorage.removeItem(AUTH_USERNAME_KEY);
      setState(prev => ({
        ...prev,
        isAuthenticated: false,
        username: null,
      }));
    } catch {
      // Ignore errors when clearing
    }
  }, []);

  /**
   * Check URL hash for token (magic link login)
   * Returns true if token was found and processed
   */
  const checkTokenFromUrl = useCallback((): boolean => {
    try {
      const hash = window.location.hash;
      if (!hash || !hash.includes('token=')) {
        return false;
      }

      // Parse token from URL fragment
      const params = new URLSearchParams(hash.slice(1)); // Remove leading #
      const token = params.get('token');
      
      if (token) {
        // Clear the URL hash to avoid exposing token
        window.history.replaceState(null, '', window.location.pathname + window.location.search);
        
        // Login with the token
        login(decodeURIComponent(token));
        return true;
      }
    } catch (error) {
      console.error('Error parsing token from URL:', error);
    }
    return false;
  }, [login]);

  /**
   * Initialize auth state
   */
  useEffect(() => {
    if (settingsLoading) {
      return;
    }

    const authEnabled = settings?.auth?.enabled ?? false;
    
    // First check for token in URL (magic link)
    const tokenFromUrl = checkTokenFromUrl();
    
    if (tokenFromUrl) {
      setState(prev => ({
        ...prev,
        isLoading: false,
        authEnabled,
      }));
      return;
    }

    // Check for existing token in storage
    const storedToken = getToken();
    const storedUsername = localStorage.getItem(AUTH_USERNAME_KEY);

    if (storedToken) {
      // If auth is enabled, we have a token, consider authenticated
      // Token validation happens on API calls (401 will trigger re-auth)
      setState({
        isAuthenticated: true,
        isLoading: false,
        username: storedUsername || extractUsernameFromToken(storedToken),
        authEnabled,
        error: null,
      });
    } else {
      setState({
        isAuthenticated: false,
        isLoading: false,
        username: null,
        authEnabled,
        error: null,
      });
    }
  }, [settings, settingsLoading, checkTokenFromUrl, getToken]);

  /**
   * Listen for auth:unauthorized events (401 responses)
   */
  useEffect(() => {
    const handleUnauthorized = () => {
      logout();
      setState(prev => ({
        ...prev,
        error: 'Session expired. Please login again.',
      }));
    };

    window.addEventListener('auth:unauthorized', handleUnauthorized);
    return () => {
      window.removeEventListener('auth:unauthorized', handleUnauthorized);
    };
  }, [logout]);

  return {
    ...state,
    login,
    logout,
    getToken,
    checkTokenFromUrl,
  };
}

/**
 * Extract username from JWT token payload
 */
function extractUsernameFromToken(token: string): string | null {
  try {
    const parts = token.split('.');
    if (parts.length !== 3) return null;
    
    const payload = JSON.parse(atob(parts[1]));
    return payload.email || payload.preferred_username || payload.sub || null;
  } catch {
    return null;
  }
}
