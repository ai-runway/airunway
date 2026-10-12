import logger from './logger';

/**
 * Kubernetes API error response structure
 */
export interface K8sApiError {
  statusCode?: number;
  response?: {
    statusCode?: number;
    body?: K8sErrorBody | string;
  };
  body?: K8sErrorBody | string;
  message?: string;
  code?: string | number;
}

/**
 * Kubernetes error body structure (Status object)
 */
export interface K8sErrorBody {
  kind?: string;
  apiVersion?: string;
  status?: string;
  message?: string;
  reason?: string;
  details?: {
    name?: string;
    group?: string;
    kind?: string;
    causes?: Array<{
      reason?: string;
      message?: string;
      field?: string;
    }>;
  };
  code?: number;
}

function getDirectK8sStatusCode(error: K8sApiError): number | undefined {
  return error.statusCode
    ?? error.response?.statusCode
    ?? (typeof error.code === 'number' ? error.code : undefined);
}

/**
 * User-friendly error messages for common Kubernetes errors
 */
const ERROR_MESSAGES: Record<string, string> = {
  'Forbidden': 'Permission denied. Check if the user has the required RBAC permissions.',
  'NotFound': 'Resource not found. The CRD or namespace may not exist.',
  'AlreadyExists': 'A deployment with this name already exists.',
  'Invalid': 'Invalid configuration. Check the deployment parameters.',
  'Conflict': 'Resource conflict. The resource was modified by another process.',
  'Unauthorized': 'Unauthorized. Check your cluster credentials.',
  'ServiceUnavailable': 'Kubernetes API server is unavailable. Try again later.',
  'InternalError': 'Kubernetes API server internal error. Try again later.',
};

/**
 * Extract a detailed, user-friendly error message from a Kubernetes API error
 */
export function extractK8sErrorMessage(error: unknown): string {
  if (!error) {
    return 'Unknown error occurred';
  }

  const k8sError = error as K8sApiError;

  // Kubernetes ApiException extends Error, but its generated message embeds
  // the complete response body and headers. Only return an Error message
  // directly when it has no Kubernetes response metadata to normalize.
  if (
    error instanceof Error
    && !k8sError.response
    && !k8sError.body
    && k8sError.statusCode === undefined
    && typeof k8sError.code !== 'number'
  ) {
    return error.message;
  }

  // Try to get the body from various locations
  const rawBody: K8sErrorBody | string | undefined =
    k8sError.body ||
    k8sError.response?.body;

  // Parse JSON body if it's a string
  let parsedBody: K8sErrorBody | undefined;
  if (typeof rawBody === 'string') {
    try {
      parsedBody = JSON.parse(rawBody) as K8sErrorBody;
    } catch {
      // If it's not JSON, use the string as the error message
      return rawBody;
    }
  } else if (rawBody && typeof rawBody === 'object') {
    parsedBody = rawBody;
  }

  // If we have a K8s Status body, extract detailed information
  if (parsedBody) {
    const parts: string[] = [];

    // Get the main message
    if (parsedBody.message) {
      parts.push(parsedBody.message);
    }

    // Add field-specific causes
    if (parsedBody.details?.causes && parsedBody.details.causes.length > 0) {
      const causeMessages = parsedBody.details.causes
        .map((cause: { reason?: string; message?: string; field?: string }) => {
          if (cause.field && cause.message) {
            return `${cause.field}: ${cause.message}`;
          }
          return cause.message || cause.reason;
        })
        .filter(Boolean);
      
      if (causeMessages.length > 0) {
        parts.push(`Details: ${causeMessages.join('; ')}`);
      }
    }

    if (parts.length > 0) {
      return parts.join(' ');
    }

    // Fall back to reason-based message
    if (parsedBody.reason && ERROR_MESSAGES[parsedBody.reason]) {
      return ERROR_MESSAGES[parsedBody.reason];
    }
  }

  // Get status code for more context
  const statusCode = getDirectK8sStatusCode(k8sError);

  if (statusCode) {
    return getStatusCodeMessage(statusCode);
  }

  if (k8sError.message) {
    return k8sError.message;
  }

  return 'Unknown Kubernetes API error';
}

/**
 * Get a human-readable message for an HTTP status code
 */
function getStatusCodeMessage(statusCode: number): string {
  switch (statusCode) {
    case 400:
      return 'Invalid request. Check the deployment configuration.';
    case 401:
      return 'Authentication failed. Check your cluster credentials.';
    case 403:
      return 'Permission denied. Check if you have the required RBAC permissions to create deployments.';
    case 404:
      return 'Resource not found. The CRD or namespace may not exist. Check if the runtime is installed.';
    case 409:
      return 'A deployment with this name already exists.';
    case 422:
      return 'Invalid deployment configuration. Check the parameters and try again.';
    case 500:
      return 'Kubernetes API server error. Try again later.';
    case 502:
    case 503:
    case 504:
      return 'Kubernetes API server is temporarily unavailable. Try again later.';
    default:
      return `Request failed with status ${statusCode}`;
  }
}

/**
 * Get the HTTP status code from a Kubernetes error
 */
export function getK8sErrorStatusCode(error: unknown): number {
  if (!error || typeof error !== 'object') {
    return 500;
  }

  const k8sError = error as K8sApiError;
  const statusCode = getDirectK8sStatusCode(k8sError);
  
  if (statusCode && statusCode >= 400 && statusCode < 600) {
    return statusCode;
  }

  // Check if it's in the body
  const body = k8sError.body || k8sError.response?.body;
  if (body && typeof body === 'object' && 'code' in body && typeof body.code === 'number') {
    return body.code;
  }

  return 500;
}

/**
 * Log detailed K8s error information and return user-friendly message
 */
export function handleK8sError(error: unknown, context: Record<string, unknown> = {}): {
  message: string;
  statusCode: number;
} {
  const message = extractK8sErrorMessage(error);
  const statusCode = getK8sErrorStatusCode(error);

  // Log full error details for debugging
  logger.error(
    {
      ...context,
      errorMessage: message,
      statusCode,
      rawError: error instanceof Error ? { message: error.message, stack: error.stack } : error,
    },
    `Kubernetes API error: ${message}`
  );

  return { message, statusCode };
}
