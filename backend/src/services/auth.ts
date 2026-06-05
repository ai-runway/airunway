import * as k8s from '@kubernetes/client-node';
import * as os from 'os';
import * as path from 'path';
import * as fs from 'fs';
import { loadKubeConfig } from '../lib/kubeconfig';
import logger from '../lib/logger';

/**
 * User information extracted from token validation
 */
export interface UserInfo {
  username: string;
  uid?: string;
  groups?: string[];
}

/**
 * Result of token validation
 */
export interface TokenValidationResult {
  valid: boolean;
  user?: UserInfo;
  error?: string;
}

/**
 * Credentials stored locally for CLI login
 */
export interface StoredCredentials {
  token: string;
  username: string;
  expiresAt?: string;
}

const CREDENTIALS_DIR = path.join(os.homedir(), '.airunway');
const CREDENTIALS_FILE = path.join(CREDENTIALS_DIR, 'credentials.json');

/**
 * Auth Service
 * Handles token validation via Kubernetes TokenReview API
 * and local credential management for CLI login
 */
class AuthService {
  private kc: k8s.KubeConfig;
  private authApi: k8s.AuthenticationV1Api;

  constructor() {
    this.kc = loadKubeConfig();
    this.authApi = this.kc.makeApiClient(k8s.AuthenticationV1Api);
  }

  /**
   * Check if authentication is enabled via environment variable
   */
  isAuthEnabled(): boolean {
    const authEnabled = process.env.AUTH_ENABLED?.toLowerCase();
    return authEnabled === 'true' || authEnabled === '1';
  }

  /**
   * Validate a bearer token using Kubernetes TokenReview API
   * This delegates trust to the Kubernetes cluster
   */
  async validateToken(token: string): Promise<TokenValidationResult> {
    if (!token) {
      return { valid: false, error: 'No token provided' };
    }

    try {
      const tokenReview: k8s.V1TokenReview = {
        apiVersion: 'authentication.k8s.io/v1',
        kind: 'TokenReview',
        spec: {
          token: token,
        },
      };

      const response = await this.authApi.createTokenReview({ body: tokenReview });
      const status = response.status;

      if (status?.authenticated) {
        return {
          valid: true,
          user: {
            username: status.user?.username || 'unknown',
            uid: status.user?.uid,
            groups: status.user?.groups,
          },
        };
      } else {
        return {
          valid: false,
          error: status?.error || 'Token not authenticated',
        };
      }
    } catch (error) {
      logger.error({ error }, 'Error validating token via TokenReview');
      return {
        valid: false,
        error: error instanceof Error ? error.message : 'Token validation failed',
      };
    }
  }

  /**
   * Extract OIDC token from current kubeconfig context
   * Used by CLI login command
   */
  async extractTokenFromKubeconfig(contextName?: string): Promise<{
    token: string;
    username: string;
    expiresAt?: string;
  } | null> {
    try {
      const kc = new k8s.KubeConfig();
      kc.loadFromDefault();

      // Use specified context or current context
      const targetContext = contextName || kc.getCurrentContext();
      const context = kc.getContexts().find(c => c.name === targetContext);
      
      if (!context) {
        logger.error({ contextName: targetContext }, 'Context not found in kubeconfig');
        return null;
      }

      const user = kc.getUsers().find(u => u.name === context.user);
      if (!user) {
        logger.error({ userName: context.user }, 'User not found in kubeconfig');
        return null;
      }

      // Check for OIDC auth provider (used by AKS, GKE, etc.)
      const authProvider = user.authProvider;
      if (authProvider?.config) {
        const idToken = authProvider.config['id-token'];
        if (idToken) {
          // Try to extract username from token
          const username = this.extractUsernameFromToken(idToken) || user.name;
          const expiresAt = this.extractExpiryFromToken(idToken);
          
          return {
            token: idToken,
            username,
            expiresAt,
          };
        }
      }

      // Check for exec-based auth (used by newer kubeconfigs)
      if (user.exec) {
        logger.info('Kubeconfig uses exec-based auth. Running kubectl to refresh token...');
        
        // Run a simple kubectl command to trigger token refresh
        const result = await this.runKubectl(['config', 'view', '--minify', '--raw', '-o', 'jsonpath={.users[0].user.token}']);
        if (result && result.trim()) {
          const username = this.extractUsernameFromToken(result) || user.name;
          return {
            token: result.trim(),
            username,
          };
        }

        // Try to get token from auth-provider after kubectl refresh
        kc.loadFromDefault(); // Reload to get refreshed token
        const refreshedUser = kc.getUsers().find(u => u.name === context.user);
        if (refreshedUser?.authProvider?.config?.['id-token']) {
          const idToken = refreshedUser.authProvider.config['id-token'];
          const username = this.extractUsernameFromToken(idToken) || user.name;
          return {
            token: idToken,
            username,
          };
        }
      }

      // Check for direct token
      if (user.token) {
        const username = this.extractUsernameFromToken(user.token) || user.name;
        return {
          token: user.token,
          username,
        };
      }

      logger.error('No supported authentication method found in kubeconfig');
      return null;
    } catch (error) {
      logger.error({ error }, 'Error extracting token from kubeconfig');
      return null;
    }
  }

  /**
   * Run kubectl command and return output
   */
  private async runKubectl(args: string[]): Promise<string | null> {
    try {
      const proc = Bun.spawn(['kubectl', ...args], {
        stdout: 'pipe',
        stderr: 'pipe',
      });
      
      const output = await new Response(proc.stdout).text();
      const exitCode = await proc.exited;
      
      if (exitCode !== 0) {
        return null;
      }
      
      return output;
    } catch {
      return null;
    }
  }

  /**
   * Extract username from JWT token (without validation)
   */
  private extractUsernameFromToken(token: string): string | null {
    try {
      const parts = token.split('.');
      if (parts.length !== 3) return null;
      
      const payload = JSON.parse(Buffer.from(parts[1], 'base64').toString());
      return payload.email || payload.preferred_username || payload.sub || null;
    } catch {
      return null;
    }
  }

  /**
   * Extract expiry from JWT token
   */
  private extractExpiryFromToken(token: string): string | undefined {
    try {
      const parts = token.split('.');
      if (parts.length !== 3) return undefined;
      
      const payload = JSON.parse(Buffer.from(parts[1], 'base64').toString());
      if (payload.exp) {
        return new Date(payload.exp * 1000).toISOString();
      }
      return undefined;
    } catch {
      return undefined;
    }
  }

  /**
   * Save credentials to local file
   */
  saveCredentials(credentials: StoredCredentials): void {
    try {
      if (!fs.existsSync(CREDENTIALS_DIR)) {
        fs.mkdirSync(CREDENTIALS_DIR, { recursive: true, mode: 0o700 });
      }
      
      fs.writeFileSync(
        CREDENTIALS_FILE,
        JSON.stringify(credentials, null, 2),
        { mode: 0o600 }
      );
      
      logger.debug({ path: CREDENTIALS_FILE }, 'Credentials saved');
    } catch (error) {
      logger.error({ error }, 'Error saving credentials');
      throw new Error('Failed to save credentials');
    }
  }

  /**
   * Load credentials from local file
   */
  loadCredentials(): StoredCredentials | null {
    try {
      if (!fs.existsSync(CREDENTIALS_FILE)) {
        return null;
      }
      
      const data = fs.readFileSync(CREDENTIALS_FILE, 'utf-8');
      return JSON.parse(data) as StoredCredentials;
    } catch (error) {
      logger.error({ error }, 'Error loading credentials');
      return null;
    }
  }

  /**
   * Clear stored credentials
   */
  clearCredentials(): void {
    try {
      if (fs.existsSync(CREDENTIALS_FILE)) {
        fs.unlinkSync(CREDENTIALS_FILE);
      }
    } catch (error) {
      logger.error({ error }, 'Error clearing credentials');
    }
  }

  /**
   * Generate a login URL with token in fragment
   */
  generateLoginUrl(serverUrl: string, token: string): string {
    // Use URL fragment so token isn't sent to server in GET request
    const baseUrl = serverUrl.replace(/\/$/, '');
    return `${baseUrl}/login#token=${encodeURIComponent(token)}`;
  }
}

// Export singleton instance
export const authService = new AuthService();
