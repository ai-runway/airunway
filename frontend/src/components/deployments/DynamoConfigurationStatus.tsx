import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import type { DeploymentStatus, DynamoIntent, DynamoReconfigureRequest } from '@airunway/shared'
import { deploymentsApi } from '@/lib/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { DynamoIntentFields } from './DynamoIntentFields'

type Draft = { action: 'retry' | 'reconfigure'; intent: DynamoIntent; modelId: string; engine: DynamoReconfigureRequest['engine']; resourceVersion: string }

export function DynamoConfigurationStatus({ deployment }: { deployment: DeploymentStatus }) {
  const client = useQueryClient()
  const [draft, setDraft] = useState<Draft | null>(null)
  const mutation = useMutation({
    retry: false,
    throwOnError: false,
    mutationFn: (payload: DynamoReconfigureRequest) => deploymentsApi.reconfigure(deployment.name, deployment.namespace, payload),
    onSuccess: () => {
      setDraft(null)
      void client.invalidateQueries({ queryKey: ['deployment', deployment.name, deployment.namespace] })
      void client.invalidateQueries({ queryKey: ['deployments'] })
    },
  })
  if (deployment.configurationMode !== 'automatic') return null
  const progress = deployment.providerStatus?.intent
  const failed = progress?.phase === 'Failed' || deployment.phase === 'Failed'
  const start = (action: Draft['action']) => {
    if (!deployment.intent || !deployment.resourceVersion) return
    mutation.reset()
    setDraft({ action, intent: structuredClone(deployment.intent), resourceVersion: deployment.resourceVersion,
      modelId: deployment.modelId, engine: deployment.engine as Draft['engine'] })
  }
  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    if (!draft) return
    mutation.mutate({ resourceVersion: draft.resourceVersion,
      ...(draft.action === 'reconfigure' ? { intent: draft.intent, modelId: draft.modelId, engine: draft.engine } : {}) })
  }
  return <section className="glass-panel space-y-4" aria-label="Automatic configuration status">
    <h2 className="text-lg font-heading">Automatic configuration</h2>
    <dl className="grid gap-4 sm:grid-cols-2 text-sm">
      <div><dt className="text-muted-foreground">Configuration progress</dt><dd>{progress?.phase || 'Waiting to start'}</dd></div>
      {progress?.profilingPhase && <div><dt className="text-muted-foreground">Profiling step</dt><dd>{progress.profilingPhase}</dd></div>}
      {deployment.intent && <div><dt className="text-muted-foreground">Requested GPU budget</dt><dd>{deployment.intent.hardware.totalGpus}</dd></div>}
      <div><dt className="text-muted-foreground">Serving copies</dt><dd>{deployment.replicas.ready} ready of {deployment.replicas.desired}</dd></div>
      {deployment.providerStatus?.workloadRef?.name && <div><dt className="text-muted-foreground">Serving workload</dt><dd className="break-all">{deployment.providerStatus.workloadRef.namespace || deployment.namespace}/{deployment.providerStatus.workloadRef.name}</dd></div>}
      <div><dt className="text-muted-foreground">Serving address</dt><dd className="break-all">{deployment.frontendService ? `${deployment.frontendService} (${deployment.frontendNamespace || deployment.namespace})` : 'Not available yet'}</dd></div>
    </dl>
    {deployment.message && <p role={failed ? 'alert' : undefined} className="text-sm">{deployment.message}</p>}
    <p className="text-sm text-muted-foreground">Performance targets guide configuration selection. A running model does not confirm that these targets were met.</p>
    <p className="text-sm text-muted-foreground">Settings are locked after profiling starts. Use Reconfigure to explicitly replace the configuration.</p>
    {deployment.intent && deployment.resourceVersion ? <div className="flex gap-2">
      {failed && <Button variant="outline" onClick={() => start('retry')}>Retry</Button>}
      <Button variant="outline" onClick={() => start('reconfigure')}>Reconfigure</Button>
    </div> : <p className="text-sm text-muted-foreground">This deployment does not yet expose the configuration and revision needed for safe reconfiguration.</p>}
    <Dialog open={!!draft} onOpenChange={open => { if (!open && !mutation.isPending) setDraft(null) }}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader><DialogTitle>{draft?.action === 'retry' ? 'Retry automatic configuration?' : 'Reconfigure this model?'}</DialogTitle>
          <DialogDescription>This starts a new configuration attempt. The current serving workload may be removed and requests may be interrupted. The operation can consume GPUs.</DialogDescription></DialogHeader>
        {draft && <form onSubmit={submit} className="space-y-4">
          {draft.action === 'reconfigure' && <fieldset disabled={mutation.isPending} className="space-y-4">
            <div className="space-y-2"><Label htmlFor="reconfigure-model">Model ID</Label><Input id="reconfigure-model" required maxLength={512} value={draft.modelId} onChange={e => setDraft({ ...draft, modelId: e.target.value })} /></div>
            <div className="space-y-2"><Label htmlFor="reconfigure-engine">Model server</Label><select id="reconfigure-engine" className="w-full rounded-md border bg-background p-2" value={draft.engine} onChange={e => setDraft({ ...draft, engine: e.target.value as Draft['engine'] })}>
              <option value="vllm">vLLM</option><option value="sglang">SGLang</option><option value="trtllm">TensorRT-LLM</option>
            </select></div>
            <DynamoIntentFields prefix="reconfigure" value={draft.intent} onChange={intent => setDraft({ ...draft, intent })} />
          </fieldset>}
          {mutation.error && <p role="alert" className="text-sm text-destructive">{mutation.error.message}</p>}
          <DialogFooter><Button type="button" variant="outline" disabled={mutation.isPending} onClick={() => setDraft(null)}>Cancel</Button>
            <Button type="submit" variant="destructive" disabled={mutation.isPending}>{mutation.isPending ? 'Submitting...' : draft.action === 'retry' ? 'Confirm retry' : 'Confirm reconfiguration'}</Button>
          </DialogFooter>
        </form>}
      </DialogContent>
    </Dialog>
  </section>
}
