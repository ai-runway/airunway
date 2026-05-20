/**
 * Utility functions for the Headlamp plugin
 */

import type { StatusLabelProps } from '@kinvolk/headlamp-plugin/lib/CommonComponents';
import type { DeploymentPhase } from '@airunway/shared';

/** Map a DeploymentPhase to a Headlamp StatusLabel color */
export function getDeploymentPhaseColor(phase: DeploymentPhase | string): StatusLabelProps['status'] {
  switch (phase) {
    case 'Running':
      return 'success';
    case 'Pending':
    case 'Deploying':
      return 'warning';
    case 'Failed':
    case 'Terminating':
      return 'error';
    default:
      return '';
  }
}

/**
 * Ayna deep link configuration (unified flow)
 * URL Pattern: ayna://chat?model={model}&prompt={message}&system={system}&provider={provider}&endpoint={url}&key={apikey}&type={type}
 */
export interface AynaOptions {
  // Chat parameters
  model?: string;
  prompt?: string;
  system?: string;
  // Model setup parameters
  provider?: 'openai' | 'azure' | 'github' | 'aikit';
  endpoint?: string;
  key?: string;
  type?: 'chat' | 'responses' | 'image';
}

/**
 * Copy text to clipboard with error handling.
 * Returns true if copy succeeded, false otherwise.
 */
export async function copyToClipboard(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Fallback for environments that block the Clipboard API (iframes, non-HTTPS)
    try {
      const textarea = document.createElement('textarea');
      textarea.value = text;
      textarea.style.position = 'fixed';
      textarea.style.opacity = '0';
      document.body.appendChild(textarea);
      textarea.select();
      document.execCommand('copy');
      document.body.removeChild(textarea);
      return true;
    } catch {
      return false;
    }
  }
}

/** Format a date string as a human-readable relative time (e.g. "3 days ago") */
export function formatRelativeTime(dateStr: string): string {
  const date = new Date(dateStr);
  const now = new Date();
  const diffMs = now.getTime() - date.getTime();
  const diffMins = Math.floor(diffMs / 60000);
  const diffHours = Math.floor(diffMins / 60);
  const diffDays = Math.floor(diffHours / 24);

  if (diffDays > 0) return `${diffDays} day${diffDays > 1 ? 's' : ''} ago`;
  if (diffHours > 0) return `${diffHours} hour${diffHours > 1 ? 's' : ''} ago`;
  if (diffMins > 0) return `${diffMins} minute${diffMins > 1 ? 's' : ''} ago`;
  return 'just now';
}

/** Format a date string as a short age string (e.g. "3d", "5h", "12m") */
export function formatAge(dateStr: string): string {
  const date = new Date(dateStr);
  const now = new Date();
  const diffMs = now.getTime() - date.getTime();
  const diffMins = Math.floor(diffMs / 60000);
  const diffHours = Math.floor(diffMins / 60);
  const diffDays = Math.floor(diffHours / 24);

  if (diffDays > 0) return `${diffDays}d`;
  if (diffHours > 0) return `${diffHours}h`;
  return `${diffMins}m`;
}

/**
 * Generate an Ayna deep link URL (unified flow for chat + model setup)
 * URL Pattern: ayna://chat?model={model}&prompt={message}&system={system}&provider={provider}&endpoint={url}&key={apikey}&type={type}
 */
export function generateAynaUrl(options: AynaOptions = {}): string {
  const params = new URLSearchParams();
  if (options.model) params.set('model', options.model);
  if (options.prompt) params.set('prompt', options.prompt);
  if (options.system) params.set('system', options.system);
  if (options.provider) params.set('provider', options.provider);
  if (options.endpoint) params.set('endpoint', options.endpoint);
  if (options.key) params.set('key', options.key);
  if (options.type) params.set('type', options.type);

  const queryString = params.toString();
  return `ayna://chat${queryString ? `?${queryString}` : ''}`;
}
