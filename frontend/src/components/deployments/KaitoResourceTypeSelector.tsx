import { Badge } from '@/components/ui/badge'
import { Label } from '@/components/ui/label'
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group'
import type { KaitoResourceType } from '@/lib/api'
import { cn } from '@/lib/utils'

interface KaitoResourceTypeSelectorProps {
  value: KaitoResourceType
  onChange: (value: KaitoResourceType) => void
  idSuffix?: string
}

export function KaitoResourceTypeSelector({ value, onChange, idSuffix = '' }: KaitoResourceTypeSelectorProps) {
  const workspaceId = idSuffix ? `resource-workspace-${idSuffix}` : 'resource-workspace'
  const inferenceSetId = idSuffix ? `resource-inferenceset-${idSuffix}` : 'resource-inferenceset'

  return (
    <div className="space-y-3">
      <Label>Resource Type</Label>
      <RadioGroup
        value={value}
        onValueChange={(nextValue) => onChange(nextValue as KaitoResourceType)}
        className="grid gap-3"
      >
        <label
          htmlFor={workspaceId}
          className={cn(
            'flex items-start space-x-3 rounded-lg border p-3 cursor-pointer transition-colors',
            value === 'workspace'
              ? 'border-primary bg-primary/5'
              : 'border-border hover:border-muted-foreground/50'
          )}
        >
          <RadioGroupItem value="workspace" id={workspaceId} className="mt-1" />
          <div className="flex-1">
            <div className="flex items-center gap-2">
              <span className="font-medium">Workspace</span>
              <Badge variant="secondary" className="text-xs">Stable</Badge>
            </div>
            <p className="text-xs text-muted-foreground mt-1">
              Original KAITO resource type (v1beta1). Recommended for most deployments.
            </p>
          </div>
        </label>
        <label
          htmlFor={inferenceSetId}
          className={cn(
            'flex items-start space-x-3 rounded-lg border p-3 cursor-pointer transition-colors',
            value === 'inferenceset'
              ? 'border-primary bg-primary/5'
              : 'border-border hover:border-muted-foreground/50'
          )}
        >
          <RadioGroupItem value="inferenceset" id={inferenceSetId} className="mt-1" />
          <div className="flex-1">
            <span className="font-medium">InferenceSet</span>
            <p className="text-xs text-muted-foreground mt-1">
              Newer KAITO resource type (v1alpha1). Supports flexible replica scaling.
            </p>
          </div>
        </label>
      </RadioGroup>
    </div>
  )
}
