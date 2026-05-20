export interface EmbeddedAsset {
  path: string;
  contentType: string;
}

export const EMBEDDED_ASSETS: Record<string, EmbeddedAsset>;
