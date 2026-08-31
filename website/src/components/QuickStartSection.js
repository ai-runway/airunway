import React from 'react';
import CodeBlock from '@theme/CodeBlock';

export default function QuickStartSection() {
  return (
    <section className="landing-section quickstart-section">
      <h2 className="section-title">Quick Start</h2>
      <p className="section-subtitle">
        Pick the flavor that matches how you work — both take less than a minute.
      </p>
      <div className="quickstart-grid">
        <div className="quickstart-card">
          <h3>Run locally</h3>
          <p>
            Download the latest release and launch the dashboard against your
            current kubeconfig.
          </p>
          <CodeBlock language="bash">{`./airunway
# open http://localhost:3001`}</CodeBlock>
        </div>
        <div className="quickstart-card">
          <h3>Deploy to Kubernetes</h3>
          <p>
            Apply the controller and optional dashboard manifests directly into
            your cluster.
          </p>
          <CodeBlock language="bash">{`kubectl apply -f https://raw.githubusercontent.com/ai-runway/airunway/main/deploy/controller.yaml
kubectl apply -f https://raw.githubusercontent.com/ai-runway/airunway/main/deploy/dashboard.yaml
kubectl port-forward -n airunway-system svc/airunway 3001:80`}</CodeBlock>
        </div>
      </div>
    </section>
  );
}
