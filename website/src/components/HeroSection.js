import React from 'react';
import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';

export default function HeroSection() {
  return (
    <section className="hero-section">
      <img
        className="hero-logo"
        alt="AI Runway logo"
        src={useBaseUrl('/img/logo.png')}
      />
      <h1 className="hero-title">AI Runway</h1>
      <p className="hero-tagline">
        Deploy and manage large language models on Kubernetes —{' '}
        <span className="hero-highlight">no YAML required.</span>
      </p>
      <p className="hero-description">
        A web UI and unified <code>ModelDeployment</code> CRD on top of the
        leading Kubernetes inference providers. Browse HuggingFace, pick a
        model, click deploy.
      </p>
      <div className="hero-buttons">
        <Link to="/development" className="button button--primary button--lg">
          Get Started
        </Link>
        <Link
          to="https://github.com/ai-runway/airunway/releases"
          className="button button--secondary button--lg"
        >
          Download
        </Link>
      </div>
    </section>
  );
}
