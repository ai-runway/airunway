import { useState } from 'react';
import { Input } from '@/components/ui/input';
import { Button } from '@/components/ui/button';
import { HfModelCard } from './HfModelCard';
import { useHfModelSearch, useHuggingFaceStatus } from '@/hooks/useHuggingFace';
import { useDebounce } from '@/hooks/useDebounce';
import { useGpuCapacity } from '@/hooks/useGpuOperator';
import { Search, Loader2, AlertCircle, LogIn } from 'lucide-react';
import { Alert, AlertDescription } from '@/components/ui/alert';
import { getGpuFitCapacityDisplay } from '@/lib/gpu-fit-capacity';

interface HfModelSearchProps {
  onLoginClick?: () => void;
  gpuCapacityGb?: number;
  gpuCount?: number;
  gpuCapacityLabel?: string;
  gpuModel?: string;
}

export function HfModelSearch({ onLoginClick, gpuCapacityGb, gpuCount, gpuCapacityLabel, gpuModel }: HfModelSearchProps) {
  const [query, setQuery] = useState('');
  const [offset, setOffset] = useState(0);
  const limit = 20;

  const debouncedQuery = useDebounce(query, 500);
  
  const { data: searchResults, isLoading, error, isFetching } = useHfModelSearch(
    debouncedQuery,
    { limit, offset }
  );

  const { data: hfStatus, isLoading: hfStatusLoading } = useHuggingFaceStatus();
  const { data: gpuCapacity } = useGpuCapacity();
  const defaultFitCapacity = getGpuFitCapacityDisplay(gpuCapacity);

  // Use provided GPU capacity or estimate from cluster
  const maxGpuMemoryGb = gpuCapacityGb ?? gpuCapacity?.totalMemoryGb;
  const effectiveGpuCount = gpuCount ?? defaultFitCapacity.gpuCount;
  const effectiveCapacityLabel = gpuCapacityLabel ?? defaultFitCapacity.capacityLabel;

  const handleSearch = (e: React.ChangeEvent<HTMLInputElement>) => {
    setQuery(e.target.value);
    setOffset(0); // Reset pagination on new search
  };

  const handleLoadMore = () => {
    setOffset((prev) => prev + limit);
  };

  const isLoggedIn = hfStatus?.configured;
  const showLoginPrompt = !hfStatusLoading && !isLoggedIn && query.length >= 2;

  return (
    <div className="space-y-4">
      {/* Search input */}
      <div className="relative">
        <Search className="absolute left-4 top-1/2 h-5 w-5 -translate-y-1/2 text-slate-400" />
        <Input
          type="text"
          placeholder="Search HuggingFace models (e.g., llama, mistral, qwen)..."
          value={query}
          onChange={handleSearch}
          className="pl-12 h-12 rounded-2xl bg-white/[0.03] border-white/5 text-base placeholder:text-slate-500"
        />
        {isFetching && (
          <Loader2 className="absolute right-3 top-1/2 h-4 w-4 -translate-y-1/2 animate-spin text-muted-foreground" />
        )}
      </div>

      {/* Login prompt for gated models */}
      {showLoginPrompt && (
        <Alert>
          <LogIn className="h-4 w-4" />
          <AlertDescription className="flex items-center justify-between">
            <span>Log in with HuggingFace to access gated models like Llama and Mistral.</span>
            {onLoginClick && (
              <Button variant="outline" size="sm" onClick={onLoginClick} className="ml-2">
                Login
              </Button>
            )}
          </AlertDescription>
        </Alert>
      )}

      {/* Error state */}
      {error && (
        <Alert variant="destructive">
          <AlertCircle className="h-4 w-4" />
          <AlertDescription>
            {error instanceof Error ? error.message : 'Failed to search models'}
          </AlertDescription>
        </Alert>
      )}

      {/* Empty state */}
      {query.length < 2 && (
        <div className="text-center py-16 text-slate-400">
          <Search className="h-12 w-12 mx-auto mb-4 text-slate-600" />
          <p className="text-slate-300">Enter at least 2 characters to search HuggingFace models</p>
          <p className="text-sm mt-2 text-slate-500">
            Only models compatible with vLLM, SGLang, or TensorRT-LLM will be shown
          </p>
        </div>
      )}

      {/* Loading state for initial search */}
      {isLoading && query.length >= 2 && (
        <div className="text-center py-16">
          <Loader2 className="h-8 w-8 mx-auto animate-spin text-cyan-400" />
          <p className="mt-4 text-slate-400">Searching compatible models...</p>
        </div>
      )}

      {/* No results */}
      {searchResults && searchResults.models.length === 0 && !isLoading && (
        <div className="text-center py-16 text-slate-400">
          <AlertCircle className="h-12 w-12 mx-auto mb-4 text-slate-600" />
          <p className="text-slate-300">No compatible models found for &ldquo;{debouncedQuery}&rdquo;</p>
          <p className="text-sm mt-2 text-slate-500">
            Try a different search term or check if the model architecture is supported
          </p>
        </div>
      )}

      {/* Results grid */}
      {searchResults && searchResults.models.length > 0 && (
        <>
          <div className="text-sm text-muted-foreground">
            Showing {searchResults.models.length} of {searchResults.total} compatible models
          </div>
          
          <div className="grid grid-cols-2 md:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4 gap-4">
            {searchResults.models.map((model) => (
              <HfModelCard
                key={model.id}
                model={model}
                gpuCapacityGb={maxGpuMemoryGb}
                gpuCount={effectiveGpuCount}
                gpuCapacityLabel={effectiveCapacityLabel}
                gpuModel={gpuModel}
              />
            ))}
          </div>

          {/* Load more button */}
          {searchResults.hasMore && (
            <div className="flex justify-center pt-4">
              <Button 
                variant="outline" 
                onClick={handleLoadMore}
                disabled={isFetching}
              >
                {isFetching ? (
                  <>
                    <Loader2 className="h-4 w-4 mr-2 animate-spin" />
                    Loading...
                  </>
                ) : (
                  'Load More'
                )}
              </Button>
            </div>
          )}
        </>
      )}
    </div>
  );
}
