import { Hono } from 'hono';
import { zValidator } from '@hono/zod-validator';
import { z } from 'zod';
import { huggingFaceService } from '../services/huggingface';
import { HTTPException } from 'hono/http-exception';
import logger from '../lib/logger';

const hfTokenExchangeSchema = z.object({
  code: z.string().min(1, 'Authorization code is required'),
  codeVerifier: z.string().min(43, 'Code verifier must be at least 43 characters'),
  redirectUri: z.string().url('Redirect URI must be a valid URL'),
});

const hfStartOAuthSchema = z.object({
  redirectUri: z.string().url('Redirect URI must be a valid URL'),
});

// In-memory storage for PKCE verifiers (keyed by state)
// In production, this should use Redis or similar for multi-instance support
const pkceStore = new Map<string, { verifier: string; redirectUri: string; createdAt: number }>();

// Clean up expired entries every 5 minutes
setInterval(() => {
  const now = Date.now();
  const maxAge = 10 * 60 * 1000; // 10 minutes
  for (const [state, data] of pkceStore.entries()) {
    if (now - data.createdAt > maxAge) {
      pkceStore.delete(state);
    }
  }
}, 5 * 60 * 1000);

/**
 * Generate a cryptographically secure random string for PKCE
 */
function generateCodeVerifier(): string {
  const array = new Uint8Array(32);
  crypto.getRandomValues(array);
  return base64UrlEncode(array);
}

/**
 * Generate code challenge from verifier using SHA-256
 */
async function generateCodeChallenge(verifier: string): Promise<string> {
  const encoder = new TextEncoder();
  const data = encoder.encode(verifier);
  const digest = await crypto.subtle.digest('SHA-256', data);
  return base64UrlEncode(new Uint8Array(digest));
}

function base64UrlEncode(buffer: Uint8Array): string {
  let binary = '';
  for (const byte of buffer) {
    binary += String.fromCharCode(byte);
  }
  const base64 = btoa(binary);
  return base64.replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

const oauth = new Hono()
  .get('/huggingface/config', (c) => {
    // Return OAuth config for frontend to construct auth URL
    return c.json({
      clientId: huggingFaceService.getClientId(),
      authorizeUrl: 'https://huggingface.co/oauth/authorize',
      scopes: ['openid', 'profile', 'read-repos'],
    });
  })
  // Start OAuth flow - generates PKCE and returns authorization URL
  // This allows any frontend (Headlamp, main frontend) to initiate OAuth
  // while the backend stores the PKCE verifier for later token exchange
  .post('/huggingface/start', zValidator('json', hfStartOAuthSchema), async (c) => {
    const { redirectUri } = c.req.valid('json');

    try {
      // Generate PKCE values
      const codeVerifier = generateCodeVerifier();
      const codeChallenge = await generateCodeChallenge(codeVerifier);
      const state = crypto.randomUUID();

      // Store verifier for later retrieval
      pkceStore.set(state, {
        verifier: codeVerifier,
        redirectUri,
        createdAt: Date.now(),
      });

      // Build authorization URL
      const config = {
        clientId: huggingFaceService.getClientId(),
        authorizeUrl: 'https://huggingface.co/oauth/authorize',
        scopes: ['openid', 'profile', 'read-repos'],
      };

      const params = new URLSearchParams({
        client_id: config.clientId,
        redirect_uri: redirectUri,
        scope: config.scopes.join(' '),
        response_type: 'code',
        code_challenge: codeChallenge,
        code_challenge_method: 'S256',
        state,
      });

      const authorizationUrl = `${config.authorizeUrl}?${params.toString()}`;

      logger.debug({ state, redirectUri }, 'Started HuggingFace OAuth flow');

      return c.json({
        authorizationUrl,
        state,
      });
    } catch (error) {
      logger.error({ error }, 'Failed to start HuggingFace OAuth flow');
      throw new HTTPException(500, {
        message: error instanceof Error ? error.message : 'Failed to start OAuth flow',
      });
    }
  })
  // Deprecated: previously returned the stored PKCE verifier so the client could
  // call POST /huggingface/token itself. That exposed the verifier to the
  // browser. Use POST /huggingface/token-with-state instead — the verifier
  // never leaves the server. This handler now always returns 410 Gone.
  .get('/huggingface/verifier/:state', () => {
    // This endpoint is deprecated — use POST /huggingface/token-with-state instead
    throw new HTTPException(410, {
      message: 'This endpoint has been removed for security. Use POST /huggingface/token-with-state instead.',
    });
  })
  .post('/huggingface/token', zValidator('json', hfTokenExchangeSchema), async (c) => {
    const { code, codeVerifier, redirectUri } = c.req.valid('json');

    try {
      const result = await huggingFaceService.handleOAuthCallback(code, codeVerifier, redirectUri);
      return c.json(result);
    } catch (error) {
      logger.error({ error }, 'HuggingFace OAuth token exchange failed');
      throw new HTTPException(400, {
        message: 'OAuth token exchange failed',
      });
    }
  })
  // Server-side token exchange using stored PKCE verifier — verifier never leaves the server
  .post('/huggingface/token-with-state', zValidator('json', z.object({
    code: z.string().min(1),
    state: z.string().uuid(),
  })), async (c) => {
    const { code, state } = c.req.valid('json');

    const data = pkceStore.get(state);
    if (!data) {
      throw new HTTPException(404, { message: 'OAuth session not found or expired' });
    }

    try {
      const result = await huggingFaceService.handleOAuthCallback(code, data.verifier, data.redirectUri);
      pkceStore.delete(state);
      return c.json(result);
    } catch (error) {
      logger.error({ error }, 'HuggingFace OAuth token exchange failed');
      throw new HTTPException(400, { message: 'OAuth token exchange failed' });
    }
  });

export default oauth;
