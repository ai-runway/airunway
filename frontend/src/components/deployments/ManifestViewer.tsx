import { useState, useEffect } from 'react';
import { Button } from '@/components/ui/button';
import { Badge } from '@/components/ui/badge';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { deploymentsApi, type DeploymentConfig } from '@/lib/api';
import { useDeploymentManifest } from '@/hooks/useDeployments';
import { Loader2, Copy, Code, FileJson, ChevronDown, ChevronRight } from 'lucide-react';
import { useToast } from '@/hooks/useToast';
import YAML from 'yaml';
import { Prism as SyntaxHighlighter } from 'react-syntax-highlighter';
import { oneDark } from 'react-syntax-highlighter/dist/esm/styles/prism';

/**
 * Resource info type shared between preview and deployed modes
 */
interface ResourceInfo {
  kind: string;
  apiVersion: string;
  name: string;
  manifest: Record<string, unknown>;
}

/**
 * Props for preview mode (before deployment)
 */
interface ManifestViewerPreviewProps {
  mode: 'preview';
  config: DeploymentConfig;
  provider: string;
}

/**
 * Props for deployed mode (after deployment)
 */
interface ManifestViewerDeployedProps {
  mode: 'deployed';
  deploymentName: string;
  namespace: string;
  provider: string;
}

type ManifestViewerProps = ManifestViewerPreviewProps | ManifestViewerDeployedProps;

/**
 * Get badge styling based on resource kind
 */
function getResourceBadgeStyle(kind: string): string {
  switch (kind.toLowerCase()) {
    case 'inferenceset':
      return 'bg-purple-500/10 text-purple-400 border border-purple-500/20';
    case 'rayservice':
      return 'bg-blue-500/10 text-blue-400 border border-blue-500/20';
    case 'dynamographdeployment':
      return 'bg-emerald-500/10 text-emerald-400 border border-emerald-500/20';
    case 'deployment':
      return 'bg-orange-500/10 text-orange-400 border border-orange-500/20';
    case 'service':
      return 'bg-orange-500/10 text-orange-400 border border-orange-500/20';
    case 'configmap':
      return 'bg-cyan-500/10 text-cyan-400 border border-cyan-500/20';
    case 'secret':
      return 'bg-red-500/10 text-red-400 border border-red-500/20';
    default:
      return 'bg-gray-500/10 text-gray-400 border border-gray-500/20';
  }
}

/**
 * Collapsible JSON/YAML tree node component
 */
interface TreeNodeProps {
  name: string;
  value: unknown;
  depth?: number;
  defaultExpanded?: boolean;
}

function TreeNode({ name, value, depth = 0, defaultExpanded = false }: TreeNodeProps) {
  const [expanded, setExpanded] = useState(defaultExpanded || depth < 2);

  const isObject = value !== null && typeof value === 'object';
  const isArray = Array.isArray(value);
  const entries = isObject ? Object.entries(value as Record<string, unknown>) : [];

  const indent = depth * 16;

  if (!isObject) {
    return (
      <div className="flex items-start py-0.5" style={{ paddingLeft: `${indent}px` }}>
        <span className="text-blue-600 dark:text-blue-400 font-medium mr-2">{name}:</span>
        <span className={
          typeof value === 'string'
            ? 'text-green-600 dark:text-green-400'
            : typeof value === 'number'
            ? 'text-orange-600 dark:text-orange-400'
            : typeof value === 'boolean'
            ? 'text-purple-600 dark:text-purple-400'
            : 'text-muted-foreground'
        }>
          {value === null ? 'null' : String(value)}
        </span>
      </div>
    );
  }

  return (
    <div>
      <div
        className="flex items-center py-0.5 cursor-pointer hover:bg-muted/50 rounded"
        style={{ paddingLeft: `${indent}px` }}
        onClick={() => setExpanded(!expanded)}
      >
        {expanded ? (
          <ChevronDown className="h-3 w-3 mr-1 text-muted-foreground" />
        ) : (
          <ChevronRight className="h-3 w-3 mr-1 text-muted-foreground" />
        )}
        <span className="text-blue-600 dark:text-blue-400 font-medium">{name}</span>
        <span className="text-muted-foreground ml-1 text-xs">
          {isArray ? `[${entries.length}]` : `{${entries.length}}`}
        </span>
      </div>
      {expanded && entries.map(([key, val]) => (
        <TreeNode
          key={key}
          name={key}
          value={val}
          depth={depth + 1}
          defaultExpanded={depth < 1}
        />
      ))}
    </div>
  );
}

/**
 * Unified manifest viewer component
 *
 * Works in two modes:
 * - "preview": Shows manifests that will be created (in DeploymentForm)
 * - "deployed": Shows manifests from deployed resources (in DeploymentDetailsPage)
 */
export function ManifestViewer(props: ManifestViewerProps) {
  const { toast } = useToast();
  const [viewMode, setViewMode] = useState<'yaml' | 'json' | 'tree'>('yaml');
  const [isExpanded, setIsExpanded] = useState(props.mode === 'deployed'); // Deployed mode starts expanded
  const [resources, setResources] = useState<ResourceInfo[]>([]);
  const [selectedResourceIndex, setSelectedResourceIndex] = useState(0);
  const [isPreviewLoading, setIsPreviewLoading] = useState(false);
  const [previewError, setPreviewError] = useState<string | null>(null);

  // For deployed mode, use the hook to fetch resources
const deployedQuery = useDeploymentManifest(
    props.mode === 'deployed' ? (props as ManifestViewerDeployedProps).deploymentName : undefined,
    props.mode === 'deployed' ? (props as ManifestViewerDeployedProps).namespace : undefined,
  );

  // Handle preview mode fetching
  useEffect(() => {
    if (props.mode !== 'preview' || !isExpanded) return;

    const fetchPreview = async () => {
      setIsPreviewLoading(true);
      setPreviewError(null);

      try {
        const result = await deploymentsApi.preview(props.config);
        setResources(result.resources);
        setSelectedResourceIndex(0);
      } catch (err) {
        setPreviewError(err instanceof Error ? err.message : 'Failed to generate preview');
        setResources([]);
      } finally {
        setIsPreviewLoading(false);
      }
    };

    fetchPreview();
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [props.mode, isExpanded, props.mode === 'preview' ? (props as ManifestViewerPreviewProps).config : undefined]);

  // Handle deployed mode data
  useEffect(() => {
    if (props.mode !== 'deployed' || !deployedQuery?.data) return;

    setResources(deployedQuery.data.resources);
    setSelectedResourceIndex(0);
  }, [props.mode, deployedQuery?.data]);

  const isLoading = props.mode === 'preview' ? isPreviewLoading : deployedQuery?.isLoading;
  const error = props.mode === 'preview' ? previewError : (deployedQuery?.error instanceof Error ? deployedQuery.error.message : null);
  const selectedResource = resources[selectedResourceIndex];

  const handleCopy = () => {
    if (!selectedResource) return;

    const content = viewMode === 'json'
      ? JSON.stringify(selectedResource.manifest, null, 2)
      : YAML.stringify(selectedResource.manifest, { indent: 2 });

    navigator.clipboard.writeText(content);
    toast({
      title: 'Copied to clipboard',
      description: `${selectedResource.kind} manifest copied as ${viewMode.toUpperCase()}`,
    });
  };

  const handleCopyAll = () => {
    if (resources.length === 0) return;

    const content = viewMode === 'json'
      ? JSON.stringify(resources.map(r => r.manifest), null, 2)
      : resources.map(r => YAML.stringify(r.manifest, { indent: 2 })).join('---\n');

    navigator.clipboard.writeText(content);
    toast({
      title: 'Copied to clipboard',
      description: `All ${resources.length} resources copied as ${viewMode.toUpperCase()}`,
    });
  };

  const title = props.mode === 'preview' ? 'Manifest Preview' : 'Manifest';
  const isCollapsible = props.mode === 'preview';

  // Error state for deployed mode
  if (props.mode === 'deployed' && error) {
    return (
      <div className="glass-panel">
        <h2 className="text-lg font-heading mb-4">{title}</h2>
        <p className="text-sm text-destructive">
          Failed to load manifest: {error}
        </p>
      </div>
    );
  }

  return (
    <div className="glass-panel">
      <div
        className={isCollapsible ? "cursor-pointer select-none mb-4" : "mb-4"}
        onClick={isCollapsible ? () => setIsExpanded(!isExpanded) : undefined}
      >
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-2">
            <h2 className="text-lg font-heading flex items-center gap-2">
              {title}
              {resources.length > 0 && (
                <Badge variant="outline" className="ml-1">
                  {resources.length}
                </Badge>
              )}
            </h2>
          </div>

          {isCollapsible && (
            <ChevronDown className={`h-5 w-5 text-muted-foreground transition-transform ${isExpanded ? 'rotate-180' : ''}`} />
          )}
        </div>
      </div>

      {(isExpanded || !isCollapsible) && (
        <div>
          {isLoading ? (
            <div className="flex items-center justify-center py-8">
              <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
              <span className="ml-2 text-muted-foreground">
                {props.mode === 'preview' ? 'Generating preview...' : 'Loading manifests...'}
              </span>
            </div>
          ) : error ? (
            <div className="py-4">
              <p className="text-sm text-destructive">{error}</p>
              <p className="text-xs text-muted-foreground mt-1">
                {props.mode === 'preview'
                  ? 'Complete the configuration to see the manifest preview.'
                  : 'Unable to load the manifest for this deployment.'
                }
              </p>
            </div>
          ) : resources.length > 0 && selectedResource ? (
            <>
              {/* Resource selector tabs when multiple resources */}
              {resources.length > 1 && (
                <div className="flex flex-wrap gap-2 mb-4 pb-3 border-b">
                  {resources.map((resource, index) => (
                    <Button
                      key={`${resource.kind}-${resource.name}`}
                      type="button"
                      variant={selectedResourceIndex === index ? 'default' : 'outline'}
                      size="sm"
                      className="h-8"
                      onClick={(e) => { e.stopPropagation(); setSelectedResourceIndex(index); }}
                    >
                      <Badge
                        variant="secondary"
                        className={`mr-2 ${getResourceBadgeStyle(resource.kind)}`}
                      >
                        {resource.kind}
                      </Badge>
                      <span className="text-xs font-mono">{resource.name}</span>
                    </Button>
                  ))}
                </div>
              )}

              <div className="flex items-center justify-between mb-3">
                <span className="text-xs text-muted-foreground">
                  {selectedResource.apiVersion} • {selectedResource.kind}/{selectedResource.name}
                </span>
                <div className="flex gap-2">
                  {resources.length > 1 && (
                    <Button type="button" variant="outline" size="sm" onClick={(e) => { e.stopPropagation(); handleCopyAll(); }}>
                      <Copy className="h-4 w-4 mr-1" />
                      Copy All
                    </Button>
                  )}
                  <Button type="button" variant="outline" size="sm" onClick={(e) => { e.stopPropagation(); handleCopy(); }}>
                    <Copy className="h-4 w-4 mr-1" />
                    Copy
                  </Button>
                </div>
              </div>

              <Tabs value={viewMode} onValueChange={(v) => setViewMode(v as 'yaml' | 'json' | 'tree')}>
                <TabsList>
                  <TabsTrigger value="yaml">
                    <FileJson className="h-4 w-4 mr-1" />
                    YAML
                  </TabsTrigger>
                  <TabsTrigger value="json">
                    <Code className="h-4 w-4 mr-1" />
                    JSON
                  </TabsTrigger>
                  <TabsTrigger value="tree">
                    <ChevronRight className="h-4 w-4 mr-1" />
                    Tree
                  </TabsTrigger>
                </TabsList>

                <TabsContent value="yaml">
                  <div className="overflow-auto max-h-[500px] rounded-lg text-xs">
                    <SyntaxHighlighter
                      language="yaml"
                      style={oneDark}
                      customStyle={{ margin: 0, borderRadius: '0.5rem', fontSize: '0.75rem' }}
                    >
                      {YAML.stringify(selectedResource.manifest, { indent: 2 })}
                    </SyntaxHighlighter>
                  </div>
                </TabsContent>

                <TabsContent value="json">
                  <div className="overflow-auto max-h-[500px] rounded-lg text-xs">
                    <SyntaxHighlighter
                      language="json"
                      style={oneDark}
                      customStyle={{ margin: 0, borderRadius: '0.5rem', fontSize: '0.75rem' }}
                    >
                      {JSON.stringify(selectedResource.manifest, null, 2)}
                    </SyntaxHighlighter>
                  </div>
                </TabsContent>

                <TabsContent value="tree">
                  <div className="overflow-auto max-h-[500px] rounded-lg bg-[#0A0A0A] p-4 text-sm font-mono-code">
                    {Object.entries(selectedResource.manifest).map(([key, value]) => (
                      <TreeNode
                        key={key}
                        name={key}
                        value={value}
                        defaultExpanded={key === 'metadata' || key === 'spec' || key === 'status'}
                      />
                    ))}
                  </div>
                </TabsContent>
              </Tabs>
            </>
          ) : (
            <p className="text-sm text-muted-foreground">No manifest data available.</p>
          )}
        </div>
      )}
    </div>
  );
}

// Backwards-compatible exports
export const ManifestPreview = (props: { config: DeploymentConfig; provider: string }) => (
  <ManifestViewer mode="preview" config={props.config} provider={props.provider} />
);

export const CustomResourceViewer = (props: { deploymentName: string; namespace: string; provider: string }) => (
  <ManifestViewer mode="deployed" deploymentName={props.deploymentName} namespace={props.namespace} provider={props.provider} />
);
