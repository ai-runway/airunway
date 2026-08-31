import React from 'react';
import Link from '@docusaurus/Link';
import {ctaLinks} from '../data/landingPageData';

export default function CtaSection() {
  return (
    <section id="community" className="landing-section cta-section">
      <h2 className="section-title">Join the community</h2>
      <p className="section-subtitle">
        Chat with us in <strong>#airunway</strong> on CNCF Slack, report an
        issue, or help improve the project.
      </p>
      <div className="cta-buttons">
        {ctaLinks.map((link) => (
          <Link
            key={link.label}
            to={link.href}
            className="button button--outline button--lg"
          >
            {link.label}
          </Link>
        ))}
      </div>
    </section>
  );
}
