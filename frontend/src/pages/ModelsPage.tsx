import { useState, useMemo } from 'react'
import { useModels } from '@/hooks/useModels'
import { useGpuCapacity } from '@/hooks/useGpuOperator'
import { useDetailedCapacity } from '@/hooks/useAutoscaler'
import { ModelGrid } from '@/components/models/ModelGrid'
import { ModelSearch } from '@/components/models/ModelSearch'
import { HfModelSearch } from '@/components/models/HfModelSearch'
import { SkeletonGrid } from '@/components/ui/skeleton'
import { BookMarked, Search, Sparkles } from 'lucide-react'
import { cn } from '@/lib/utils'
import type { Engine } from '@airunway/shared'
import { getGpuFitCapacityDisplay } from '@/lib/gpu-fit-capacity'
import { pickGpuModel } from '@/lib/gpu-throughput-params'

type Tab = 'curated' | 'huggingface'

export function ModelsPage() {
  const { data: models, isLoading, error } = useModels()
  const { data: gpuCapacity } = useGpuCapacity()
  const { data: detailedCapacity } = useDetailedCapacity()
  const [search, setSearch] = useState('')
  const [selectedEngines, setSelectedEngines] = useState<Engine[]>([])
  const [activeTab, setActiveTab] = useState<Tab>('curated')
  const gpuFitCapacity = getGpuFitCapacityDisplay(gpuCapacity)
  const gpuModel = pickGpuModel(detailedCapacity)

  const filteredModels = useMemo(() => {
    if (!models) return []

    return models.filter((model) => {
      // Filter by search
      const searchMatch = search === '' ||
        model.name.toLowerCase().includes(search.toLowerCase()) ||
        model.id.toLowerCase().includes(search.toLowerCase()) ||
        model.description.toLowerCase().includes(search.toLowerCase())

      // Filter by engine
      const engineMatch = selectedEngines.length === 0 ||
        selectedEngines.some((engine) => model.supportedEngines.includes(engine))

      return searchMatch && engineMatch
    })
  }, [models, search, selectedEngines])

  const handleEngineToggle = (engine: Engine) => {
    setSelectedEngines((prev) =>
      prev.includes(engine)
        ? prev.filter((e) => e !== engine)
        : [...prev, engine]
    )
  }

  if (isLoading && activeTab === 'curated') {
    return (
      <div className="space-y-6">
        <div className="text-center py-8">
          <h1 className="font-heading text-4xl flex items-center justify-center gap-3">
            Model Catalog
            <Sparkles className="h-7 w-7 text-cyan-400" />
          </h1>
          <p className="text-slate-400 mt-2">
            Browse curated models or search HuggingFace Hub
          </p>
        </div>
        <SkeletonGrid count={8} className="lg:grid-cols-4" />
      </div>
    )
  }

  if (error && activeTab === 'curated') {
    return (
      <div className="flex flex-col items-center justify-center py-12 text-center">
        <p className="text-lg font-medium text-destructive">
          Failed to load models
        </p>
        <p className="text-sm text-muted-foreground mt-1">
          {error instanceof Error ? error.message : 'Unknown error'}
        </p>
      </div>
    )
  }

  return (
    <div className="space-y-6 animate-slide-up">
      {/* Hero section */}
      <div className="text-center py-8">
        <h1 className="font-heading text-4xl flex items-center justify-center gap-3">
          Model Catalog
          <Sparkles className="h-7 w-7 text-cyan-400" />
        </h1>
        <p className="text-slate-400 mt-2">
          Browse curated models or search HuggingFace Hub
        </p>
        {models && (
          <p className="text-xs text-slate-500 mt-1 tabular-nums">
            {filteredModels.length} of {models.length} models
          </p>
        )}
      </div>

      {/* Tab navigation — underline variant */}
      <div className="flex gap-6 border-b border-white/5">
        <button
          onClick={() => setActiveTab('curated')}
          className={cn(
            'flex items-center gap-2 px-1 py-2.5 text-sm font-medium transition-all duration-200 border-b-2 -mb-px',
            activeTab === 'curated'
              ? 'border-cyan-400 text-white'
              : 'border-transparent text-slate-500 hover:text-slate-300'
          )}
        >
          <BookMarked className={cn(
            "h-4 w-4 transition-transform duration-200",
            activeTab === 'curated' && "scale-110"
          )} />
          Curated Models
        </button>
        <button
          onClick={() => setActiveTab('huggingface')}
          className={cn(
            'flex items-center gap-2 px-1 py-2.5 text-sm font-medium transition-all duration-200 border-b-2 -mb-px',
            activeTab === 'huggingface'
              ? 'border-cyan-400 text-white'
              : 'border-transparent text-slate-500 hover:text-slate-300'
          )}
        >
          <Search className={cn(
            "h-4 w-4 transition-transform duration-200",
            activeTab === 'huggingface' && "scale-110"
          )} />
          HuggingFace Hub
        </button>
      </div>

      {/* Curated models tab */}
      {activeTab === 'curated' && (
        <>
          <ModelSearch
            search={search}
            onSearchChange={setSearch}
            selectedEngines={selectedEngines}
            onEngineToggle={handleEngineToggle}
          />
          <ModelGrid
            models={filteredModels}
            gpuCapacityGb={gpuCapacity?.totalMemoryGb}
            gpuCount={gpuFitCapacity.gpuCount}
            gpuCapacityLabel={gpuFitCapacity.capacityLabel}
            gpuModel={gpuModel}
          />
        </>
      )}

      {/* HuggingFace search tab */}
      {activeTab === 'huggingface' && (
        <HfModelSearch
          gpuCapacityGb={gpuCapacity?.totalMemoryGb}
          gpuCount={gpuFitCapacity.gpuCount}
          gpuCapacityLabel={gpuFitCapacity.capacityLabel}
          gpuModel={gpuModel}
        />
      )}
    </div>
  )
}
