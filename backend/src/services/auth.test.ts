import { describe, test, expect, afterEach } from 'bun:test';
import { authService } from './auth';

describe('AuthService', () => {
  describe('isAuthEnabled', () => {
    const originalEnv = process.env.AUTH_ENABLED;

    afterEach(() => {
      if (originalEnv !== undefined) {
        process.env.AUTH_ENABLED = originalEnv;
      } else {
        delete process.env.AUTH_ENABLED;
      }
    });

    test('returns false when AUTH_ENABLED is not set', () => {
      delete process.env.AUTH_ENABLED;
      expect(authService.isAuthEnabled()).toBe(false);
    });

    test('returns true when AUTH_ENABLED is "true"', () => {
      process.env.AUTH_ENABLED = 'true';
      expect(authService.isAuthEnabled()).toBe(true);
    });

    test('returns true when AUTH_ENABLED is "1"', () => {
      process.env.AUTH_ENABLED = '1';
      expect(authService.isAuthEnabled()).toBe(true);
    });

    test('returns false when AUTH_ENABLED is "false"', () => {
      process.env.AUTH_ENABLED = 'false';
      expect(authService.isAuthEnabled()).toBe(false);
    });

    test('returns true when AUTH_ENABLED is "TRUE" (case insensitive)', () => {
      process.env.AUTH_ENABLED = 'TRUE';
      expect(authService.isAuthEnabled()).toBe(true);
    });
  });

  describe('generateLoginUrl', () => {
    test('generates URL with token in fragment', () => {
      const url = authService.generateLoginUrl('http://localhost:3001', 'test-token');
      expect(url).toBe('http://localhost:3001/login#token=test-token');
    });

    test('removes trailing slash from server URL', () => {
      const url = authService.generateLoginUrl('http://localhost:3001/', 'test-token');
      expect(url).toBe('http://localhost:3001/login#token=test-token');
    });

    test('encodes special characters in token', () => {
      const tokenWithSpecialChars = 'token+with/special=chars';
      const url = authService.generateLoginUrl('http://localhost:3001', tokenWithSpecialChars);
      expect(url).toContain('token%2Bwith%2Fspecial%3Dchars');
    });
  });

  describe('credential storage', () => {
    const testCredentials = {
      token: 'test-token-123',
      username: 'testuser@example.com',
      expiresAt: new Date(Date.now() + 3600000).toISOString(),
    };

    afterEach(() => {
      // Clean up credentials after each test
      authService.clearCredentials();
    });

    test('saveCredentials and loadCredentials work together', () => {
      authService.saveCredentials(testCredentials);
      const loaded = authService.loadCredentials();
      
      expect(loaded).not.toBeNull();
      expect(loaded?.token).toBe(testCredentials.token);
      expect(loaded?.username).toBe(testCredentials.username);
      expect(loaded?.expiresAt).toBe(testCredentials.expiresAt);
    });

    test('clearCredentials removes stored credentials', () => {
      authService.saveCredentials(testCredentials);
      expect(authService.loadCredentials()).not.toBeNull();
      
      authService.clearCredentials();
      expect(authService.loadCredentials()).toBeNull();
    });

    test('loadCredentials returns null when no credentials stored', () => {
      authService.clearCredentials();
      expect(authService.loadCredentials()).toBeNull();
    });
  });

  describe('validateToken', () => {
    test('returns invalid for empty token', async () => {
      const result = await authService.validateToken('');
      expect(result.valid).toBe(false);
      expect(result.error).toBe('No token provided');
    });

    // Note: Full token validation tests would require a Kubernetes cluster
    // These are integration tests that should be run in a real environment
  });
});
