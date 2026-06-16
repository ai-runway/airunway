import { Box, Cpu, Loader2, Server } from 'lucide-react'

import { Badge } from '@/components/ui/badge'
import { Label } from '@/components/ui/label'
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import type { KaitoResourceType } from '@/lib/api'
import { cn } from '@/lib/utils'

import type { GgufRunMode, KaitoComputeType } from './deploymentFormModel'
import { KaitoResourceTypeSelector } from './KaitoResourceTypeSelector'

interface KaitoModelConfigurationProps {
  computeType: KaitoComputeType
  onComputeTypeChange: (value: KaitoComputeType) => void
  resourceType: KaitoResourceType
  onResourceTypeChange: (value: KaitoResourceType) => void
  isHuggingFaceGgufModel: boolean
  ggufRunMode: GgufRunMode
  onGgufRunModeChange: (value: GgufRunMode) => void
  ggufFilesLoading: boolean
  ggufFiles: string[]
  ggufFile: string
  onGgufFileChange: (value: string) => void
}

export function KaitoModelConfiguration({
  computeType,
  onComputeTypeChange,
  resourceType,
  onResourceTypeChange,
  isHuggingFaceGgufModel,
  ggufRunMode,
  onGgufRunModeChange,
  ggufFilesLoading,
  ggufFiles,
  ggufFile,
  onGgufFileChange,
}: KaitoModelConfigurationProps) {
  return (
    <div className="glass-panel">
      <h3 className="text-lg font-semibold flex items-center gap-2 mb-4">
        <Box className="h-5 w-5" />
        KAITO Model Configuration
      </h3>
      <div className="space-y-6">
        <div className="space-y-3">
          <Label>Compute Type</Label>
          <RadioGroup
            value={computeType}
            onValueChange={(value) => onComputeTypeChange(value as KaitoComputeType)}
            className="flex gap-4"
          >
            <div className="flex items-center space-x-2">
              <RadioGroupItem value="cpu" id="compute-cpu" />
              <Label htmlFor="compute-cpu" className="cursor-pointer flex items-center gap-1">
                <Cpu className="h-4 w-4" />
                CPU
              </Label>
            </div>
            <div className="flex items-center space-x-2">
              <RadioGroupItem value="gpu" id="compute-gpu" />
              <Label htmlFor="compute-gpu" className="cursor-pointer flex items-center gap-1">
                <Server className="h-4 w-4" />
                GPU
              </Label>
            </div>
          </RadioGroup>
          <p className="text-xs text-muted-foreground">
            {computeType === 'cpu'
              ? 'Run inference on CPU compute - slower but no GPU required'
              : 'Run inference on GPU compute - faster performance'}
          </p>
        </div>

        <KaitoResourceTypeSelector
          value={resourceType}
          onChange={onResourceTypeChange}
        />

        {isHuggingFaceGgufModel && (
          <div className="space-y-3">
            <Label>Run Mode</Label>
            <RadioGroup
              value={ggufRunMode}
              onValueChange={(value) => onGgufRunModeChange(value as GgufRunMode)}
              className="grid gap-3"
            >
              <label
                htmlFor="run-direct"
                className={cn(
                  'flex items-start space-x-3 rounded-lg border p-3 cursor-pointer transition-colors',
                  ggufRunMode === 'direct'
                    ? 'border-primary bg-primary/5'
                    : 'border-border hover:border-muted-foreground/50'
                )}
              >
                <RadioGroupItem value="direct" id="run-direct" className="mt-1" />
                <div className="flex-1">
                  <div className="flex items-center gap-2">
                    <span className="font-medium">Direct Run</span>
                    <Badge variant="secondary" className="text-xs">Recommended</Badge>
                  </div>
                  <p className="text-xs text-muted-foreground mt-1">
                    Downloads model at runtime. No Docker required.
                  </p>
                </div>
              </label>
              <label
                htmlFor="run-build"
                className={cn(
                  'flex items-start space-x-3 rounded-lg border p-3 cursor-pointer transition-colors',
                  ggufRunMode === 'build'
                    ? 'border-primary bg-primary/5'
                    : 'border-border hover:border-muted-foreground/50'
                )}
              >
                <RadioGroupItem value="build" id="run-build" className="mt-1" />
                <div className="flex-1">
                  <span className="font-medium">Build Image</span>
                  <p className="text-xs text-muted-foreground mt-1">
                    Pre-builds container image. Requires Docker running locally.
                  </p>
                </div>
              </label>
            </RadioGroup>
          </div>
        )}

        {isHuggingFaceGgufModel && (
          <div className="space-y-3">
            <Label htmlFor="ggufFile">GGUF File</Label>
            {ggufFilesLoading ? (
              <div className="flex items-center gap-2 text-sm text-muted-foreground py-2">
                <Loader2 className="h-4 w-4 animate-spin" />
                Loading GGUF files from repository...
              </div>
            ) : ggufFiles.length > 0 ? (
              <Select value={ggufFile} onValueChange={onGgufFileChange}>
                <SelectTrigger>
                  <SelectValue placeholder="Select a GGUF file" />
                </SelectTrigger>
                <SelectContent>
                  {ggufFiles.map((file) => (
                    <SelectItem key={file} value={file}>
                      {file}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            ) : (
              <div className="text-sm text-muted-foreground py-2">
                No GGUF files found in this repository.
              </div>
            )}
            <p className="text-xs text-muted-foreground">
              Select the quantization variant to use. Q4_K_M offers a good balance of quality and size.
            </p>
          </div>
        )}
      </div>
    </div>
  )
}
