/**
 * Gateway API
 */

import type { RequestFn } from './client';
import type { GatewayCRDStatus, GatewayCRDInstallResult, GatewayInfo, GatewayModelInfo } from '../types';

interface GatewayModelsResponse {
  models: GatewayModelInfo[];
}

export interface GatewayApi {
  /** Get Gateway CRD installation status */
  getStatus: () => Promise<GatewayCRDStatus>;

  /** Install Gateway API and Inference Extension CRDs */
  installCrds: () => Promise<GatewayCRDInstallResult>;

  /** Get live gateway runtime info (availability, endpoint) */
  getInfo: () => Promise<GatewayInfo>;

  /** Get models currently routed through the gateway */
  getModels: () => Promise<GatewayModelInfo[]>;
}

export function createGatewayApi(request: RequestFn): GatewayApi {
  return {
    getStatus: () => request<GatewayCRDStatus>('/installation/gateway/status'),

    installCrds: () =>
      request<GatewayCRDInstallResult>('/installation/gateway/install-crds', {
        method: 'POST',
      }),

    getInfo: () => request<GatewayInfo>('/gateway/status'),

    getModels: async () => {
      const data = await request<GatewayModelsResponse>('/gateway/models');
      return data.models ?? [];
    },
  };
}
